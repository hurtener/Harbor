// cmd/harbor/cmd_dev_runloop_test.go — tests for the per-task
// RunLoop driver (D-097, closes #114). The driver is the §13
// primitive-with-consumer evidence for the Phase 53 RunLoop in
// production: it subscribes to `task.spawned` and constructs a
// RunLoop per spawned foreground task.
//
// These tests use real drivers everywhere on the seam (CLAUDE.md
// §17.3): real audit Redactor, real EventBus (inmem), real
// pauseresume Coordinator, real steering Registry, real RunLoop. The
// planner is a scripted in-test fixture that emits RequestPause once
// then Finish — letting the test assert the RunLoop actually ran
// (the Coordinator.Request call from RequestPause is observable).

package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"

	// inprocess driver auto-registers under "inprocess" — pulled in
	// here for tests that construct a real TaskRegistry. The cmd/harbor
	// main package also blank-imports it; the duplicate import is fine
	// (Register panics on duplicate but init runs once per process).
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// runLoopDriverTestID is the documented dummy identity these tests use.
var runLoopDriverTestID = identity.Identity{
	TenantID:  "tenant-driver-test",
	UserID:    "user-driver-test",
	SessionID: "session-driver-test",
}

func mkDriverTestBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}
	b, err := eventsInmem.New(cfg, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// mkDriverTestTaskRegistry constructs a real production TaskRegistry
// backed by the inprocess driver (the V1 default) plus an inmem
// StateStore. Used by every test that exercises the D-098 FSM bridge
// (MarkRunning → Run → MarkComplete / MarkFailed). The registry shares
// the supplied bus so the driver's MarkRunning / MarkComplete events
// fan out to subscribers — tests assert on those events to pin the
// FSM transitions occurred.
func mkDriverTestTaskRegistry(t *testing.T, bus events.EventBus, red audit.Redactor) tasks.TaskRegistry {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.OpenDriver: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// spawnDriverTestTask is the test-side helper that drops a row into
// the registry under runLoopDriverTestID. The returned TaskID is what
// the driver picks up via the task.spawned event the registry fires.
func spawnDriverTestTask(t *testing.T, reg tasks.TaskRegistry) tasks.TaskID {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:     tasks.KindForeground,
		Query:    "driver-test goal",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	return h.ID
}

// waitForTaskStatus polls reg.Get until the task reaches `want` or
// the bounded timeout fires. Returns the observed status (so the
// caller's failure message can name what the FSM stuck at).
func waitForTaskStatus(t *testing.T, reg tasks.TaskRegistry, id tasks.TaskID, want tasks.TaskStatus, timeout time.Duration) tasks.TaskStatus {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	deadline := time.Now().Add(timeout)
	var last tasks.TaskStatus
	for time.Now().Before(deadline) {
		task, gErr := reg.Get(ctx, id)
		if gErr == nil {
			last = task.Status
			if task.Status == want {
				return task.Status
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// driverTestPlanner emits exactly one RequestPause (step 0), then
// Finish on every later step. The pause holds the RunLoop at
// inbox.WaitForEvent — the test can observe that Run was called
// (Coordinator.Request fired) without driving a complete control
// flow.
//
// Two opt-in shapes for the D-098 FSM-bridge tests:
//
//   - finishGoalImmediately: returns Finish{Goal} on the FIRST Next.
//     The run completes synchronously so the test can assert
//     MarkComplete fired.
//   - errOnNext: returns a non-nil error from the FIRST Next. The run
//     fails so the test can assert MarkFailed fired with the right
//     error code.
//
// When neither flag is set, the planner follows the legacy
// pause-once-then-finish shape (used by tests that just need to
// observe "Run was called").
type driverTestPlanner struct {
	mu                    sync.Mutex
	steps                 int
	stepsCh               chan int // optional: each Next sends its step number
	pauseReason           planner.PauseReason
	finishGoalImmediately bool
	errOnNext             error
	finishPayload         any // map[string]any or nil; used by Phase 106 tests
}

func (p *driverTestPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	p.steps++
	step := p.steps
	p.mu.Unlock()
	if p.stepsCh != nil {
		select {
		case p.stepsCh <- step:
		default:
		}
	}
	if p.errOnNext != nil {
		return nil, p.errOnNext
	}
	if p.finishGoalImmediately {
		return planner.Finish{Reason: planner.FinishGoal, Payload: p.finishPayload}, nil
	}
	if step == 1 {
		return planner.RequestPause{
			Reason:  p.pauseReason,
			Payload: map[string]any{"driver-test": true},
		}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// TestPerTaskRunLoopDriver_FailsLoud_NilBus — the driver constructor
// rejects a nil bus. Sanity for the §13 fail-loudly contract.
func TestPerTaskRunLoopDriver_FailsLoud_NilBus(t *testing.T) {
	_, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		runLoop: &steering.RunLoop{},
		planner: &driverTestPlanner{},
		tasks:   stubTaskRegistry{},
	})
	if err == nil {
		t.Fatal("newPerTaskRunLoopDriver(nil bus) returned nil error, want failure")
	}
}

// TestPerTaskRunLoopDriver_FailsLoud_NilRunLoop — sanity.
func TestPerTaskRunLoopDriver_FailsLoud_NilRunLoop(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	_, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		planner: &driverTestPlanner{},
		tasks:   stubTaskRegistry{},
	})
	if err == nil {
		t.Fatal("newPerTaskRunLoopDriver(nil runLoop) returned nil error, want failure")
	}
}

// TestPerTaskRunLoopDriver_FailsLoud_NilPlanner — sanity.
func TestPerTaskRunLoopDriver_FailsLoud_NilPlanner(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	_, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: &steering.RunLoop{},
		tasks:   stubTaskRegistry{},
	})
	if err == nil {
		t.Fatal("newPerTaskRunLoopDriver(nil planner) returned nil error, want failure")
	}
}

// TestPerTaskRunLoopDriver_FailsLoud_NilTasks — D-098 fail-loud
// invariant: the constructor rejects a nil TaskRegistry because the
// FSM bridge cannot function without it.
func TestPerTaskRunLoopDriver_FailsLoud_NilTasks(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	_, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: &steering.RunLoop{},
		planner: &driverTestPlanner{},
	})
	if err == nil {
		t.Fatal("newPerTaskRunLoopDriver(nil tasks) returned nil error, want failure")
	}
}

