package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/conformancetest"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// memBackend is a test engine.Backend. SaveX is a no-op (the engine
// holds the authoritative in-memory state); Hydrate replays a seeded
// Snapshot. saveErr, when set, makes every SaveX fail loudly; groupErr
// overrides it for SaveGroup only (fail group persist while task
// persist succeeds). It records DeleteTask calls and SaveTask counts so
// rollback / compensation can be asserted.
type memBackend struct {
	seed     engine.Snapshot
	saveErr  error
	groupErr error

	mu           sync.Mutex
	savedTasks   int
	deletedTasks []tasks.TaskID
}

func (m *memBackend) SaveTask(_ context.Context, _ engine.TaskRecord) error {
	m.mu.Lock()
	m.savedTasks++
	m.mu.Unlock()
	return m.saveErr
}

func (m *memBackend) DeleteTask(_ context.Context, t *tasks.Task) error {
	m.mu.Lock()
	if t != nil {
		m.deletedTasks = append(m.deletedTasks, t.ID)
	}
	m.mu.Unlock()
	return nil
}

func (m *memBackend) SaveGroup(_ context.Context, _ *tasks.TaskGroup) error {
	if m.groupErr != nil {
		return m.groupErr
	}
	return m.saveErr
}
func (m *memBackend) SavePatch(_ context.Context, _ *tasks.Patch) error  { return m.saveErr }
func (m *memBackend) Hydrate(_ context.Context) (engine.Snapshot, error) { return m.seed, nil }

func (m *memBackend) deletedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deletedTasks)
}

func (m *memBackend) saveTaskCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.savedTasks
}

func mkBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	return bus
}

// TestEngine_Conformance runs the canonical TaskRegistry suite against
// the engine with a no-op backend, locking the shared state machine to
// the contract independent of any driver's persistence.
func TestEngine_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (tasks.TaskRegistry, func()) {
		bus := mkBus(t)
		eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		return eng, func() {
			ctx := context.Background()
			_ = eng.Close(ctx)
			_ = bus.Close(ctx)
		}
	})
}

// TestEngine_OldestRetainedAt_RuntimeWideAcrossTenants pins the
// identity-free runtime-wide retention reader (D-310): it reports the
// oldest CreatedAt across EVERY tenant's tasks — the fleet-observe
// horizon — not a per-caller slice. Empty → (zero,false,nil); after
// Close → ErrRegistryClosed.
func TestEngine_OldestRetainedAt_RuntimeWideAcrossTenants(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	quadA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s1"}}
	quadB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s2"}}
	seed := engine.Snapshot{Tasks: []engine.TaskRecord{
		{Task: &tasks.Task{ID: "01A", Identity: quadA, Kind: tasks.KindBackground,
			Status: tasks.StatusRunning, CreatedAt: base.Add(2 * time.Hour).UnixNano(), UpdatedAt: base.UnixNano()}},
		{Task: &tasks.Task{ID: "01B", Identity: quadB, Kind: tasks.KindBackground,
			Status: tasks.StatusRunning, CreatedAt: base.UnixNano(), UpdatedAt: base.UnixNano()}}, // oldest, other tenant
	}}

	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: seed})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	oldest, present, err := eng.OldestRetainedAt(context.Background())
	if err != nil || !present {
		t.Fatalf("OldestRetainedAt = (%v, %v, %v), want a present horizon", oldest, present, err)
	}
	if !oldest.Equal(base) {
		t.Fatalf("runtime-wide oldest = %v, want %v (tenant-b's task, cross-tenant)", oldest, base)
	}

	if err := eng.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := eng.OldestRetainedAt(context.Background()); err == nil {
		t.Fatal("OldestRetainedAt after Close: want ErrRegistryClosed, got nil")
	}
}

func TestEngine_OldestRetainedAt_Empty_NotPresent(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	oldest, present, err := eng.OldestRetainedAt(context.Background())
	if err != nil || present || !oldest.IsZero() {
		t.Fatalf("empty OldestRetainedAt = (%v, %v, %v), want (zero, false, nil)", oldest, present, err)
	}
}

func TestEngine_New_NilArgs_FailLoud(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	if _, err := engine.New(nil, auditpatterns.New(), &memBackend{}); err == nil {
		t.Error("New(nil bus): want error, got nil")
	}
	if _, err := engine.New(bus, nil, &memBackend{}); err == nil {
		t.Error("New(nil redactor): want error, got nil")
	}
	if _, err := engine.New(bus, auditpatterns.New(), nil); err == nil {
		t.Error("New(nil backend): want error, got nil")
	}
}

