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
	"github.com/hurtener/Harbor/internal/tasks"
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

// driverTestPlanner emits exactly one RequestPause (step 0), then
// Finish on every later step. The pause holds the RunLoop at
// inbox.WaitForEvent — the test can observe that Run was called
// (Coordinator.Request fired) without driving a complete control
// flow.
type driverTestPlanner struct {
	mu          sync.Mutex
	steps       int
	stepsCh     chan int // optional: each Next sends its step number
	pauseReason planner.PauseReason
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
	})
	if err == nil {
		t.Fatal("newPerTaskRunLoopDriver(nil planner) returned nil error, want failure")
	}
}

// TestPerTaskRunLoopDriver_PicksUpTaskSpawned_DrivesRunLoop — the
// §13 primitive-with-consumer evidence at the cmd layer. Construct
// the driver against a real bus + real RunLoop + scripted planner.
// Publish a `task.spawned` event. Assert the planner's Next is
// called (the driver started the RunLoop). Close drains cleanly.
func TestPerTaskRunLoopDriver_PicksUpTaskSpawned_DrivesRunLoop(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
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
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	// Publish a task.spawned event. The driver should pick it up and
	// call rl.Run.
	taskID := tasks.TaskID("task-driver-test-1")
	ev := events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Payload: tasks.TaskSpawnedPayload{
			TaskID: taskID,
			Kind:   tasks.KindForeground,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("bus.Publish(task.spawned): %v", err)
	}

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

// TestPerTaskRunLoopDriver_SkipsBackgroundTasks — the driver only
// drives foreground tasks; background tasks are emitted by the
// planner itself (SpawnTask Decision) and run on the runtime
// dispatch executor. Asserts the driver does NOT start a RunLoop for
// a `task.spawned` of `KindBackground`.
func TestPerTaskRunLoopDriver_SkipsBackgroundTasks(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
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
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	ev := events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Payload: tasks.TaskSpawnedPayload{
			TaskID: tasks.TaskID("task-driver-bg"),
			Kind:   tasks.KindBackground,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("bus.Publish: %v", err)
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
// drain. Asserts no goroutine leak across teardown.
func TestPerTaskRunLoopDriver_Close_DrainsRunningRuns(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
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
	})
	if err != nil {
		t.Fatalf("newPerTaskRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}

	// Spawn a task and wait for the planner to be invoked (the run
	// is now blocked at WaitForEvent because the planner RequestPaused).
	ev := events.Event{
		Type:     tasks.EventTypeTaskSpawned,
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Payload: tasks.TaskSpawnedPayload{
			TaskID: tasks.TaskID("task-driver-drain"),
			Kind:   tasks.KindForeground,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}
	select {
	case <-stepsCh:
	case <-time.After(2 * time.Second):
		t.Fatal("planner.Next never fired")
	}

	// Now Close — should cancel the run's ctx and wait for it to
	// return (the RunLoop's WaitForEvent unblocks with ctx.Err).
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