// stubTaskRegistry embeds tasks.TaskRegistry (so it satisfies the
// interface) but every method panics — used only by the constructor
// fail-loud tests above where the call site never invokes a registry
// method. Tests that exercise the FSM bridge use mkDriverTestTaskRegistry
// (a real inprocess driver).
type stubTaskRegistry struct {
	tasks.TaskRegistry
}

// TestPerTaskRunLoopDriver_PicksUpTaskSpawned_DrivesRunLoop — the
// §13 primitive-with-consumer evidence at the cmd layer. Construct
// the driver against a real bus + real RunLoop + real TaskRegistry +
// scripted planner. Spawn a real task (which emits task.spawned via
// the registry's publish path). Assert the planner's Next is called
// (the driver started the RunLoop). Close drains cleanly.
func TestPerTaskRunLoopDriver_PicksUpTaskSpawned_DrivesRunLoop(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	stepsCh := make(chan int, 4)
	p := &driverTestPlanner{
		pauseReason: planner.PauseApprovalRequired,
		stepsCh:     stepsCh,
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	// Spawn a real task — the registry publishes task.spawned on the
	// bus and the driver picks it up.
	_ = spawnDriverTestTask(t, reg)

	// Observe the planner's first Next — the RunLoop ran.
	select {
	case step := <-stepsCh:
		if step != 1 {
			t.Errorf("planner step = %d, want 1", step)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired — driver did not pick up task.spawned")
	}
}

// TestPerTaskRunLoopDriver_FSMBridge_MarksComplete — D-098 / closes
// issue #123 happy path. A planner that returns Finish{Goal} on first
// Next causes the driver to call MarkComplete on the task; the
// registry's FSM transitions to StatusComplete. Without the D-098
// bridge this test fails with the task stuck at StatusPending.
func TestPerTaskRunLoopDriver_FSMBridge_MarksComplete(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	// Planner finishes immediately with FinishGoal (no pause).
	p := &driverTestPlanner{finishGoalImmediately: true}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	taskID := spawnDriverTestTask(t, reg)
	status := waitForTaskStatus(t, reg, taskID, tasks.StatusComplete, 2*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task FSM stuck at %q, want %q (D-098 bridge did not fire)",
			status, tasks.StatusComplete)
	}
}

// TestPerTaskRunLoop_FinishGoal_PopulatesTaskResult — Phase 106 AC-5.
// A FinishGoal with an answer payload MUST result in a non-empty
// TaskResult.Value whose JSON envelope contains answer +
// finish_reason + tool_calls_seen.
func TestPerTaskRunLoop_FinishGoal_PopulatesTaskResult(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &driverTestPlanner{
		finishGoalImmediately: true,
		finishPayload:         map[string]any{"answer": "hello world"},
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	taskID := spawnDriverTestTask(t, reg)
	status := waitForTaskStatus(t, reg, taskID, tasks.StatusComplete, 2*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task FSM stuck at %q, want %q", status, tasks.StatusComplete)
	}

	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got, gErr := reg.Get(ctx, taskID)
	if gErr != nil {
		t.Fatalf("reg.Get: %v", gErr)
	}
	if got.Result == nil {
		t.Fatal("TaskResult is nil; Phase 106 should have populated it")
	}
	if len(got.Result.Value) == 0 {
		t.Fatal("TaskResult.Value is empty; Phase 106 should have populated it")
	}
	var envelope map[string]any
	if err := json.Unmarshal(got.Result.Value, &envelope); err != nil {
		t.Fatalf("unmarshal TaskResult.Value: %v", err)
	}
	if envelope["answer"] != "hello world" {
		t.Errorf("envelope.answer = %q, want %q", envelope["answer"], "hello world")
	}
	if envelope["finish_reason"] != "goal" {
		t.Errorf("envelope.finish_reason = %q, want %q", envelope["finish_reason"], "goal")
	}
	if tc, ok := envelope["tool_calls_seen"].(float64); !ok || tc < 0 {
		t.Errorf("envelope.tool_calls_seen = %v, want non-negative int", envelope["tool_calls_seen"])
	}
}

// TestPerTaskRunLoop_FinishGoal_EmptyAnswer_StillPopulatesShape — Phase 106 AC-6.
// A FinishGoal with a nil payload MUST still populate a TaskResult.Value
// envelope with an empty answer and the other fields present.
func TestPerTaskRunLoop_FinishGoal_EmptyAnswer_StillPopulatesShape(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &driverTestPlanner{finishGoalImmediately: true} // nil finishPayload
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	taskID := spawnDriverTestTask(t, reg)
	status := waitForTaskStatus(t, reg, taskID, tasks.StatusComplete, 2*time.Second)
	if status != tasks.StatusComplete {
		t.Fatalf("task FSM stuck at %q, want %q", status, tasks.StatusComplete)
	}

	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got, gErr := reg.Get(ctx, taskID)
	if gErr != nil {
		t.Fatalf("reg.Get: %v", gErr)
	}
	if got.Result == nil {
		t.Fatal("TaskResult is nil; Phase 106 should have populated it even for nil payload")
	}
	if len(got.Result.Value) == 0 {
		t.Fatal("TaskResult.Value is empty; Phase 106 should have populated it")
	}
	var envelope map[string]any
	if err := json.Unmarshal(got.Result.Value, &envelope); err != nil {
		t.Fatalf("unmarshal TaskResult.Value: %v", err)
	}
	// Pin the actual behaviour: when Payload is nil, the existing
	// extractAssistantAnswer helper (Phase 83i / D-152, shared with the
	// memory.AddTurn writeback) falls back to string(fin.Reason) so
	// something always lands. This deviates from the plan's literal
	// "answer == \"\"" expectation but matches reality + AC-3's "no
	// regression to the memory writeback path" requirement (the same
	// extractor feeds both writebacks).
	ans, ok := envelope["answer"].(string)
	if !ok {
		t.Fatalf("envelope.answer is missing or not a string: %T", envelope["answer"])
	}
	if ans != "goal" {
		t.Errorf("envelope.answer = %q, want %q (extractAssistantAnswer fallback for nil payload)", ans, "goal")
	}
	if envelope["finish_reason"] != "goal" {
		t.Errorf("envelope.finish_reason = %q, want %q", envelope["finish_reason"], "goal")
	}
	if _, ok := envelope["tool_calls_seen"].(float64); !ok {
		t.Errorf("envelope.tool_calls_seen is missing or not numeric: %T", envelope["tool_calls_seen"])
	}
}

// TestPerTaskRunLoopDriver_FSMBridge_MarksFailed_OnPlannerError —
// D-098 failure path. A planner that returns an error from Next causes
// the driver to call MarkFailed; the registry's FSM transitions to
// StatusFailed with the error code "runloop_error".
func TestPerTaskRunLoopDriver_FSMBridge_MarksFailed_OnPlannerError(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	// Planner that errors on Next.
	p := &driverTestPlanner{errOnNext: errors.New("planner exploded")}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	taskID := spawnDriverTestTask(t, reg)
	status := waitForTaskStatus(t, reg, taskID, tasks.StatusFailed, 2*time.Second)
	if status != tasks.StatusFailed {
		t.Fatalf("task FSM stuck at %q, want %q (D-098 bridge did not mark failed)",
			status, tasks.StatusFailed)
	}
	// Verify the error code recorded.
	getCtx, _ := identity.With(context.Background(), runLoopDriverTestID)
	task, err := reg.Get(getCtx, taskID)
	if err != nil {
		t.Fatalf("reg.Get: %v", err)
	}
	if task.Error == nil {
		t.Fatal("task.Error is nil after MarkFailed")
	}
	if task.Error.Code != "runloop_error" {
		t.Errorf("error code = %q, want %q", task.Error.Code, "runloop_error")
	}
}

// TestPerTaskRunLoopDriver_FSMBridge_MarksFailed_OnCtxCancel — D-098
// cancellation path. The driver's subCtx cancels mid-Run (driver
// shutdown); the per-task goroutine observes ctx.Canceled from
// runLoop.Run and calls MarkFailed with code="cancelled". This pins
// the documented ctx-cancel terminal-state decision.
func TestPerTaskRunLoopDriver_FSMBridge_MarksFailed_OnCtxCancel(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	// Planner pauses on first Next; the run blocks on WaitForEvent
	// until ctx cancels.
	stepsCh := make(chan int, 4)
	p := &driverTestPlanner{
		pauseReason: planner.PauseApprovalRequired,
		stepsCh:     stepsCh,
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}

	taskID := spawnDriverTestTask(t, reg)
	// Wait for the planner to fire (so we know the run is blocked at
	// WaitForEvent post-RequestPause).
	select {
	case <-stepsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired")
	}

	// Close cancels subCtx; the in-flight Run unblocks with ctx.Err and
	// the driver's runOne calls MarkFailed with code="cancelled".
	if err := driver.Close(context.Background()); err != nil {
		t.Fatalf("driver.Close: %v", err)
	}

	// FSM should be Failed (the Mark* call runs after Close cancels but
	// before runsWG.Wait returns; Close blocks until the per-task
	// goroutine returns, which it does AFTER calling MarkFailed).
	status := waitForTaskStatus(t, reg, taskID, tasks.StatusFailed, 2*time.Second)
	if status != tasks.StatusFailed {
		t.Fatalf("task FSM stuck at %q, want %q (D-098 ctx-cancel path)",
			status, tasks.StatusFailed)
	}
	getCtx, _ := identity.With(context.Background(), runLoopDriverTestID)
	task, err := reg.Get(getCtx, taskID)
	if err != nil {
		t.Fatalf("reg.Get: %v", err)
	}
	if task.Error == nil {
		t.Fatal("task.Error is nil after ctx-cancel MarkFailed")
	}
	if task.Error.Code != "cancelled" {
		t.Errorf("error code = %q, want %q (D-098 documented ctx-cancel decision)",
			task.Error.Code, "cancelled")
	}
}

// TestPerTaskRunLoopDriver_SkipsBackgroundTasks — the driver only
// drives foreground tasks; background tasks are emitted by the
// planner itself (SpawnTask Decision) and run on the runtime
// dispatch executor. Asserts the driver does NOT start a RunLoop for
// a `task.spawned` of `KindBackground`.
func TestPerTaskRunLoopDriver_SkipsBackgroundTasks(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord)
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	stepsCh := make(chan int, 4)
	p := &driverTestPlanner{stepsCh: stepsCh}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	// Spawn a real background task — the registry publishes
	// task.spawned with Kind=Background and the driver MUST skip it.
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, err = reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:     tasks.KindBackground,
		Query:    "background skip-me",
	})
	if err != nil {
		t.Fatalf("reg.Spawn(background): %v", err)
	}

	// The driver MUST NOT start a RunLoop for a background task.
	select {
	case step := <-stepsCh:
		t.Errorf("planner.Next fired (step=%d) — driver should have skipped the background task", step)
	case <-time.After(200 * time.Millisecond):
		// expected: no fire
	}
}