// TestEngine_RecoverInterruptedTasks_RunningToFailed asserts the open-
// time sweep transitions a hydrated Running task to Failed with the
// reserved recovery code and emits exactly one task.failed event.
func TestEngine_RecoverInterruptedTasks_RunningToFailed(t *testing.T) {
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	now := time.Now().UnixNano()
	running := &tasks.Task{
		ID: "01RUNNING", Identity: quad, Kind: tasks.KindBackground,
		Status: tasks.StatusRunning, CreatedAt: now, UpdatedAt: now,
	}
	pending := &tasks.Task{
		ID: "01PENDING", Identity: quad, Kind: tasks.KindBackground,
		Status: tasks.StatusPending, CreatedAt: now, UpdatedAt: now,
	}
	paused := &tasks.Task{
		ID: "01PAUSED", Identity: quad, Kind: tasks.KindBackground,
		Status: tasks.StatusPaused, CreatedAt: now, UpdatedAt: now,
	}
	seed := engine.Snapshot{Tasks: []engine.TaskRecord{{Task: running}, {Task: pending}, {Task: paused}}}

	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: seed})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	n, err := eng.RecoverInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("RecoverInterruptedTasks: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered count = %d, want 1 (only the Running task)", n)
	}

	ctx, _ := identity.With(context.Background(), quad.Identity)
	got, err := eng.Get(ctx, "01RUNNING")
	if err != nil {
		t.Fatalf("Get(running): %v", err)
	}
	if got.Status != tasks.StatusFailed {
		t.Errorf("recovered task status = %q, want Failed", got.Status)
	}
	if got.Error == nil || got.Error.Code != engine.RecoveryErrorCode {
		t.Errorf("recovered task error = %+v, want code %q", got.Error, engine.RecoveryErrorCode)
	}

	// Pending and Paused are left untouched.
	if p, _ := eng.Get(ctx, "01PENDING"); p == nil || p.Status != tasks.StatusPending {
		t.Errorf("pending task should be untouched, got %+v", p)
	}
	if p, _ := eng.Get(ctx, "01PAUSED"); p == nil || p.Status != tasks.StatusPaused {
		t.Errorf("paused task should be untouched, got %+v", p)
	}
}

// TestEngine_RecoverInterruptedTasks_NoRunning is a no-op when nothing
// was interrupted (the common steady-state restart).
func TestEngine_RecoverInterruptedTasks_NoRunning(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	n, err := eng.RecoverInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("RecoverInterruptedTasks: %v", err)
	}
	if n != 0 {
		t.Errorf("recovered = %d, want 0", n)
	}
}

// TestEngine_PersistError_Propagates asserts a backend Save error
// surfaces from a mutating call (fail-loud, no silent swallow).
func TestEngine_PersistError_Propagates(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{saveErr: errors.New("disk full")})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	_, err = eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}},
		Kind:     tasks.KindBackground,
	})
	if err == nil {
		t.Fatal("Spawn with failing backend: want error, got nil")
	}
}

func idQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

// TestEngine_SpawnIntoMissingGroup_DoesNotPersist asserts a spawn into
// a non-existent group fails BEFORE persisting the task — so a durable
// backend cannot resurrect an orphan after restart (adversarial FAIL #1
// case a).
func TestEngine_SpawnIntoMissingGroup_DoesNotPersist(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	b := &memBackend{}
	eng, err := engine.New(bus, auditpatterns.New(), b)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)
	_, err = eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, GroupID: "no-such-group"})
	if !errors.Is(err, tasks.ErrGroupNotFound) {
		t.Fatalf("Spawn into missing group: err = %v, want ErrGroupNotFound", err)
	}
	if b.saveTaskCount() != 0 {
		t.Errorf("task was persisted (%d SaveTask calls) despite group-not-found — orphan risk", b.saveTaskCount())
	}
}

