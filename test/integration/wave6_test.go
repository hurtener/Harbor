// Wave 6 cross-subsystem integration test per AGENTS.md §17.5.
//
// Wave 6 closed five surfaces:
//
//   - Phase 18: SQLite-blob + Postgres-blob ArtifactStore drivers.
//   - Phase 19: S3-style ArtifactStore driver (skip-clean without
//     HARBOR_TEST_S3_DSN; covered in CI's s3 conformance job).
//   - Phase 20: TaskRegistry interface + InProcess driver — per-task
//     lifecycle (Spawn / Mark* / Cancel / Get / List / Prioritize).
//   - Phase 21: TaskGroup + WatchGroup + retain-turn + patches +
//     ack-background — the group governance surface stacked on top
//     of Phase 20's per-task floor. Phase 21 ships `WatchGroup` as
//     the non-retain-turn wake mechanism (D-032); the planner-phase
//     authors (Phase 42+) will consume it.
//   - Phase 22: A2A v1 contracts (full surface) + loopback drivers
//     (MessageBus + RemoteTransport) — the cross-process / cross-host
//     edge. V1 loopback is the conformance reference; Phase 29 ships
//     the wire driver later.
//
// Each phase shipped its own conformance suite + per-package tests.
// This wave-end smoke proves the new surfaces COMPOSE with the
// already-shipped subsystems (artifacts / events / state / audit)
// and that identity propagates through every layer.
//
// Four tests:
//
//   - TestE2E_Wave6_TaskGroup_WatchGroup_ResolvePath: group of 3
//     members; one member produces a heavy result substituted with
//     an ArtifactRef (proves the D-022/D-026 ref-shaped discipline
//     end-to-end). `WatchGroup` delivers a typed `GroupCompletion`;
//     the EventBus delivers `task.group_resolved` carrying the SAME
//     payload to subscribers; a different-tenant subscriber sees
//     neither (identity-scoped isolation).
//   - TestE2E_Wave6_TaskGroup_CancelPath_DeliversReason: `CancelGroup`
//     with `propagate=true` cascades the cancel to every non-terminal
//     member; `WatchGroup` delivers `GroupCompletion{FinalStatus:
//     GroupCancelled, Reason: ...}`; the EventBus delivers
//     `task.group_cancelled` carrying the same payload.
//   - TestE2E_Wave6_LoopbackRemoteTransport_TasksRoundTrip: a
//     registered loopback A2A Agent receives a `SendMessage` and
//     spawns a Harbor task into the local TaskRegistry; the returned
//     A2A Task envelope carries the spawned TaskID; the EventBus
//     observes `task.spawned`. Proves Phase 22 ↔ Phase 20 wiring.
//   - TestE2E_Wave6_Concurrent_MultiTenant_TaskGroups: 8 tenants ×
//     4 sessions concurrent group lifecycles; each session creates a
//     group + 3 members, subscribes WatchGroup, completes the group,
//     asserts the completion lands with the correct identity. No
//     cross-talk; goroutine baseline restored after teardown.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/a2a"
	"github.com/hurtener/Harbor/internal/distributed/drivers/loopback"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// TestE2E_Wave6_TaskGroup_WatchGroup_ResolvePath wires TaskRegistry +