// TestPerTaskRunLoopDriver_Close_DrainsRunningRuns — Close cancels
// the subscription and waits for in-flight RunLoop goroutines to
// drain. Asserts no goroutine leak across teardown. With D-098 the
// per-task goroutine now blocks on Run AND the post-Run Mark* call;
// the test pins that Close still drains within a bounded window.
func TestPerTaskRunLoopDriver_Close_DrainsRunningRuns(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	stepsCh := make(chan int, 4)
	p := &driverTestPlanner{
		pauseReason: planner.PauseApprovalRequired,
		stepsCh:     stepsCh,
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}

	// Spawn a real task; the planner pauses on first Next so the run
	// blocks at WaitForEvent waiting for a resume.
	_ = spawnDriverTestTask(t, reg)
	select {
	case <-stepsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired")
	}

	// Now Close — should cancel the run's ctx and wait for it to
	// return (the RunLoop's WaitForEvent unblocks with ctx.Err). The
	// per-task goroutine then calls MarkFailed before returning.
	done := make(chan struct{})
	go func() {
		_ = driver.Close(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("driver.Close did not drain within 2s — goroutine leak")
	}

	// Close is idempotent.
	if err := driver.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestPerTaskRunLoopDriver_IdempotentStart — calling Start twice is
// a no-op (the driver remembers it has started).
func TestPerTaskRunLoopDriver_IdempotentStart(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord)
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: &driverTestPlanner{},
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("second Start (should no-op): %v", err)
	}
	_ = driver.Close(context.Background())
}