// TestEngine_GroupPersistFails_CompensatesTask asserts that when the
// group persist fails AFTER the task was persisted, the task record is
// compensated (DeleteTask) and the in-memory state is fully rolled back
// — and a same-key re-spawn neither panics nor resurrects a phantom
// (adversarial FAIL #1 case b + FAIL #2).
func TestEngine_GroupPersistFails_CompensatesTask(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	b := &memBackend{}
	eng, err := engine.New(bus, auditpatterns.New(), b)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)

	g, err := eng.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: id.Identity})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}

	b.groupErr = errors.New("group disk full")
	_, err = eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, GroupID: g.ID, IdempotencyKey: "k1"})
	if err == nil {
		t.Fatal("Spawn with failing group persist: want error, got nil")
	}
	if b.deletedCount() != 1 {
		t.Errorf("task record not compensated: %d DeleteTask calls, want 1", b.deletedCount())
	}
	// The rolled-back task must not be visible.
	list, err := eng.List(ctx, id.Identity, tasks.TaskFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("rolled-back task still listed: %d tasks", len(list))
	}

	// FAIL #2: same-key re-spawn must NOT panic and must spawn fresh
	// (the idempotency index entry must have been rolled back too).
	b.groupErr = nil
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, IdempotencyKey: "k1"})
	if err != nil {
		t.Fatalf("re-spawn after rollback: %v", err)
	}
	if h.Reused {
		t.Error("re-spawn after rollback wrongly deduped to a phantom (dangling idempotency index)")
	}
}

// TestEngine_MarkComplete_PersistFails_NoDivergence asserts a persist
// failure during MarkComplete leaves the in-memory record consistent
// with the store (still Running, no result) rather than advancing it —
// so the task stays completable and a restart recovers a consistent
// state (adversarial WARN #1).
func TestEngine_MarkComplete_PersistFails_NoDivergence(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	b := &memBackend{}
	eng, err := engine.New(bus, auditpatterns.New(), b)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := eng.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	b.saveErr = errors.New("disk full")
	if err := eng.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`{"x":1}`)}); err == nil {
		t.Fatal("MarkComplete with failing persist: want error, got nil")
	}
	got, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusRunning {
		t.Errorf("status diverged after failed persist: %q, want Running (rolled back)", got.Status)
	}
	if got.Result != nil {
		t.Error("result leaked into in-memory record despite failed persist")
	}

	// With persistence restored, the task is still completable.
	b.saveErr = nil
	if err := eng.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("MarkComplete retry after persistence restored: %v", err)
	}
	if got, _ := eng.Get(ctx, h.ID); got.Status != tasks.StatusComplete {
		t.Errorf("retry did not complete: status = %q", got.Status)
	}
}

// TestEngine_Recovery_ReconcilesSealedGroup asserts the recovery sweep
// heals a group whose resolution diverged before a crash: a Sealed
// group with all members terminal (but never resolved in the store) is
// resolved to Completed on open (adversarial FAIL-1). The plan requires
// recovery to "recompute group resolution from member terminality".
func TestEngine_Recovery_ReconcilesSealedGroup(t *testing.T) {
	quad := idQuad()
	now := time.Now().UnixNano()
	member := &tasks.Task{ID: "01MEMBER", Identity: quad, Kind: tasks.KindBackground, Status: tasks.StatusComplete, CreatedAt: now, UpdatedAt: now}
	sealed := &tasks.TaskGroup{ID: "01GROUP", SessionID: quad.Identity, Status: tasks.GroupSealed, Members: []tasks.TaskID{"01MEMBER"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	seed := engine.Snapshot{Tasks: []engine.TaskRecord{{Task: member}}, Groups: []*tasks.TaskGroup{sealed}}

	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: seed})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := eng.RecoverInterruptedTasks(context.Background()); err != nil {
		t.Fatalf("RecoverInterruptedTasks: %v", err)
	}

	ctx, _ := identity.With(context.Background(), quad.Identity)
	groups, err := eng.ListGroups(ctx, quad.Identity, nil)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("ListGroups = %d, want 1", len(groups))
	}
	if groups[0].Status != tasks.GroupCompleted {
		t.Errorf("diverged sealed group not reconciled: status = %q, want Completed", groups[0].Status)
	}
}

