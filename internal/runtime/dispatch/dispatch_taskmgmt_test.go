// internal/runtime/dispatch/dispatch_taskmgmt_test.go — _task_status /
// _cancel_task dispatch tests: descendant-scoping, list-all vs
// explicit-ids, atomic partial-scope rejection, idempotent cancel,
// cross-run isolation under one session, and D-025 concurrent reuse.
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real
// inprocess TaskRegistry over an inmem StateStore + a real inmem
// ArtifactStore + a real inmem EventBus (the shared spawn/await test
// fixtures). The dispatch package IS the planner→registry wiring
// boundary (AGENTS.md §17.2 in-package carve-out), so the cross-run
// isolation test lives here against the production driver.

package dispatch

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// spawnChildUnder spawns a background task whose ParentTaskID is
// callerID (the run's own task), returning the child's id. This is how a
// run's own descendant tree is built at the dispatch layer.
func spawnChildUnder(t *testing.T, exec steering.ToolExecutor, cat tools.ToolCatalog, callerID tasks.TaskID, query string) tasks.TaskID {
	t.Helper()
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(callerID, cat), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Query: query, Description: query},
	})
	if err != nil {
		t.Fatalf("spawn child under %q: %v", callerID, err)
	}
	return tasks.TaskID(raw.(map[string]any)["task_id"].(string))
}

// TestExecutor_TaskStatus_NilRegistry_Unsupported — with no registry
// wired, TaskStatusQuery fails loud (never panic / silent no-op).
func TestExecutor_TaskStatus_NilRegistry_Unsupported(t *testing.T) {
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, nil, 32*1024, 4, cat)
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.TaskStatusQuery{})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("nil-registry TaskStatusQuery err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_CancelTask_NilRegistry_Unsupported — same for CancelTask.
func TestExecutor_CancelTask_NilRegistry_Unsupported(t *testing.T) {
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, nil, 32*1024, 4, cat)
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.CancelTask{TaskID: "x"})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("nil-registry CancelTask err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestExecutor_TaskStatus_ListAll_ReturnsOwnDescendants — AC-9: a
// nil-TaskIDs query walks the caller's own descendant subtree and
// returns {task_id, status, description, group_id} rows, transitively.
func TestExecutor_TaskStatus_ListAll_ReturnsOwnDescendants(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	// runA spawns two direct children; one of them spawns a grandchild.
	c1 := spawnChildUnder(t, exec, cat, "runA", "child-1")
	c2 := spawnChildUnder(t, exec, cat, "runA", "child-2")
	gc := spawnChildUnder(t, exec, cat, c1, "grandchild")

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.TaskStatusQuery{})
	if err != nil {
		t.Fatalf("TaskStatusQuery(list-all): %v", err)
	}
	rows, _ := raw.(map[string]any)["tasks"].([]map[string]any)
	got := map[tasks.TaskID]bool{}
	for _, r := range rows {
		got[tasks.TaskID(r["task_id"].(string))] = true
		if _, ok := r["status"]; !ok {
			t.Errorf("row %v missing status", r)
		}
		if _, ok := r["group_id"]; !ok {
			t.Errorf("row %v missing group_id", r)
		}
	}
	for _, want := range []tasks.TaskID{c1, c2, gc} {
		if !got[want] {
			t.Errorf("list-all missing own descendant %q (got %v)", want, got)
		}
	}
	if got["runA"] {
		t.Errorf("list-all must not include the caller's own task")
	}
}

// TestExecutor_TaskStatus_ExplicitIDs_Descendant — AC-9: an explicit
// own-descendant id returns its row with the persisted description.
func TestExecutor_TaskStatus_ExplicitIDs_Descendant(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	c1 := spawnChildUnder(t, exec, cat, "runA", "child-1")
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.TaskStatusQuery{TaskIDs: []tasks.TaskID{c1}})
	if err != nil {
		t.Fatalf("TaskStatusQuery(explicit): %v", err)
	}
	rows := raw.(map[string]any)["tasks"].([]map[string]any)
	if len(rows) != 1 || rows[0]["task_id"] != string(c1) {
		t.Fatalf("rows = %v, want single row for %q", rows, c1)
	}
	if rows[0]["description"] != "child-1" {
		t.Errorf("description = %v, want child-1", rows[0]["description"])
	}
}

// TestExecutor_TaskStatus_OutOfScopeID_AtomicReject — AC-9: one
// out-of-scope id fails the WHOLE call loud with ErrTaskNotOwnDescendant
// (atomic — no partial result), even alongside a valid own-descendant.
func TestExecutor_TaskStatus_OutOfScopeID_AtomicReject(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	own := spawnChildUnder(t, exec, cat, "runA", "own")
	// A task owned by a DIFFERENT run in the same session.
	sibling := spawnChildUnder(t, exec, cat, "runB", "sibling")

	_, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.TaskStatusQuery{
		TaskIDs: []tasks.TaskID{own, sibling},
	})
	if !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("mixed-scope query err = %v, want ErrTaskNotOwnDescendant", err)
	}
}