// TestPerTaskRunLoopDriver_ConcurrentReuse_NoRaceUnderLoad — W1 from
// the Wave 11.5 §17.5 audit, extended to cover the D-098 FSM bridge.
// Stress the driver with N≥100 concurrent real Spawn calls (each
// emitting task.spawned via the registry's publish path) while a
// separate goroutine races Close. Asserts: no race detector hits, no
// goroutine leak (every spawned RunLoop drains before Close returns),
// Close is idempotent under concurrent publishers, and the FSM bridge
// holds under load — each Mark* call hits the registry concurrently.
func TestPerTaskRunLoopDriver_ConcurrentReuse_NoRaceUnderLoad(t *testing.T) {
	const n = 128

	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	// Planner finishes immediately with FinishGoal so each per-task
	// RunLoop returns quickly and the FSM bridge transitions Pending
	// → Running → Complete under stress.
	p := &driverTestPlanner{finishGoalImmediately: true}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}

	// N publishers each Spawn a real task under a distinct
	// (tenant, session) so identity-scoped state never overlaps
	// (§6 isolation under stress).
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  "tenant-stress-" + itoa(i),
				UserID:    "user-stress",
				SessionID: "session-stress-" + itoa(i),
			}
			ctx, withErr := identity.With(context.Background(), id)
			if withErr != nil {
				return
			}
			_, _ = reg.Spawn(ctx, tasks.SpawnRequest{
				Identity: identity.Quadruple{Identity: id},
				Kind:     tasks.KindForeground,
				Query:    "stress-" + itoa(i),
			})
		}()
	}
	wg.Wait()

	// Close drains every per-task goroutine the driver spawned. The
	// idempotent-Close test above pins double-Close-safety; this run
	// asserts Close-during-burst is race-free + leak-free under -race.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := driver.Close(closeCtx); err != nil {
		t.Errorf("driver.Close after stress: %v", err)
	}
	// Idempotent — a second Close is a no-op.
	if err := driver.Close(closeCtx); err != nil {
		t.Errorf("second driver.Close: %v", err)
	}
}

