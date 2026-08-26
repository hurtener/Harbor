package durable_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// seeder exposes the two driver-internal helpers the conformance suite
// uses; the durable driver inherits them from the engine.
type seeder interface {
	AddMemberToGroup(ctx context.Context, gid tasks.TaskGroupID, tid tasks.TaskID) error
	CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
}

// TestDurable_RestartSurvival_TasksGroupsPatches spawns tasks, a group
// (with a member), and a patch; closes the driver; reopens a fresh
// driver over the SAME store; and asserts every record survived with
// its fields intact. No task is left Running, so the recovery sweep is
// a no-op here (recovery is exercised separately).
func TestDurable_RestartSurvival_TasksGroupsPatches(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := quadA()
	ctx := ctxFor(t, id.Identity)

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)

	// A completed task (with a result) + a pending task.
	done, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "done-task"})
	if err != nil {
		t.Fatalf("Spawn done: %v", err)
	}
	if err := r1.MarkRunning(ctx, done.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := r1.MarkComplete(ctx, done.ID, tasks.TaskResult{Value: []byte(`{"answer":42}`)}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	sealer, err := toolauth.NewAESGCMSealer(make([]byte, toolauth.KEKSizeBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := tasks.NewAgentReachAdmissionAuthority(sealer)
	if err != nil {
		t.Fatal(err)
	}
	admittedCtx, err := authority.Admit(ctx, id.Identity, "agent-restart")
	if err != nil {
		t.Fatal(err)
	}
	route := &llm.ProviderRoute{
		RouteID: "route-restart", RouteGeneration: 4,
		ProviderConnectionID: "connection-restart", ProviderConnectionGeneration: 3,
		CredentialAssetGeneration: 2, ModelSelector: "balanced",
	}
	pending, err := r1.Spawn(admittedCtx, tasks.SpawnRequest{
		Identity: id, Kind: tasks.KindForeground, Description: "pending-task",
		ProviderRoute: route,
	})
	if err != nil {
		t.Fatalf("Spawn pending: %v", err)
	}

	// A group with a member.
	grp, err := r1.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: id.Identity, Description: "grp"})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	member, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "member"})
	if err != nil {
		t.Fatalf("Spawn member: %v", err)
	}
	sd, ok := r1.(seeder)
	if !ok {
		t.Fatal("durable driver does not expose seeder helpers")
	}
	if err := sd.AddMemberToGroup(ctx, grp.ID, member.ID); err != nil {
		t.Fatalf("AddMemberToGroup: %v", err)
	}
	// A pending patch.
	if _, err := sd.CreatePendingPatch(ctx, id.Identity, "patch-1", []byte(`{"op":"x"}`)); err != nil {
		t.Fatalf("CreatePendingPatch: %v", err)
	}

	// Simulate a restart: close the registry + bus, keep the store.
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	bus2 := mkBus(t)
	defer func() { _ = bus2.Close(context.Background()) }()
	r2 := openOver(t, store, bus2)
	defer func() { _ = r2.Close(context.Background()) }()

	// Tasks survived with their statuses + result.
	gotDone, err := r2.Get(ctx, done.ID)
	if err != nil {
		t.Fatalf("Get(done) after restart: %v", err)
	}
	if gotDone.Status != tasks.StatusComplete {
		t.Errorf("done task status = %q, want Complete", gotDone.Status)
	}
	if gotDone.Result == nil || string(gotDone.Result.Value) != `{"answer":42}` {
		t.Errorf("done task result = %+v, want {\"answer\":42}", gotDone.Result)
	}
	gotPending, err := r2.Get(ctx, pending.ID)
	if err != nil {
		t.Fatalf("Get(pending) after restart: %v", err)
	}
	if gotPending.Status != tasks.StatusPending {
		t.Errorf("pending task status = %q, want Pending", gotPending.Status)
	}
	if _, gotAgent, admitted := authority.Restore(context.Background(), gotPending); !admitted || gotAgent != "agent-restart" {
		t.Errorf("pending task admission after restart = (%q, %v), want (agent-restart, true)", gotAgent, admitted)
	}
	if gotPending.ProviderRoute == nil || *gotPending.ProviderRoute != *route {
		t.Errorf("pending task provider route after restart = %+v, want %+v", gotPending.ProviderRoute, route)
	}

	// List returns all three tasks.
	summaries, err := r2.List(ctx, id.Identity, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List after restart: %v", err)
	}
	if len(summaries) != 3 {
		t.Errorf("List returned %d tasks, want 3", len(summaries))
	}

	// Group survived with its member.
	groups, err := r2.ListGroups(ctx, id.Identity, nil)
	if err != nil {
		t.Fatalf("ListGroups after restart: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("ListGroups returned %d, want 1", len(groups))
	}
	if len(groups[0].Members) != 1 || groups[0].Members[0] != member.ID {
		t.Errorf("group members = %v, want [%s]", groups[0].Members, member.ID)
	}

	// Patch survived: applying it succeeds (proves it hydrated).
	applied, err := r2.ApplyPatch(ctx, id.Identity, "patch-1", tasks.PatchAccept)
	if err != nil {
		t.Fatalf("ApplyPatch after restart: %v", err)
	}
	if !applied {
		t.Error("ApplyPatch after restart returned applied=false, want true (patch should have survived as pending)")
	}
}

