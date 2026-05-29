// cmd/harbor/cmd_dev_spawn_await_test.go — tests for the dev
// ToolExecutor's SpawnTask + AwaitTask dispatch (Phase 107e — D-170).
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real
// inprocess TaskRegistry over an inmem StateStore + a real inmem
// ArtifactStore. The executor is the production devToolExecutor. The
// end-to-end test additionally wires a real per-task RunLoop driver (with
// driveBackground=true) so a spawned background task is actually driven
// to completion and then joined by AwaitTask.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// newSpawnAwaitTestExecutor builds a devToolExecutor over the supplied
// registry with an empty catalog + a real inmem artifact store.
func newSpawnAwaitTestExecutor(t *testing.T, reg tasks.TaskRegistry, heavyThreshold, maxDepth int) *devToolExecutor {
	t.Helper()
	artStore, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	return newDevToolExecutor(tools.NewCatalog(), artStore, reg, heavyThreshold, maxDepth, nil)
}

// spawnAwaitIDCtx returns a ctx carrying the shared test identity triple.
func spawnAwaitIDCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// rcFor builds a RunContext whose identity is the shared test triple and
// whose RunID (= the current task id at the dev layer) is `runID`.
func rcFor(runID tasks.TaskID) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{Identity: runLoopDriverTestID, RunID: string(runID)},
	}
}

// TestSpawnTask_NonRetain_SpawnsBackgroundTask — AC-2/AC-3: a
// non-retain-turn SpawnTask creates a KindBackground task under the run's
// triple and returns {task_id, kind, status:"spawned"} immediately.
func TestSpawnTask_NonRetain_SpawnsBackgroundTask(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	raw, llmObs, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Description: "sub goal", Query: "do the sub goal"},
	})
	if err != nil {
		t.Fatalf("ExecuteDecision(SpawnTask): %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("raw observation type = %T, want map[string]any", raw)
	}
	if _, ok := llmObs.(map[string]any); !ok {
		t.Fatalf("llmObs type = %T, want map[string]any", llmObs)
	}
	taskID, _ := m["task_id"].(string)
	if taskID == "" {
		t.Fatalf("observation missing task_id: %v", m)
	}
	if m["status"] != "spawned" {
		t.Errorf("status = %v, want spawned", m["status"])
	}

	// The task exists in the registry under the run's triple, KindBackground.
	task, gErr := reg.Get(spawnAwaitIDCtx(t), tasks.TaskID(taskID))
	if gErr != nil {
		t.Fatalf("reg.Get(%q): %v", taskID, gErr)
	}
	if task.Kind != tasks.KindBackground {
		t.Errorf("spawned kind = %q, want background", task.Kind)
	}
	if task.Query != "do the sub goal" {
		t.Errorf("spawned query = %q, want %q", task.Query, "do the sub goal")
	}
	if task.Identity.Identity != runLoopDriverTestID {
		t.Errorf("spawned identity = %+v, want %+v", task.Identity.Identity, runLoopDriverTestID)
	}
	if task.ParentTaskID != nil {
		t.Errorf("root spawn (empty RunID) should have nil ParentTaskID, got %v", *task.ParentTaskID)
	}
}

// TestSpawnTask_NilRegistry_Unsupported — with no TaskRegistry wired the
// dispatch fails loud with ErrDecisionShapeUnsupported (never a panic /
// silent no-op).
func TestSpawnTask_NilRegistry_Unsupported(t *testing.T) {
	exec := newSpawnAwaitTestExecutor(t, nil, 32*1024, 4)
	_, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "x"},
	})
	if !errors.Is(err, steering.ErrDecisionShapeUnsupported) {
		t.Fatalf("err = %v, want ErrDecisionShapeUnsupported", err)
	}
}

// TestAwaitTask_EmptyTaskID_Errors — AwaitTask with an empty TaskID fails
// loud (the projector rejects this at emission; the executor re-asserts).
func TestAwaitTask_EmptyTaskID_Errors(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	_, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: ""})
	if err == nil {
		t.Fatal("AwaitTask(empty) returned nil error, want failure")
	}
}