// EventBus + ArtifactStore end-to-end and proves three contracts:
//
//  1. `WatchGroup` delivers a typed `GroupCompletion` payload when the
//     group resolves naturally (all members terminal).
//  2. The EventBus delivers a `task.group_resolved` event carrying the
//     SAME `GroupCompletion` so subscribers (Console, planner runtime
//     at Phase 42+, sidecar status emitters) consume one canonical
//     shape.
//  3. `MemberOutcome.Result` is ref-shaped (D-022/D-026): a member
//     whose tool output is heavy stuffs an `ArtifactRef` into the
//     result, NOT the raw bytes. The ref round-trips through the
//     completion payload unchanged; the artifact is still loadable.
//
// Failure mode covered: a different-tenant subscriber on the same bus
// receives ZERO events from the test tenant's group resolution
// (identity-scoped subscription filter).
func TestE2E_Wave6_TaskGroup_WatchGroup_ResolvePath(t *testing.T) {
	deps, cleanup := openWave6(t)
	defer cleanup()

	tenA := identity.Identity{TenantID: "T1", UserID: "U1", SessionID: "S1"}
	ctxA, err := identity.With(context.Background(), tenA)
	if err != nil {
		t.Fatalf("identity.With(tenant A): %v", err)
	}
	tenB := identity.Identity{TenantID: "T2", UserID: "U2", SessionID: "S2"}
	ctxB, err := identity.With(context.Background(), tenB)
	if err != nil {
		t.Fatalf("identity.With(tenant B): %v", err)
	}

	// Subscribe tenant A to the resolve event.
	subA, err := deps.bus.Subscribe(ctxA, events.Filter{
		Tenant:  tenA.TenantID,
		User:    tenA.UserID,
		Session: tenA.SessionID,
		Types:   []events.EventType{tasks.EventTypeTaskGroupResolved, tasks.EventTypeTaskSpawned},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(A): %v", err)
	}
	defer subA.Cancel()

	// Subscribe tenant B (cross-tenant isolation gate).
	subB, err := deps.bus.Subscribe(ctxB, events.Filter{
		Tenant: tenB.TenantID, User: tenB.UserID, Session: tenB.SessionID,
		Types: []events.EventType{tasks.EventTypeTaskGroupResolved},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe(B): %v", err)
	}
	defer subB.Cancel()

	// Create the group. RetainTurn=false → the planner-runtime path
	// where WatchGroup is the wake mechanism (D-032).
	grp, err := deps.reg.ResolveOrCreateGroup(ctxA, tasks.GroupRequest{
		SessionID:   tenA,
		OwnerTaskID: tasks.TaskID("owner"),
		RetainTurn:  false,
		FailFast:    false,
		Description: "wave6-resolve",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}

	// WatchGroup BEFORE spawning members so we cover the
	// register-then-resolve order (the late-subscriber order is
	// covered by the per-package conformance suite).
	completionCh, unsub, err := deps.reg.WatchGroup(tenA, grp.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	defer unsub()

	// Spawn three foreground members of the group.
	const memberCount = 3
	memberIDs := make([]tasks.TaskID, memberCount)
	for i := 0; i < memberCount; i++ {
		h, err := deps.reg.Spawn(ctxA, tasks.SpawnRequest{
			Identity:    identity.Quadruple{Identity: tenA},
			Kind:        tasks.KindForeground,
			Description: fmt.Sprintf("member-%d", i),
			GroupID:     grp.ID,
		})
		if err != nil {
			t.Fatalf("Spawn member %d: %v", i, err)
		}
		memberIDs[i] = h.ID
	}
	if err := deps.reg.SealGroup(ctxA, grp.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}

	// One member produces a heavy result → goes through the
	// ArtifactStore; the `MemberOutcome.Result.Value` carries the
	// ref-shaped JSON, NOT the raw bytes (D-026).
	scope := artifacts.ArtifactScope{
		TenantID: tenA.TenantID, UserID: tenA.UserID, SessionID: tenA.SessionID,
		TaskID: string(memberIDs[0]),
	}
	scoped := artifacts.NewScoped(deps.artStore, scope)
	heavy := make([]byte, 40*1024)
	for i := range heavy {
		heavy[i] = byte(i % 251)
	}
	ref, err := scoped.PutBytes(ctxA, heavy, artifacts.PutOpts{
		MimeType:  "application/octet-stream",
		Namespace: "wave6.tool-output",
	})
	if err != nil {
		t.Fatalf("scoped.PutBytes(heavy): %v", err)
	}
	refJSON, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("json.Marshal(ref): %v", err)
	}

	// Drive the three members through Running → Complete.
	for i, id := range memberIDs {
		if err := deps.reg.MarkRunning(ctxA, id); err != nil {
			t.Fatalf("MarkRunning %d: %v", i, err)
		}
		result := tasks.TaskResult{Value: json.RawMessage(`{"ok":true}`)}
		if i == 0 {
			// The heavy member: stuff the ArtifactRef JSON in.
			result = tasks.TaskResult{Value: refJSON}
		}
		if err := deps.reg.MarkComplete(ctxA, id, result); err != nil {
			t.Fatalf("MarkComplete %d: %v", i, err)
		}
	}

	// Receive the GroupCompletion from WatchGroup.
	var comp tasks.GroupCompletion
	select {
	case got, ok := <-completionCh:
		if !ok {
			t.Fatalf("WatchGroup channel closed without delivery")
		}
		comp = got
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for WatchGroup delivery")
	}

	if comp.FinalStatus != tasks.GroupCompleted {
		t.Errorf("FinalStatus=%q want %q", comp.FinalStatus, tasks.GroupCompleted)
	}
	if comp.GroupID != grp.ID {
		t.Errorf("GroupID=%q want %q", comp.GroupID, grp.ID)
	}
	if comp.SessionID != tenA {
		t.Errorf("SessionID=%+v want %+v", comp.SessionID, tenA)
	}
	if got, want := len(comp.Members), memberCount; got != want {
		t.Errorf("len(Members)=%d want %d", got, want)
	}
	if comp.Reason != "" {
		t.Errorf("Reason=%q want empty (Completed)", comp.Reason)
	}

	// The heavy member's MemberOutcome.Result.Value is the ref-shaped
	// JSON; load via the same store and compare to the original heavy
	// bytes.
	var heavyOutcome *tasks.MemberOutcome
	for i := range comp.Members {
		if comp.Members[i].TaskID == memberIDs[0] {
			heavyOutcome = &comp.Members[i]
			break
		}
	}
	if heavyOutcome == nil || heavyOutcome.Result == nil {
		t.Fatalf("heavy member outcome missing: %+v", comp.Members)
	}
	var roundtripped artifacts.ArtifactRef
	if err := json.Unmarshal(heavyOutcome.Result.Value, &roundtripped); err != nil {
		t.Fatalf("MemberOutcome.Result is not ref-shaped: %v (value=%q)", err, heavyOutcome.Result.Value)
	}
	if roundtripped.ID != ref.ID {
		t.Errorf("ref roundtrip mismatch: got %q want %q", roundtripped.ID, ref.ID)
	}
	gotBytes, found, err := scoped.Get(ctxA, roundtripped.ID)
	if err != nil {
		t.Fatalf("scoped.Get(ref): %v", err)
	}
	if !found {
		t.Fatalf("ref artifact not found after roundtrip")
	}
	if !bytesEqualWave6(gotBytes, heavy) {
		t.Errorf("heavy bytes mismatch after artifact roundtrip")
	}

	// The bus event landed with the same Completion payload.
	resolved := waitForGroupEvent(t, subA, tasks.EventTypeTaskGroupResolved, 2*time.Second)
	resolvedPayload, ok := resolved.Payload.(tasks.TaskGroupResolvedPayload)
	if !ok {
		t.Fatalf("bus event payload type=%T want TaskGroupResolvedPayload", resolved.Payload)
	}
	if resolvedPayload.Completion.GroupID != comp.GroupID {
		t.Errorf("bus Completion.GroupID=%q != WatchGroup Completion.GroupID=%q",
			resolvedPayload.Completion.GroupID, comp.GroupID)
	}
	if resolvedPayload.Completion.FinalStatus != comp.FinalStatus {
		t.Errorf("bus Completion.FinalStatus mismatch")
	}
	if len(resolvedPayload.Completion.Members) != len(comp.Members) {
		t.Errorf("bus Completion.Members count=%d want %d",
			len(resolvedPayload.Completion.Members), len(comp.Members))
	}

	// Cross-tenant: subscriber B sees no events from tenant A's group.
	select {
	case ev := <-subB.Events():
		t.Errorf("cross-tenant leak: subscriber B received event %+v", ev)
	case <-time.After(100 * time.Millisecond):
		// Expected: identity-filtered Subscribe drops cross-tenant events.
	}
}

// TestE2E_Wave6_TaskGroup_CancelPath_DeliversReason proves the cancel
// path of the group-completion contract:
//
//   - `CancelGroup(reason, propagate=true)` cascades the cancel to
//     every non-terminal member.
//   - The WatchGroup subscriber receives `GroupCompletion{FinalStatus:
//     GroupCancelled, Reason: <reason>}`.
//   - The bus delivers `task.group_cancelled` carrying the same
//     payload.
//   - All members transitioned to `StatusCancelled` (via `Get`).
func TestE2E_Wave6_TaskGroup_CancelPath_DeliversReason(t *testing.T) {
	deps, cleanup := openWave6(t)
	defer cleanup()

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	sub, err := deps.bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{tasks.EventTypeTaskGroupCancelled},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	grp, err := deps.reg.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:   id,
		OwnerTaskID: tasks.TaskID("owner"),
		Description: "wave6-cancel",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	completionCh, unsub, err := deps.reg.WatchGroup(id, grp.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	defer unsub()

	const memberCount = 3
	memberIDs := make([]tasks.TaskID, memberCount)
	for i := 0; i < memberCount; i++ {
		h, err := deps.reg.Spawn(ctx, tasks.SpawnRequest{
			Identity: identity.Quadruple{Identity: id},
			Kind:     tasks.KindForeground,
			GroupID:  grp.ID,
		})
		if err != nil {
			t.Fatalf("Spawn member %d: %v", i, err)
		}
		memberIDs[i] = h.ID
	}
	// Mark only member 0 running; leave 1 + 2 at Pending. The cancel
	// path must cover both Running → Cancelled and Pending → Cancelled
	// transitions in the same cascade.
	if err := deps.reg.MarkRunning(ctx, memberIDs[0]); err != nil {
		t.Fatalf("MarkRunning member 0: %v", err)
	}

	const reason = "user-cancelled"
	if err := deps.reg.CancelGroup(ctx, grp.ID, reason, true); err != nil {
		t.Fatalf("CancelGroup: %v", err)
	}

	// WatchGroup delivers the cancel-shaped completion.
	var comp tasks.GroupCompletion
	select {
	case got, ok := <-completionCh:
		if !ok {
			t.Fatalf("WatchGroup channel closed without delivery")
		}
		comp = got
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for cancel delivery")
	}
	if comp.FinalStatus != tasks.GroupCancelled {
		t.Errorf("FinalStatus=%q want %q", comp.FinalStatus, tasks.GroupCancelled)
	}
	if comp.Reason != reason {
		t.Errorf("Reason=%q want %q", comp.Reason, reason)
	}
	if got, want := len(comp.Members), memberCount; got != want {
		t.Errorf("len(Members)=%d want %d", got, want)
	}

	// Bus event carries the same payload.
	cancelled := waitForGroupEvent(t, sub, tasks.EventTypeTaskGroupCancelled, 2*time.Second)
	cancelPayload, ok := cancelled.Payload.(tasks.TaskGroupCancelledPayload)
	if !ok {
		t.Fatalf("payload type=%T want TaskGroupCancelledPayload", cancelled.Payload)
	}
	if cancelPayload.Completion.Reason != reason {
		t.Errorf("bus Completion.Reason=%q want %q", cancelPayload.Completion.Reason, reason)
	}
	if cancelPayload.Completion.FinalStatus != tasks.GroupCancelled {
		t.Errorf("bus Completion.FinalStatus=%q want %q",
			cancelPayload.Completion.FinalStatus, tasks.GroupCancelled)
	}

	// All members terminated as cancelled.
	for i, taskID := range memberIDs {
		got, err := deps.reg.Get(ctx, taskID)
		if err != nil {
			t.Errorf("Get member %d: %v", i, err)
			continue
		}
		if got.Status != tasks.StatusCancelled {
			t.Errorf("member %d Status=%q want %q", i, got.Status, tasks.StatusCancelled)
		}
	}
}

// TestE2E_Wave6_LoopbackRemoteTransport_TasksRoundTrip proves the
// Phase 22 ↔ Phase 20 wiring: a loopback A2A Agent receives a
// SendMessage call, spawns a Harbor task on the local TaskRegistry,
// and returns the spawned task's A2A representation. The EventBus
// observes a `task.spawned` event for the new task — proving the
// distributed-edge and the task-registry events compose without
// per-test glue.
//
// This is the V1 reference for how Phase 29 (southbound A2A wire)
// will wire in: at Phase 29 the transport flips from loopback to
// HTTP/gRPC but the Agent → TaskRegistry → events composition stays
// identical.
func TestE2E_Wave6_LoopbackRemoteTransport_TasksRoundTrip(t *testing.T) {
	deps, cleanup := openWave6(t)
	defer cleanup()

	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	sub, err := deps.bus.Subscribe(ctx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{tasks.EventTypeTaskSpawned},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Loopback RemoteTransport with an in-test Agent that spawns a
	// Harbor task on receive. This is the simplest realistic Agent: a
	// remote endpoint that, in response to "do work", registers a
	// background task locally and returns its handle.
	rt, err := loopback.NewRemoteTransport(distributed.Dependencies{})
	if err != nil {
		t.Fatalf("loopback.NewRemoteTransport: %v", err)
	}
	defer rt.Close(context.Background())
	lt := rt.(loopback.LoopbackTransport)

	agent := &taskSpawningAgent{reg: deps.reg, identity: id}
	lt.RegisterAgent("agent-1", agent)

	res, err := rt.Send(ctx, distributed.RemoteCallRequest{
		AgentURL: "agent-1",
		Kind:     distributed.RemoteCallKindSend,
		Message: a2a.Message{
			MessageID: "m-1",
			Role:      a2a.RoleUser,
			Parts:     a2a.Parts{&a2a.TextPart{Text: "do-work"}},
		},
	})
	if err != nil {
		t.Fatalf("rt.Send: %v", err)
	}
	if res.Task.ID == "" {
		t.Fatalf("rt.Send returned empty Task.ID")
	}
	// The A2A Task.ID is the spawned Harbor TaskID (the Agent passes
	// it through verbatim so the caller can correlate).
	taskID := tasks.TaskID(res.Task.ID)
	got, err := deps.reg.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("reg.Get(%q): %v", taskID, err)
	}
	if got.Kind != tasks.KindBackground {
		t.Errorf("spawned task Kind=%q want %q", got.Kind, tasks.KindBackground)
	}

	// The bus observes task.spawned for the new task.
	spawned := waitForGroupEvent(t, sub, tasks.EventTypeTaskSpawned, 2*time.Second)
	spawnedPayload, ok := spawned.Payload.(tasks.TaskSpawnedPayload)
	if !ok {
		t.Fatalf("payload type=%T want TaskSpawnedPayload", spawned.Payload)
	}
	if spawnedPayload.TaskID != taskID {
		t.Errorf("bus TaskID=%q want %q", spawnedPayload.TaskID, taskID)
	}

	// Failure mode: Send to a URL with no registered agent fails
	// loudly with ErrAgentNotFound (no silent default-routing).
	_, err = rt.Send(ctx, distributed.RemoteCallRequest{
		AgentURL: "nonexistent",
		Message:  a2a.Message{MessageID: "m-2", Role: a2a.RoleUser},
	})
	if !errors.Is(err, distributed.ErrAgentNotFound) {
		t.Errorf("Send(missing agent): err=%v want errors.Is ErrAgentNotFound", err)
	}
}

// TestE2E_Wave6_Concurrent_MultiTenant_TaskGroups runs N tenants × M
// sessions concurrent group lifecycles against the SAME shared
// TaskRegistry + EventBus + ArtifactStore, asserting per-tenant
// isolation under load and a clean goroutine baseline after teardown
// (D-025).
//
// Each goroutine:
//
//  1. Creates a fresh group scoped to its session.
//  2. Subscribes WatchGroup before spawning members.
//  3. Spawns 3 members, drives them through Complete.
//  4. Receives the WatchGroup completion and verifies identity matches.
//
// Cross-tenant isolation: the per-session `MemberOutcome.TaskID`s
// must NOT appear in any other session's completion payload.
func TestE2E_Wave6_Concurrent_MultiTenant_TaskGroups(t *testing.T) {
	const tenantCount = 8
	const sessionsPerTenant = 4
	const membersPerGroup = 3

	baseline := runtime.NumGoroutine()
	deps, cleanup := openWave6(t)

	var (
		wg     sync.WaitGroup
		errCnt atomic.Int64
		// Per-(tenant, session) we record the set of TaskIDs that
		// landed in the WatchGroup payload, so we can prove no cross-
		// session leak.
		seenMu      sync.Mutex
		seenPerSess = make(map[string]map[tasks.TaskID]bool)
	)
	wg.Add(tenantCount * sessionsPerTenant)

	for ti := 0; ti < tenantCount; ti++ {
		for sj := 0; sj < sessionsPerTenant; sj++ {
			ti, sj := ti, sj
			go func() {
				defer wg.Done()
				id := identity.Identity{
					TenantID:  fmt.Sprintf("T-%d", ti),
					UserID:    fmt.Sprintf("U-%d", ti),
					SessionID: fmt.Sprintf("S-%d-%d", ti, sj),
				}
				ctx, err := identity.With(context.Background(), id)
				if err != nil {
					errCnt.Add(1)
					t.Errorf("identity.With(%s): %v", id.SessionID, err)
					return
				}

				grp, err := deps.reg.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
					SessionID:   id,
					OwnerTaskID: tasks.TaskID("owner-" + id.SessionID),
					Description: "stress",
				})
				if err != nil {
					errCnt.Add(1)
					t.Errorf("ResolveOrCreateGroup(%s): %v", id.SessionID, err)
					return
				}
				completionCh, unsub, err := deps.reg.WatchGroup(id, grp.ID)
				if err != nil {
					errCnt.Add(1)
					t.Errorf("WatchGroup(%s): %v", id.SessionID, err)
					return
				}
				defer unsub()

				memberIDs := make([]tasks.TaskID, membersPerGroup)
				for m := 0; m < membersPerGroup; m++ {
					h, err := deps.reg.Spawn(ctx, tasks.SpawnRequest{
						Identity:    identity.Quadruple{Identity: id},
						Kind:        tasks.KindForeground,
						Description: fmt.Sprintf("stress-%s-m%d", id.SessionID, m),
						GroupID:     grp.ID,
					})
					if err != nil {
						errCnt.Add(1)
						t.Errorf("Spawn(%s, m=%d): %v", id.SessionID, m, err)
						return
					}
					memberIDs[m] = h.ID
				}
				if err := deps.reg.SealGroup(ctx, grp.ID); err != nil {
					errCnt.Add(1)
					t.Errorf("SealGroup(%s): %v", id.SessionID, err)
					return
				}
				for m, taskID := range memberIDs {
					if err := deps.reg.MarkRunning(ctx, taskID); err != nil {
						errCnt.Add(1)
						t.Errorf("MarkRunning(%s, m=%d): %v", id.SessionID, m, err)
						return
					}
					payload := []byte(fmt.Sprintf(`{"sess":%q,"m":%d}`, id.SessionID, m))
					if err := deps.reg.MarkComplete(ctx, taskID, tasks.TaskResult{Value: payload}); err != nil {
						errCnt.Add(1)
						t.Errorf("MarkComplete(%s, m=%d): %v", id.SessionID, m, err)
						return
					}
				}

				select {
				case comp, ok := <-completionCh:
					if !ok {
						errCnt.Add(1)
						t.Errorf("WatchGroup(%s): channel closed without delivery", id.SessionID)
						return
					}
					if comp.SessionID != id {
						errCnt.Add(1)
						t.Errorf("completion identity drift: got %+v want %+v", comp.SessionID, id)
						return
					}
					if got, want := len(comp.Members), membersPerGroup; got != want {
						errCnt.Add(1)
						t.Errorf("len(Members)=%d want %d for %s", got, want, id.SessionID)
						return
					}
					seenMu.Lock()
					seen := make(map[tasks.TaskID]bool, len(comp.Members))
					for _, mo := range comp.Members {
						seen[mo.TaskID] = true
					}
					seenPerSess[id.SessionID] = seen
					seenMu.Unlock()
				case <-time.After(5 * time.Second):
					errCnt.Add(1)
					t.Errorf("WatchGroup(%s): timeout", id.SessionID)
					return
				}
			}()
		}
	}

	wg.Wait()
	if n := errCnt.Load(); n != 0 {
		cleanup()
		t.Fatalf("%d concurrent operations errored", n)
	}

	// Cross-session isolation: no TaskID appears in two different
	// sessions' completion payloads.
	taskOwner := make(map[tasks.TaskID]string)
	for sess, seen := range seenPerSess {
		for taskID := range seen {
			if prior, ok := taskOwner[taskID]; ok {
				t.Errorf("cross-session leak: TaskID=%q in %s and %s", taskID, prior, sess)
			}
			taskOwner[taskID] = sess
		}
	}

	// Teardown.
	cleanup()

	// Gosched-only settle loop per AGENTS.md §11 (no time.Sleep for
	// sync). 2s hard cap; +5 tolerance for parked-but-not-yet-retired
	// goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// --- helpers ---

// wave6Deps bundles the subsystems wave6_test.go composes. Constructed
// once per test by openWave6 and torn down via the returned closure.
type wave6Deps struct {
	store    state.StateStore
	bus      events.EventBus
	artStore artifacts.ArtifactStore
	reg      tasks.TaskRegistry
}

// openWave6 stands up the inmem-driver stack for a wave-6 e2e test.
// Tasks ride on inmem state + inmem events + inmem artifacts; the
// loopback distributed driver is constructed per-test where used.
//
// Returns the bundle and a cleanup func that closes everything in the
// correct order. Closing is idempotent across paths (some tests call
// cleanup before assertions, others after).
func openWave6(t *testing.T) (*wave6Deps, func()) {
	t.Helper()
	cfg := wave6Config()
	red := auditpatterns.New()

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("events.Open: %v", err)
	}
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("artifacts.Open: %v", err)
	}
	reg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      cfg.Tasks,
	})
	if err != nil {
		_ = artStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}

	closed := false
	cleanup := func() {
		if closed {
			return
		}
		closed = true
		_ = reg.Close(context.Background())
		_ = artStore.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = store.Close(context.Background())
	}
	return &wave6Deps{store: store, bus: bus, artStore: artStore, reg: reg}, cleanup
}