// TestDurable_RestartSurvival_IdentityIsolation asserts cross-identity
// reads stay rejected after a restart — durability does not widen the
// isolation boundary.
func TestDurable_RestartSurvival_IdentityIsolation(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := quadA()
	ctx := ctxFor(t, id.Identity)

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	h, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "secret"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	bus2 := mkBus(t)
	defer func() { _ = bus2.Close(context.Background()) }()
	r2 := openOver(t, store, bus2)
	defer func() { _ = r2.Close(context.Background()) }()

	otherCtx := ctxFor(t, identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"})
	if _, err := r2.Get(otherCtx, h.ID); err == nil {
		t.Error("cross-identity Get after restart: want ErrNotFound, got nil")
	}
	// The owning identity still sees it.
	if _, err := r2.Get(ctx, h.ID); err != nil {
		t.Errorf("owner Get after restart: %v", err)
	}
}

// TestDurable_RestartSurvival_IdempotencyPreserved asserts a re-spawn
// with the same key + content after a restart returns the SAME handle
// (Reused) rather than creating a duplicate — proving the content hash
// survived hydration.
func TestDurable_RestartSurvival_IdempotencyPreserved(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := quadA()
	ctx := ctxFor(t, id.Identity)
	req := tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "idem", Query: "q", IdempotencyKey: "key-1"}

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	first, err := r1.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn first: %v", err)
	}
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	bus2 := mkBus(t)
	defer func() { _ = bus2.Close(context.Background()) }()
	r2 := openOver(t, store, bus2)
	defer func() { _ = r2.Close(context.Background()) }()

	again, err := r2.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("Spawn again after restart: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("re-spawn after restart: got new ID %s, want reused %s", again.ID, first.ID)
	}
	if !again.Reused {
		t.Error("re-spawn after restart: Reused=false, want true")
	}

	// A divergent re-spawn under the same key still conflicts.
	divergent := req
	divergent.Description = "different"
	if _, err := r2.Spawn(ctx, divergent); err == nil {
		t.Error("divergent re-spawn after restart: want ErrIdempotencyConflict, got nil")
	}
}

// TestDurable_RestartSurvival_CrossTenantIdempotencyKeyStaysIsolated
// asserts the tenant separation of the idempotency index SURVIVES a
// restart — and, by construction, that no store migration is needed to
// get it.
//
// The index is derived state: it is never written to the StateStore
// (task records are keyed by the task's own ULID under the identity
// triple's scope), and the engine rebuilds it at Hydrate from each
// persisted task's own Identity + IdempotencyKey. So a change to the
// index key's SHAPE has no on-disk footprint: the records this test
// writes through instance 1 are byte-identical to what the pre-fix
// code wrote, and instance 2 rebuilds the corrected key from them.
func TestDurable_RestartSurvival_CrossTenantIdempotencyKeyStaysIsolated(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()

	// A COLLIDING (session, idempotency-key) pair across two tenants.
	idA := identity.Identity{TenantID: "tenant-a", UserID: "user-x", SessionID: "sess-shared"}
	idB := identity.Identity{TenantID: "tenant-b", UserID: "user-x", SessionID: "sess-shared"}
	ctxA := ctxFor(t, idA)
	ctxB := ctxFor(t, idB)
	reqA := tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: idA}, Kind: tasks.KindBackground,
		Description: "idem", Query: "q", IdempotencyKey: "shared-key",
	}
	reqB := reqA
	reqB.Identity = identity.Quadruple{Identity: idB}

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	hA, err := r1.Spawn(ctxA, reqA)
	if err != nil {
		t.Fatalf("Spawn tenant-a: %v", err)
	}
	hB, err := r1.Spawn(ctxB, reqB)
	if err != nil {
		t.Fatalf("Spawn tenant-b under a colliding (session, key): %v", err)
	}
	if hA.ID == hB.ID {
		t.Fatalf("cross-tenant collision before restart: shared TaskID %s", hA.ID)
	}
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	// Reopen over the SAME store: hydration rebuilds the index.
	bus2 := mkBus(t)
	defer func() { _ = bus2.Close(context.Background()) }()
	r2 := openOver(t, store, bus2)
	defer func() { _ = r2.Close(context.Background()) }()

	// Each tenant's retry dedups to ITS OWN pre-restart task — proving
	// both index entries survived hydration under distinct keys rather
	// than one clobbering the other.
	againA, err := r2.Spawn(ctxA, reqA)
	if err != nil {
		t.Fatalf("tenant-a retry after restart: %v", err)
	}
	if againA.ID != hA.ID || !againA.Reused {
		t.Errorf("tenant-a retry after restart: id=%s reused=%v, want %s true", againA.ID, againA.Reused, hA.ID)
	}
	againB, err := r2.Spawn(ctxB, reqB)
	if err != nil {
		t.Fatalf("tenant-b retry after restart: %v", err)
	}
	if againB.ID != hB.ID || !againB.Reused {
		t.Errorf("tenant-b retry after restart: id=%s reused=%v, want %s true", againB.ID, againB.Reused, hB.ID)
	}
	if againA.ID == againB.ID {
		t.Errorf("hydration collapsed two tenants onto one task: %s", againA.ID)
	}
}