// TestAwaitTask_UnknownTask_Errors — AwaitTask on a non-existent task id
// surfaces the registry's not-found error (no hang): Get fails on the
// first poll iteration and awaitTerminal returns immediately. This also
// covers the cross-session reject path (AC-11) — Get rejects a task not
// visible to the ctx identity exactly as it does a missing one.
func TestAwaitTask_UnknownTask_Errors(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := exec.ExecuteDecision(ctx, rcFor(""), planner.AwaitTask{TaskID: "no-such-task"})
	if err == nil {
		t.Fatal("AwaitTask(unknown) returned nil error, want not-found failure")
	}
}

// markTaskComplete spawns a task and drives it Pending → Running →
// Complete with the given answer-envelope Value, returning its id.
func markTaskComplete(t *testing.T, reg tasks.TaskRegistry, value []byte) tasks.TaskID {
	t.Helper()
	ctx := spawnAwaitIDCtx(t)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:     tasks.KindBackground,
		Query:    "pre-completed",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: value}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	return h.ID
}

// TestAwaitTask_CompletedTask_ReturnsOutcome — AC-5: awaiting a terminal
// task returns its answer-envelope as the observation `result`, parsed.
func TestAwaitTask_CompletedTask_ReturnsOutcome(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	envelope := []byte(`{"answer":"the sub answer","finish_reason":"goal","tool_calls_seen":2}`)
	id := markTaskComplete(t, reg, envelope)

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: id})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("raw type = %T, want map", raw)
	}
	if m["status"] != string(tasks.StatusComplete) {
		t.Errorf("status = %v, want complete", m["status"])
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want parsed map", m["result"])
	}
	if result["answer"] != "the sub answer" {
		t.Errorf("result.answer = %v, want %q", result["answer"], "the sub answer")
	}
}

// TestAwaitTask_HeavyResult_Projected — AC-6: a heavy awaited result is
// promoted to an artifact-stub llmObservation while the raw observation
// keeps the full value, so the LLM-edge ErrContextLeak guard is not
// tripped.
func TestAwaitTask_HeavyResult_Projected(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	// Tiny heavy threshold so the envelope easily exceeds it.
	exec := newSpawnAwaitTestExecutor(t, reg, 256, 4)

	big := strings.Repeat("x", 4096)
	envelope, mErr := json.Marshal(map[string]any{"answer": big, "finish_reason": "goal"})
	if mErr != nil {
		t.Fatalf("marshal envelope: %v", mErr)
	}
	id := markTaskComplete(t, reg, envelope)

	raw, llmObs, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: id})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	// Raw keeps the full value.
	rawEnc, _ := json.Marshal(raw)
	if len(rawEnc) < 4096 {
		t.Errorf("raw observation looks truncated (%d bytes); should carry the full value", len(rawEnc))
	}
	// llmObs is the projected stub (under the heavy threshold's intent).
	stub, ok := llmObs.(map[string]any)
	if !ok {
		t.Fatalf("llmObs type = %T, want stub map", llmObs)
	}
	if stub["truncated"] != true {
		t.Errorf("llmObs not projected to an artifact stub: %v", stub)
	}
}

