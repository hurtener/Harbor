package engine_test

import (
	"context"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestEngine_AllocationFinalityWaitsForAcceptedDescendants(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	mgr := allocation.New(st)
	e, err := engine.New(bus, auditpatterns.New(), &memBackend{}, engine.WithAllocations(mgr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	if !e.InferenceAllocationFinality() {
		t.Fatal("missing wired finality")
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000}
	req := tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "root", IdempotencyKey: "root", InferenceAllocation: &a}
	root, err := e.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	childReq := tasks.SpawnRequest{Identity: req.Identity, Kind: tasks.KindBackground, Query: "accepted helper", ParentTaskID: &root.ID, IdempotencyKey: "child"}
	child, err := e.Spawn(ctx, childReq)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.MarkRunning(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.MarkRunning(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	q := req.Identity
	q.RunID = string(root.ID)
	if err = e.MarkComplete(ctx, root.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	if err = mgr.Reserve(ctx, q, a, "helper-after-root", 100); err != nil {
		t.Fatal("accepted background helper cut off", err)
	}
	if err = e.MarkComplete(ctx, child.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := mgr.Snapshot(ctx, q, a)
	if err != nil || !snapshot.Closed || snapshot.ReservedTokens != 100 {
		t.Fatalf("last member did not close retained liability: %+v %v", snapshot, err)
	}
	if err = mgr.Reserve(ctx, q, a, "late-unregistered-helper", 1); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("late helper: %v", err)
	}
	if err = mgr.Settle(ctx, q, a, "helper-after-root", new(int64(7)), false); err != nil {
		t.Fatal(err)
	}
	snapshot, err = mgr.Snapshot(ctx, q, a)
	if err != nil || !snapshot.Closed || snapshot.ReservedTokens != 0 || snapshot.SettledTokens != 7 {
		t.Fatalf("final settlement: %+v %v", snapshot, err)
	}
	childReq.IdempotencyKey = "another-child"
	if _, err = e.Spawn(ctx, childReq); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("new child reopened root: %v", err)
	}
	childReq.IdempotencyKey = "child"
	replay, err := e.Spawn(ctx, childReq)
	if err != nil || replay.ID != child.ID || !replay.Reused {
		t.Fatalf("closed child replay: %+v %v", replay, err)
	}
}

func TestEngine_AllocationFinalitySurvivesFailedTerminalWrite(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	backend := &memBackend{}
	mgr := allocation.New(st)
	e, err := engine.New(bus, auditpatterns.New(), backend, engine.WithAllocations(mgr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
	q := identity.Quadruple{Identity: id}
	root, err := e.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, Query: "root", InferenceAllocation: &a})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.MarkRunning(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	backend.saveErr = errors.New("synthetic terminal persistence failure")
	if err = e.MarkComplete(ctx, root.ID, tasks.TaskResult{}); err == nil {
		t.Fatal("expected failed terminal persistence")
	}
	q.RunID = string(root.ID)
	if err = mgr.Reserve(ctx, q, a, "late", 1); !errors.Is(err, llm.ErrAllocationClosed) {
		t.Fatalf("terminal failure reopened calls: %v", err)
	}
	got, err := e.Get(ctx, root.ID)
	if err != nil || got.Status != tasks.StatusRunning {
		t.Fatalf("terminal retry was lost: %+v %v", got, err)
	}
	backend.saveErr = nil
	if err = e.MarkComplete(ctx, root.ID, tasks.TaskResult{}); err != nil {
		t.Fatal("terminal retry", err)
	}
}

func TestEngine_AllocationFinalityRecoversLegacyTerminalFamily(t *testing.T) {
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	q := identity.Quadruple{Identity: id, RunID: "root"}
	a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
	mgr := allocation.New(st)
	if err = mgr.Reserve(t.Context(), q, a, "crashed", 20); err != nil {
		t.Fatal(err)
	}
	root := &tasks.Task{ID: "root", Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Status: tasks.StatusComplete, InferenceAllocation: &a}
	child := &tasks.Task{ID: "child", Identity: root.Identity, ParentTaskID: &root.ID, AllocationTaskID: "root", Kind: tasks.KindBackground, Status: tasks.StatusRunning, InferenceAllocation: &a}
	e, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: engine.Snapshot{Tasks: []engine.TaskRecord{{Task: root}, {Task: child}}}}, engine.WithAllocations(mgr))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	if count, err := e.RecoverInterruptedTasks(t.Context()); err != nil || count != 1 {
		t.Fatalf("recovery: %d %v", count, err)
	}
	snapshot, err := mgr.Snapshot(t.Context(), q, a)
	if err != nil || !snapshot.Closed || snapshot.ReservedTokens != 20 {
		t.Fatalf("recovery finality lost liability: %+v %v", snapshot, err)
	}
	if _, err = e.RecoverInterruptedTasks(t.Context()); err != nil {
		t.Fatal("repeat recovery", err)
	}
}

func TestEngine_AllocationFinalityReconcilesTerminalButNotPausedLegacyFamily(t *testing.T) {
	for _, childStatus := range []tasks.TaskStatus{tasks.StatusComplete, tasks.StatusPaused} {
		t.Run(string(childStatus), func(t *testing.T) {
			st, err := inmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close(context.Background()) }()
			bus := mkBus(t)
			defer func() { _ = bus.Close(context.Background()) }()
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "root"}
			a := llm.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 100}
			root := &tasks.Task{ID: "root", Identity: identity.Quadruple{Identity: q.Identity}, Kind: tasks.KindForeground, Status: tasks.StatusComplete, InferenceAllocation: &a}
			child := &tasks.Task{ID: "child", Identity: root.Identity, ParentTaskID: &root.ID, AllocationTaskID: "root", Kind: tasks.KindBackground, Status: childStatus, InferenceAllocation: &a}
			mgr := allocation.New(st)
			e, err := engine.New(bus, auditpatterns.New(), &memBackend{seed: engine.Snapshot{Tasks: []engine.TaskRecord{{Task: root}, {Task: child}}}}, engine.WithAllocations(mgr))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = e.Close(context.Background()) }()
			if count, err := e.RecoverInterruptedTasks(t.Context()); err != nil || count != 0 {
				t.Fatalf("recovery: %d %v", count, err)
			}
			snapshot, err := mgr.Snapshot(t.Context(), q, a)
			if err != nil || snapshot.Closed != (childStatus == tasks.StatusComplete) {
				t.Fatalf("legacy family closure: %+v %v", snapshot, err)
			}
		})
	}
}