// TestEngine_Recovery_ReconcilesFailFastGroup asserts recovery recomputes
// a fail-fast group whose failed-member cancellation diverged before a
// crash: the failed member triggers cancellation of the pending sibling
// and the group resolves Cancelled.
func TestEngine_Recovery_ReconcilesFailFastGroup(t *testing.T) {
	quad := idQuad()
	now := time.Now().UnixNano()
	failed := &tasks.Task{ID: "01FAILED", Identity: quad, Kind: tasks.KindBackground, Status: tasks.StatusFailed, Error: &tasks.TaskError{Code: "boom"}, CreatedAt: now, UpdatedAt: now}
	pending := &tasks.Task{ID: "01PEND", Identity: quad, Kind: tasks.KindBackground, Status: tasks.StatusPending, CreatedAt: now, UpdatedAt: now}
	ff := &tasks.TaskGroup{ID: "01FF", SessionID: quad.Identity, Status: tasks.GroupOpen, FailFast: true, Members: []tasks.TaskID{"01FAILED", "01PEND"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	seed := engine.Snapshot{Tasks: []engine.TaskRecord{{Task: failed}, {Task: pending}}, Groups: []*tasks.TaskGroup{ff}}

	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: seed})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if _, err := eng.RecoverInterruptedTasks(context.Background()); err != nil {
		t.Fatalf("RecoverInterruptedTasks: %v", err)
	}

	ctx, _ := identity.With(context.Background(), quad.Identity)
	groups, _ := eng.ListGroups(ctx, quad.Identity, nil)
	if len(groups) != 1 || groups[0].Status != tasks.GroupCancelled {
		t.Fatalf("fail-fast group not reconciled to Cancelled: %+v", groups)
	}
	// The pending sibling was cancelled by the fail-fast recompute.
	if sib, _ := eng.Get(ctx, "01PEND"); sib == nil || sib.Status != tasks.StatusCancelled {
		t.Errorf("pending sibling not cancelled by fail-fast reconcile: %+v", sib)
	}
}

// TestEngine_CancelHierarchy_IsolateDetachesFromAncestorCascade — the
// cancel-hierarchy invariant end-to-end (the AC-11 cascade-walk fix
// consumed): a parent P has a cascade-default child C2 and an
// isolate-marked child C1 (with its own grandchild GC1). Cancelling P
// (an ancestor cascade) sweeps C2 but leaves the whole C1 subtree
// running; a DIRECT Cancel on C1 still transitions it (isolate never
// blocks a direct target — the operator's / spawning-run's last word).
func TestEngine_CancelHierarchy_IsolateDetachesFromAncestorCascade(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)

	// Parent P (cascade default).
	p, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "P"})
	if err != nil {
		t.Fatalf("spawn P: %v", err)
	}
	pid := p.ID
	if err := eng.MarkRunning(ctx, pid); err != nil {
		t.Fatal(err)
	}

	// C1 (isolate) under P, with a grandchild GC1 (cascade) under C1.
	c1, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: id, Kind: tasks.KindBackground, Description: "C1",
		ParentTaskID: &pid, PropagateOnCancel: tasks.PropagateIsolate,
	})
	if err != nil {
		t.Fatalf("spawn C1: %v", err)
	}
	c1id := c1.ID
	if err := eng.MarkRunning(ctx, c1id); err != nil {
		t.Fatal(err)
	}
	gc1, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: id, Kind: tasks.KindBackground, Description: "GC1",
		ParentTaskID: &c1id, PropagateOnCancel: tasks.PropagateCascade,
	})
	if err != nil {
		t.Fatalf("spawn GC1: %v", err)
	}
	if err := eng.MarkRunning(ctx, gc1.ID); err != nil {
		t.Fatal(err)
	}

	// C2 (cascade default) under P.
	c2, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: id, Kind: tasks.KindBackground, Description: "C2",
		ParentTaskID: &pid, PropagateOnCancel: tasks.PropagateCascade,
	})
	if err != nil {
		t.Fatalf("spawn C2: %v", err)
	}
	if err := eng.MarkRunning(ctx, c2.ID); err != nil {
		t.Fatal(err)
	}

	// (a) Cancel P directly — ancestor cascade. C2 is swept; the whole C1
	// isolate subtree (C1 AND its grandchild GC1) survives.
	if _, err := eng.Cancel(ctx, pid, "ancestor-cancel"); err != nil {
		t.Fatalf("Cancel P: %v", err)
	}
	assertStatus(t, eng, ctx, pid, tasks.StatusCancelled, "P (direct target)")
	assertStatus(t, eng, ctx, c2.ID, tasks.StatusCancelled, "C2 (cascade swept)")
	assertStatus(t, eng, ctx, c1id, tasks.StatusRunning, "C1 (isolate detached)")
	assertStatus(t, eng, ctx, gc1.ID, tasks.StatusRunning, "GC1 (isolate subtree detached)")

	// (b) A direct Cancel on the isolate task C1 DOES transition it —
	// isolate never blocks a direct target (the operator's last word, and
	// the same path the run's own _cancel_task drives). Its cascade child
	// GC1 is then swept by C1's own cascade... wait: C1 is isolate, so its
	// OWN cascade does not start — only C1 transitions. GC1 stays running.
	if _, err := eng.Cancel(ctx, c1id, "direct-cancel-isolate"); err != nil {
		t.Fatalf("direct Cancel C1: %v", err)
	}
	assertStatus(t, eng, ctx, c1id, tasks.StatusCancelled, "C1 (direct cancel reaches isolate)")
	assertStatus(t, eng, ctx, gc1.ID, tasks.StatusRunning, "GC1 (C1 isolate → no cascade from its direct cancel)")

	// (c) A direct Cancel on GC1 finally reaches it (agent/operator direct
	// cancel of an own descendant is never gated on any isolate ancestor).
	if _, err := eng.Cancel(ctx, gc1.ID, "direct-cancel-gc1"); err != nil {
		t.Fatalf("direct Cancel GC1: %v", err)
	}
	assertStatus(t, eng, ctx, gc1.ID, tasks.StatusCancelled, "GC1 (direct cancel)")
}