// TestSpawnTask_DepthCap — AC-8: with cap=1, a spawn whose parent chain is
// already at depth 1 is rejected loudly; a spawn at depth 0 succeeds.
func TestSpawnTask_DepthCap(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 1)

	// Root spawn (RunID empty → ParentTaskID nil, depth 0). Allowed.
	raw1, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "root"},
	})
	if err != nil {
		t.Fatalf("root spawn rejected: %v", err)
	}
	t1 := tasks.TaskID(raw1.(map[string]any)["task_id"].(string))

	// Spawn whose parent is the root (child depth 1 ≤ cap 1). Allowed.
	raw2, _, err := exec.ExecuteDecision(context.Background(), rcFor(t1), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "depth-1 child"},
	})
	if err != nil {
		t.Fatalf("depth-1 spawn rejected (cap=1 allows child depth 1): %v", err)
	}
	t2 := tasks.TaskID(raw2.(map[string]any)["task_id"].(string))

	// Spawn whose parent (t2) is at depth 1 → child depth 2 > cap 1. Rejected.
	_, _, err = exec.ExecuteDecision(context.Background(), rcFor(t2), planner.SpawnTask{
		Spec: planner.SpawnSpec{Query: "depth-2 child"},
	})
	if err == nil {
		t.Fatal("depth-2 spawn was accepted; want rejection above absolute_max_spawn_depth")
	}
	if !strings.Contains(err.Error(), "absolute_max_spawn_depth") {
		t.Errorf("rejection error = %q, want it to name absolute_max_spawn_depth", err.Error())
	}
}

// TestSpawnThenAwait_BackgroundDrivenEndToEnd — AC-15: a non-retain
// SpawnTask creates a background task; a real per-task driver (with
// driveBackground=true) picks it up and drives it to completion; AwaitTask
// then joins it and receives the child's answer. Identity propagates to
// the spawned run. Runs under -race.
func TestSpawnThenAwait_BackgroundDrivenEndToEnd(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	// The background sub-run's planner finishes immediately with an answer.
	p := &driverTestPlanner{
		finishGoalImmediately: true,
		finishPayload:         map[string]any{"answer": "child done"},
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:             bus,
		runLoop:         rl,
		planner:         p,
		tasks:           reg,
		driveBackground: true,
		executor:        exec,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = driver.Close(closeCtx)
	}()

	// Parent emits SpawnTask (non-retain).
	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Query: "child goal"},
	})
	if err != nil {
		t.Fatalf("ExecuteDecision(SpawnTask): %v", err)
	}
	childID := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

	// The driver drives the background task to completion.
	if got := waitForTaskStatus(t, reg, childID, tasks.StatusComplete, 5*time.Second); got != tasks.StatusComplete {
		t.Fatalf("background task status = %q, want complete", got)
	}

	// Identity propagated to the spawned run.
	task, gErr := reg.Get(spawnAwaitIDCtx(t), childID)
	if gErr != nil {
		t.Fatalf("reg.Get(child): %v", gErr)
	}
	if task.Identity.Identity != runLoopDriverTestID {
		t.Errorf("child identity = %+v, want %+v", task.Identity.Identity, runLoopDriverTestID)
	}

	// Parent joins via AwaitTask and reads the child's answer.
	awaitRaw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: childID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	result, ok := awaitRaw.(map[string]any)["result"].(map[string]any)
	if !ok {
		t.Fatalf("await observation missing parsed result: %v", awaitRaw)
	}
	if result["answer"] != "child done" {
		t.Errorf("awaited answer = %v, want %q", result["answer"], "child done")
	}
}

// TestSpawnTask_RetainTurn_BlocksAndReturnsOutcome — AC-4: a retain-turn
// SpawnTask spawns AND joins in one decision — the executor blocks until
// the driver drives the spawned task terminal, then returns its outcome
// directly (no separate AwaitTask). Runs under -race.
func TestSpawnTask_RetainTurn_BlocksAndReturnsOutcome(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)
	p := &driverTestPlanner{finishGoalImmediately: true, finishPayload: map[string]any{"answer": "retained answer"}}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:             bus,
		runLoop:         rl,
		planner:         p,
		tasks:           reg,
		driveBackground: true,
		executor:        exec,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = driver.Close(closeCtx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _, err := exec.ExecuteDecision(ctx, rcFor(""), planner.SpawnTask{
		Kind: tasks.KindBackground,
		Spec: planner.SpawnSpec{Query: "retained sub-goal", RetainTurn: true},
	})
	if err != nil {
		t.Fatalf("ExecuteDecision(SpawnTask retain-turn): %v", err)
	}
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("raw type = %T, want terminal outcome map", raw)
	}
	if m["status"] != string(tasks.StatusComplete) {
		t.Errorf("retain-turn status = %v, want complete (the executor should have blocked until terminal)", m["status"])
	}
	result, ok := m["result"].(map[string]any)
	if !ok || result["answer"] != "retained answer" {
		t.Errorf("retain-turn result = %v, want answer=%q", m["result"], "retained answer")
	}
}