// wave6Config returns the in-memory config wave-6 tests use. All
// subsystem drivers default to their inmem variants — wave-5 already
// exercised the durable + multi-tenant SQLite + FS axis; wave-6's
// surface (tasks + groups + distributed loopback) is process-local
// at V1 by design, so inmem is the right floor.
func wave6Config() *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-wave6-e2e",
		},
		State: config.StateConfig{Driver: "inmem"},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			RepairAttempts: 2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "inmem",
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		Tasks: config.TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
		Memory:      config.MemoryConfig{Driver: "inmem", Strategy: "none"},
	}
}

// waitForGroupEvent drains the subscription until it sees an event of
// the expected type, bounded by `timeout`. Other event types
// (e.g. task.spawned interleaving group events on the same
// subscription) are skipped.
func waitForGroupEvent(t *testing.T, sub events.Subscription, want events.EventType, timeout time.Duration) events.Event {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatalf("subscription closed before %q", want)
			}
			if ev.Type == want {
				return ev
			}
		case <-deadline.C:
			t.Fatalf("timeout waiting for event %q", want)
			return events.Event{}
		}
	}
}

// taskSpawningAgent is the in-test loopback Agent implementation
// wave-6's loopback round-trip test registers. On SendMessage, it
// spawns a Harbor background task into the registry and returns the
// new task's A2A representation. The other Agent methods are not
// exercised here — the loopback driver's per-package tests cover the
// full surface.
type taskSpawningAgent struct {
	reg      tasks.TaskRegistry
	identity identity.Identity
}