// TestExecutor_CancelTask_OwnDescendant — AC-10: cancelling an own
// descendant returns {task_id, cancelled:true} and the task transitions.
func TestExecutor_CancelTask_OwnDescendant(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	c1 := spawnChildUnder(t, exec, cat, "runA", "doomed")
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.CancelTask{TaskID: c1, Reason: "abandon"})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	m := raw.(map[string]any)
	if m["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", m["cancelled"])
	}
	task, gErr := reg.Get(spawnAwaitIDCtx(t), c1)
	if gErr != nil {
		t.Fatalf("Get: %v", gErr)
	}
	if task.Status != tasks.StatusCancelled {
		t.Errorf("status = %q, want cancelled", task.Status)
	}
}

// TestExecutor_CancelTask_IsolateDescendant_Succeeds — AC-14(c): a run's
// own _cancel_task on a descendant it spawned with propagate_on_cancel:
// isolate still succeeds — a direct agent cancel of an own descendant is
// never gated on that descendant's own isolate flag (isolate detaches
// only from an ANCESTOR's cascade).
func TestExecutor_CancelTask_IsolateDescendant_Succeeds(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Query: "detached", PropagateOnCancel: tasks.PropagateIsolate},
	})
	if err != nil {
		t.Fatalf("spawn isolate child: %v", err)
	}
	child := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

	// Confirm the isolate policy was threaded onto the persisted task.
	task, gErr := reg.Get(spawnAwaitIDCtx(t), child)
	if gErr != nil {
		t.Fatal(gErr)
	}
	if task.PropagateOnCancel != tasks.PropagateIsolate {
		t.Fatalf("spawned task PropagateOnCancel = %q, want isolate", task.PropagateOnCancel)
	}

	craw, _, cErr := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.CancelTask{TaskID: child, Reason: "own direct cancel"})
	if cErr != nil {
		t.Fatalf("CancelTask isolate descendant: %v", cErr)
	}
	if craw.(map[string]any)["cancelled"] != true {
		t.Errorf("cancelled = %v, want true (isolate never blocks a direct own cancel)", craw.(map[string]any)["cancelled"])
	}
}

// TestExecutor_CancelTask_AlreadyTerminal_Idempotent — AC-10: cancelling
// an already-terminal descendant returns cancelled:false, not an error.
func TestExecutor_CancelTask_AlreadyTerminal_Idempotent(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	c1 := spawnChildUnder(t, exec, cat, "runA", "child")
	ctx := spawnAwaitIDCtx(t)
	if err := reg.MarkRunning(ctx, c1); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkComplete(ctx, c1, tasks.TaskResult{Value: []byte(`"x"`)}); err != nil {
		t.Fatal(err)
	}
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.CancelTask{TaskID: c1})
	if err != nil {
		t.Fatalf("CancelTask(terminal): %v", err)
	}
	if raw.(map[string]any)["cancelled"] != false {
		t.Errorf("cancelled = %v, want false on already-terminal target", raw.(map[string]any)["cancelled"])
	}
}

// TestExecutor_CancelTask_Self_NotDescendant — AC-8: a run targeting its
// own task id is rejected (self is not a descendant).
func TestExecutor_CancelTask_Self_NotDescendant(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	_, _, err := exec.ExecuteDecision(context.Background(), rcFor("runA", cat), planner.CancelTask{TaskID: "runA"})
	if !errors.Is(err, ErrTaskNotOwnDescendant) {
		t.Fatalf("self-cancel err = %v, want ErrTaskNotOwnDescendant", err)
	}
}