// TestSpawnThenAwait_FailedChild — AC-15 failure mode: a child that ends
// non-goal (driver MarkFailed) surfaces its error on the parent's await
// observation rather than a success result.
func TestSpawnThenAwait_FailedChild(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	// Spawn + drive to Failed directly (no planner needed for this shape).
	ctx := spawnAwaitIDCtx(t)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:     tasks.KindBackground,
		Query:    "doomed",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "boom", Message: "child failed"}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	raw, _, err := exec.ExecuteDecision(context.Background(), rcFor(""), planner.AwaitTask{TaskID: h.ID})
	if err != nil {
		t.Fatalf("ExecuteDecision(AwaitTask): %v", err)
	}
	m := raw.(map[string]any)
	if m["status"] != string(tasks.StatusFailed) {
		t.Errorf("status = %v, want failed", m["status"])
	}
	errObj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("await observation missing error block: %v", m)
	}
	if errObj["code"] != "boom" {
		t.Errorf("error.code = %v, want boom", errObj["code"])
	}
}

// TestSpawnAwait_ConcurrentReuse — AC-16: N concurrent spawn+await cycles
// against ONE shared executor + ONE shared registry, each with its own
// identity, under -race. Asserts no cross-talk (each await sees its own
// child's answer) and no goroutine leak after all cycles return.
func TestSpawnAwait_ConcurrentReuse(t *testing.T) {
	const n = 100

	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	exec := newSpawnAwaitTestExecutor(t, reg, 32*1024, 4)

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-" + itoa(i),
				UserID:    "user-" + itoa(i),
				SessionID: "session-" + itoa(i),
			}
			rc := planner.RunContext{Quadruple: identity.Quadruple{Identity: id}}
			idCtx, wErr := identity.With(context.Background(), id)
			if wErr != nil {
				errCh <- wErr
				return
			}

			// Spawn a background task under this goroutine's own identity.
			raw, _, sErr := exec.ExecuteDecision(context.Background(), rc, planner.SpawnTask{
				Kind: tasks.KindBackground,
				Spec: planner.SpawnSpec{Query: "q-" + itoa(i)},
			})
			if sErr != nil {
				errCh <- sErr
				return
			}
			childID := tasks.TaskID(raw.(map[string]any)["task_id"].(string))

			// Drive it terminal directly (no driver) with a per-goroutine answer.
			if mErr := reg.MarkRunning(idCtx, childID); mErr != nil {
				errCh <- mErr
				return
			}
			envelope, _ := json.Marshal(map[string]any{"answer": "ans-" + itoa(i)})
			if mErr := reg.MarkComplete(idCtx, childID, tasks.TaskResult{Value: envelope}); mErr != nil {
				errCh <- mErr
				return
			}

			// Await + assert no cross-talk.
			awaitRaw, _, aErr := exec.ExecuteDecision(context.Background(), rc, planner.AwaitTask{TaskID: childID})
			if aErr != nil {
				errCh <- aErr
				return
			}
			result := awaitRaw.(map[string]any)["result"].(map[string]any)
			if result["answer"] != "ans-"+itoa(i) {
				errCh <- errors.New("cross-talk: await saw " + asString(result["answer"]) + " want ans-" + itoa(i))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent spawn/await: %v", err)
	}

	// Goroutine-leak check: the executor's await pollers stop on return
	// (ticker stopped via defer). Allow a small settle window.
	for range 20 {
		if runtime.NumGoroutine() <= baseline+5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

// asString renders an arbitrary value for an error message without
// pulling in fmt for one call site.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "<non-string>"
}