func (a *taskSpawningAgent) SendMessage(ctx context.Context, msg a2a.Message, _ a2a.SendMessageConfiguration) (a2a.Task, error) {
	h, err := a.reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: a.identity},
		Kind:        tasks.KindBackground,
		Description: "loopback-" + msg.MessageID,
	})
	if err != nil {
		return a2a.Task{}, err
	}
	return a2a.Task{
		ID:        string(h.ID),
		ContextID: msg.ContextID,
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}, nil
}

func (a *taskSpawningAgent) SendStreamingMessage(context.Context, a2a.Message, a2a.SendMessageConfiguration) (<-chan a2a.StreamResponse, error) {
	return nil, errors.New("not used in wave-6 e2e")
}

func (a *taskSpawningAgent) GetTask(context.Context, string, string) (a2a.Task, error) {
	return a2a.Task{}, distributed.ErrTaskNotFound
}

func (a *taskSpawningAgent) ListTasks(context.Context, loopback.ListTasksFilter) ([]a2a.Task, error) {
	return nil, nil
}

func (a *taskSpawningAgent) CancelTask(context.Context, string, string) (a2a.Task, error) {
	return a2a.Task{}, distributed.ErrTaskNotFound
}

func (a *taskSpawningAgent) SubscribeToTask(context.Context, string, string) (<-chan a2a.StreamResponse, error) {
	return nil, errors.New("not used in wave-6 e2e")
}

func (a *taskSpawningAgent) CreateTaskPushNotificationConfig(context.Context, a2a.TaskPushNotificationConfig) (a2a.TaskPushNotificationConfig, error) {
	return a2a.TaskPushNotificationConfig{}, errors.New("not used")
}

func (a *taskSpawningAgent) GetTaskPushNotificationConfig(context.Context, string, string) (a2a.TaskPushNotificationConfig, error) {
	return a2a.TaskPushNotificationConfig{}, errors.New("not used")
}

func (a *taskSpawningAgent) ListTaskPushNotificationConfigs(context.Context, string) ([]a2a.TaskPushNotificationConfig, error) {
	return nil, nil
}

func (a *taskSpawningAgent) DeleteTaskPushNotificationConfig(context.Context, string, string) error {
	return errors.New("not used")
}

func (a *taskSpawningAgent) GetExtendedAgentCard(context.Context) (a2a.AgentCard, error) {
	return a2a.AgentCard{}, nil
}

// bytesEqualWave6 is local to avoid colliding with wave5_test.go's
// helper (both files are `package integration_test`).
func bytesEqualWave6(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