// TestDurable_RecoverySweep_RunningBecomesFailed asserts a task left
// Running by a crash is transitioned to Failed{runtime_restarted} on
// reopen, emitting exactly one task.failed event.
func TestDurable_RecoverySweep_RunningBecomesFailed(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := quadA()
	ctx := ctxFor(t, id.Identity)

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	h, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "crashed"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := r1.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	// Subscribe on the new bus BEFORE reopening so the recovery event
	// (published during driver open) is captured.
	bus2 := mkBus(t)
	defer func() { _ = bus2.Close(context.Background()) }()
	sub, err := bus2.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	r2 := openOver(t, store, bus2)
	defer func() { _ = r2.Close(context.Background()) }()

	got, err := r2.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.Status != tasks.StatusFailed {
		t.Errorf("recovered task status = %q, want Failed", got.Status)
	}
	if got.Error == nil || got.Error.Code != engine.RecoveryErrorCode {
		t.Errorf("recovered task error = %+v, want code %q", got.Error, engine.RecoveryErrorCode)
	}

	// Exactly one task.failed for this task on the bus.
	failedCount := 0
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == tasks.EventTypeTaskFailed {
				failedCount++
			}
		case <-deadline:
			if failedCount != 1 {
				t.Errorf("task.failed events = %d, want exactly 1", failedCount)
			}
			return
		}
	}
}

// TestDurable_RecoverySweep_IdempotentOnReopen asserts a second reopen
// after recovery does NOT re-sweep or re-emit — the recovered task is
// already persisted as Failed, so it is no longer a sweep candidate.
func TestDurable_RecoverySweep_IdempotentOnReopen(t *testing.T) {
	store := newStore(t)
	defer func() { _ = store.Close(context.Background()) }()
	id := quadA()
	ctx := ctxFor(t, id.Identity)

	bus1 := mkBus(t)
	r1 := openOver(t, store, bus1)
	h, err := r1.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "crashed"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := r1.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	_ = r1.Close(context.Background())
	_ = bus1.Close(context.Background())

	// First reopen: recovery runs.
	bus2 := mkBus(t)
	r2 := openOver(t, store, bus2)
	_ = r2.Close(context.Background())
	_ = bus2.Close(context.Background())

	// Second reopen: subscribe, then open. The task is already Failed,
	// so NO task.failed should fire.
	bus3 := mkBus(t)
	defer func() { _ = bus3.Close(context.Background()) }()
	sub, err := bus3.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	r3 := openOver(t, store, bus3)
	defer func() { _ = r3.Close(context.Background()) }()

	select {
	case ev := <-sub.Events():
		t.Errorf("unexpected event on second reopen (no re-sweep expected): %s", ev.Type)
	case <-time.After(300 * time.Millisecond):
		// No re-emit — correct.
	}

	got, err := r3.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get after second reopen: %v", err)
	}
	if got.Status != tasks.StatusFailed {
		t.Errorf("task status after second reopen = %q, want Failed (stable)", got.Status)
	}
}