// TestEngine_CancelCascade_SkipsTerminalDescendantEnqueuesGrandchildren —
// exercises the shared cascade walk's terminal-skip branch and its
// past-a-cascaded-node enqueue: a parent P (cascade) has an
// already-complete child C1 (whose own grandchild GC1 must therefore NOT
// be reached — a terminal node stops that sub-branch) and a running
// child C2 (cascade) with a running grandchild GC2 that IS cancelled.
func TestEngine_CancelCascade_SkipsTerminalDescendantEnqueuesGrandchildren(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)

	spawnRunning := func(desc string, parent *tasks.TaskID) tasks.TaskID {
		h, err := eng.Spawn(ctx, tasks.SpawnRequest{
			Identity: id, Kind: tasks.KindBackground, Description: desc,
			ParentTaskID: parent, PropagateOnCancel: tasks.PropagateCascade,
		})
		if err != nil {
			t.Fatalf("spawn %s: %v", desc, err)
		}
		if err := eng.MarkRunning(ctx, h.ID); err != nil {
			t.Fatalf("MarkRunning %s: %v", desc, err)
		}
		return h.ID
	}

	pid := spawnRunning("P", nil)
	c1 := spawnRunning("C1", &pid)
	gc1 := spawnRunning("GC1", &c1)
	c2 := spawnRunning("C2", &pid)
	gc2 := spawnRunning("GC2", &c2)

	// Drive C1 terminal (complete) BEFORE the cascade reaches it.
	if err := eng.MarkComplete(ctx, c1, tasks.TaskResult{Value: []byte(`"c1-done"`)}); err != nil {
		t.Fatalf("MarkComplete C1: %v", err)
	}

	if _, err := eng.Cancel(ctx, pid, "cascade"); err != nil {
		t.Fatalf("Cancel P: %v", err)
	}

	// C1 stays complete (terminal-skip); its grandchild GC1 is NOT reached
	// because the terminal node stops that sub-branch. C2 + GC2 cancelled.
	assertStatus(t, eng, ctx, pid, tasks.StatusCancelled, "P")
	assertStatus(t, eng, ctx, c1, tasks.StatusComplete, "C1 (terminal, skipped)")
	assertStatus(t, eng, ctx, gc1, tasks.StatusRunning, "GC1 (behind terminal C1, not reached)")
	assertStatus(t, eng, ctx, c2, tasks.StatusCancelled, "C2 (cascade)")
	assertStatus(t, eng, ctx, gc2, tasks.StatusCancelled, "GC2 (cascade past C2)")
}

func assertStatus(t *testing.T, eng *engine.Engine, ctx context.Context, id tasks.TaskID, want tasks.TaskStatus, label string) {
	t.Helper()
	task, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %s (%q): %v", label, id, err)
	}
	if task.Status != want {
		t.Errorf("%s: status = %q, want %q", label, task.Status, want)
	}
}