// TestExecutor_TaskMgmt_CrossRunIsolation — AC-13: two sibling runs in
// the SAME (tenant, user, session). Run A spawns a background task; run B
// (an independent run in the same session) calls _task_status and
// _cancel_task naming run A's task. Both fail loud with
// ErrTaskNotOwnDescendant; run A's task is untouched. A third assertion
// confirms the registry's EXISTING cross-session identity check still
// independently rejects an out-of-session id.
func TestExecutor_TaskMgmt_CrossRunIsolation(t *testing.T) {
	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	// Run A spawns a task under its own run (same session identity).
	aTask := spawnChildUnder(t, exec, cat, "runA", "run-A-work")

	// Run B (same session, different run) cannot status run A's task.
	_, _, statusErr := exec.ExecuteDecision(context.Background(), rcFor("runB", cat), planner.TaskStatusQuery{
		TaskIDs: []tasks.TaskID{aTask},
	})
	if !errors.Is(statusErr, ErrTaskNotOwnDescendant) {
		t.Fatalf("run B status of run A's task err = %v, want ErrTaskNotOwnDescendant", statusErr)
	}

	// Run B cannot cancel run A's task.
	_, _, cancelErr := exec.ExecuteDecision(context.Background(), rcFor("runB", cat), planner.CancelTask{TaskID: aTask, Reason: "not mine"})
	if !errors.Is(cancelErr, ErrTaskNotOwnDescendant) {
		t.Fatalf("run B cancel of run A's task err = %v, want ErrTaskNotOwnDescendant", cancelErr)
	}

	// Run A's task is untouched — never cancelled by run B.
	task, gErr := reg.Get(spawnAwaitIDCtx(t), aTask)
	if gErr != nil {
		t.Fatalf("Get run A's task: %v", gErr)
	}
	if task.Status == tasks.StatusCancelled {
		t.Fatalf("run A's task was cancelled by run B — isolation violated")
	}

	// The registry's EXISTING cross-session identity check is independent
	// and still rejects an out-of-session id: a task spawned under a
	// DIFFERENT session is not even visible to run A's session identity
	// (AC-8's descendant scope is additive to this, never a replacement).
	otherID := identity.Identity{TenantID: "tenant-other", UserID: "user-other", SessionID: "session-other"}
	otherCtx, wErr := identity.With(context.Background(), otherID)
	if wErr != nil {
		t.Fatal(wErr)
	}
	otherHandle, sErr := reg.Spawn(otherCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: otherID},
		Kind:     tasks.KindBackground,
		Query:    "other-session-work",
	})
	if sErr != nil {
		t.Fatalf("spawn other-session task: %v", sErr)
	}
	if _, err := reg.Get(spawnAwaitIDCtx(t), otherHandle.ID); !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("registry Get of out-of-session id err = %v, want ErrNotFound (independent identity check)", err)
	}
}

// TestExecutor_TaskMgmt_ConcurrentReuse — AC-16 / D-025: N=100
// concurrent _task_status + _cancel_task cycles against ONE shared
// executor + ONE shared registry, each with its own identity, under
// -race. Asserts no cross-talk (each cycle sees only its own child) and
// no goroutine leak after all cycles return.
func TestExecutor_TaskMgmt_ConcurrentReuse(t *testing.T) {
	const n = 100

	bus := mkSpawnAwaitTestBus(t)
	reg := mkSpawnAwaitTestTaskRegistry(t, bus)
	cat := tools.NewCatalog()
	exec := newSpawnAwaitExecutor(t, reg, 32*1024, 4, cat)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + strconv.Itoa(i),
				UserID:    "user-" + strconv.Itoa(i),
				SessionID: "session-" + strconv.Itoa(i),
			}
			runID := tasks.TaskID("run-" + strconv.Itoa(i))
			rc := planner.RunContext{
				Quadruple: identity.Quadruple{Identity: id, RunID: string(runID)},
				Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{
					TenantID:  id.TenantID,
					UserID:    id.UserID,
					SessionID: id.SessionID,
				}),
			}

			// Spawn a child under this goroutine's own run.
			raw, _, sErr := exec.ExecuteDecision(context.Background(), rc, planner.SpawnTask{
				Kind: tasks.KindBackground,
				Spec: planner.SpawnSpec{Query: "q-" + strconv.Itoa(i)},
			})
			if sErr != nil {
				errCh <- sErr
				return
			}
			child := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

			// Status the run's own subtree; expect exactly this one child.
			sraw, _, stErr := exec.ExecuteDecision(context.Background(), rc, planner.TaskStatusQuery{})
			if stErr != nil {
				errCh <- stErr
				return
			}
			rows := sraw.(map[string]any)["tasks"].([]map[string]any)
			if len(rows) != 1 || rows[0]["task_id"] != string(child) {
				errCh <- errors.New("context bleed: status returned another run's tasks")
				return
			}

			// Cancel the run's own child.
			craw, _, cErr := exec.ExecuteDecision(context.Background(), rc, planner.CancelTask{TaskID: child})
			if cErr != nil {
				errCh <- cErr
				return
			}
			if craw.(map[string]any)["cancelled"] != true {
				errCh <- errors.New("own-child cancel returned cancelled:false")
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent cycle: %v", err)
		}
	}

	// Goroutine baseline restored (allow slack for the registry's own
	// background workers to settle).
	if delta := runtime.NumGoroutine() - baseline; delta > 8 {
		t.Errorf("goroutine leak: delta=%d after %d cycles", delta, n)
	}
}
