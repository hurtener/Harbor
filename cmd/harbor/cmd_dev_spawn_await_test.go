// cmd/harbor/cmd_dev_spawn_await_test.go — driver-integrated tests for
// SpawnTask + AwaitTask dispatch (Phase 107e — D-170) against the
// promoted production executor (Phase 110a — D-194,
// `internal/runtime/dispatch`).
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3): a real
// inprocess TaskRegistry over an inmem StateStore + a real inmem
// ArtifactStore + the real promoted `dispatch.NewToolExecutor`. These
// two tests additionally wire a real per-task RunLoop driver (with
// driveBackground=true) so a spawned background task is actually
// driven to completion through the cmd-side driver wiring — proving
// the converted thin-caller wiring drives spawn/await end-to-end. The
// executor's pure dispatch behaviour (depth caps, terminal polling,
// D-026 projection, concurrent reuse) is tested in
// internal/runtime/dispatch/dispatch_test.go, where the code now lives.

package main

import (
	"context"
	"testing"
	"time"

	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// newSpawnAwaitTestExecutor builds the promoted production executor
// over the supplied registry with an empty catalog + a real inmem
// artifact store.
func newSpawnAwaitTestExecutor(t *testing.T, reg tasks.TaskRegistry, heavyThreshold, maxDepth int) steering.ToolExecutor {
	t.Helper()
	artStore, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	return dispatch.NewToolExecutor(tools.NewCatalog(), artStore, reg,
		dispatch.WithHeavyThreshold(heavyThreshold),
		dispatch.WithMaxSpawnDepth(maxDepth))
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