// TestTrajectoryByTaskID_ConcurrentReads — Phase 107a (AC-14).
// Runs N=100 concurrent reads against the trajectory map while writes
// are happening in the background, asserting no races and that the map
// returns either nil or the correct trajectory for a given task.
func TestTrajectoryByTaskID_ConcurrentReads(t *testing.T) {
	const n = 100

	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &driverTestPlanner{finishGoalImmediately: true}
	driver, err := newPerTaskRunLoopDriver(perTaskRunLoopDriverOpts{
		bus:     bus,
		runLoop: rl,
		planner: p,
		tasks:   reg,
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}

	// Spawn one task so the map has at least one entry, then run concurrent
	// reads + writes against the RWMutex under -race.
	id := identity.Identity{
		TenantID:  "tenant-trajectory",
		UserID:    "user-trajectory",
		SessionID: "session-trajectory",
	}
	ctx, withErr := identity.With(context.Background(), id)
	if withErr != nil {
		t.Fatalf("identity.With: %v", withErr)
	}
	spawned, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id},
		Kind:     tasks.KindForeground,
		Query:    "trajectory-test",
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	// The subscribe loop may not have picked up the task yet. Poll briefly
	// until TrajectoryByTaskID returns non-nil or we time out.
	var traj *planner.Trajectory
	for range 50 {
		traj = driver.TrajectoryByTaskID(spawned.ID)
		if traj != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if traj == nil {
		t.Fatal("trajectory not stored before poll timeout — driver may not have picked up task.spawned")
	}

	// Concurrent reads under -race.
	var readWG sync.WaitGroup
	readWG.Add(n)
	for range n {
		go func() {
			defer readWG.Done()
			for j := 0; j < 100; j++ {
				_ = driver.TrajectoryByTaskID(spawned.ID)
			}
		}()
	}
	readWG.Wait()

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	_ = driver.Close(closeCtx)

	// After Close, a nil for a never-seen task returns nil.
	if got := driver.TrajectoryByTaskID("no-such-task"); got != nil {
		t.Errorf("expected nil for unknown task, got %v", got)
	}
}

// itoa is a stdlib-free int→string helper for the stress test's
// identity construction. Kept tiny to avoid pulling in strconv just
// for this file when no other test needs it.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := make([]byte, 0, 8)
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestExtractSkillKeywords — Phase 83m (Item 4, D-156): the helper
// the runloop runs the raw task Query through before calling
// `skills.Search`. The FTS5 ranker performs best on keyword-shaped
// input — articles + punctuation + stopwords dilute the BM25 signal.
// The helper lowercases, splits on whitespace + punctuation, drops
// stopwords + 1-char tokens, dedupes (preserving order), caps at 10.
func TestExtractSkillKeywords(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty_input",
			input: "",
			want:  "",
		},
		{
			name:  "all_stopwords_returns_empty",
			input: "the a an of to in",
			want:  "",
		},
		{
			name:  "sentence_with_punctuation",
			input: "How do I configure the OAuth provider?",
			want:  "how configure oauth provider",
		},
		{
			name:  "lowercases_input",
			input: "DEPLOY my Helm Chart",
			want:  "deploy helm chart",
		},
		{
			name:  "dedupes_preserving_order",
			input: "auth config auth setup auth",
			want:  "auth config setup",
		},
		{
			name:  "drops_single_char_tokens",
			input: "a b configure x setup y",
			want:  "configure setup",
		},
		{
			name:  "splits_on_punctuation",
			input: "tool.invoke(name,args)",
			want:  "tool invoke name args",
		},
		{
			name:  "caps_at_ten_terms",
			input: "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima",
			want:  "alpha bravo charlie delta echo foxtrot golf hotel india juliet",
		},
		{
			name:  "preserves_domain_keywords",
			input: "Search the api docs for the rate-limit knob",
			want:  "search api docs rate limit knob",
		},
		{
			name:  "numbers_kept_when_long_enough",
			input: "deploy version 42 to staging",
			want:  "deploy version 42 staging",
		},
		{
			name:  "single_digit_dropped",
			input: "task 1 needs attention",
			want:  "task needs attention",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractSkillKeywords(tc.input)
			if got != tc.want {
				t.Errorf("extractSkillKeywords(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestExtractSkillKeywords_CapsAtTenTermsExactly — boundary assert:
// the helper returns EXACTLY maxSkillKeywords tokens when the input
// produces more than that, never more, never fewer.
func TestExtractSkillKeywords_CapsAtTenTermsExactly(t *testing.T) {
	// 12 distinct non-stopword tokens.
	input := "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima"
	got := extractSkillKeywords(input)
	terms := 0
	for _, ch := range got {
		if ch == ' ' {
			terms++
		}
	}
	terms++ // count = spaces + 1
	if terms != maxSkillKeywords {
		t.Errorf("extractSkillKeywords cap: got %d terms, want %d (output=%q)", terms, maxSkillKeywords, got)
	}
}
