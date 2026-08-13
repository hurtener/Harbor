package serve

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// trancheDriverPlanner is the serve-driver fixture for the step-tranche
// path: it emits TrancheSteps worth of CallTool steps, then a terminal
// Finish on the step AFTER the tranche would park — so the driver's
// TrancheSteps knob is what parks the run, not the planner.
type trancheDriverPlanner struct {
	mu    sync.Mutex
	steps int
}

func (p *trancheDriverPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	p.steps++
	step := p.steps
	p.mu.Unlock()
	if step <= 2 {
		return planner.CallTool{Tool: "noop"}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"answer": "done"}}, nil
}

func (p *trancheDriverPlanner) stepCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.steps
}

// TestRunLoopDriver_TrancheSteps_ParksAndResumesViaControl is the
// serve-driver end-to-end: with TrancheSteps=2, a spawned task's run
// parks at its step-tranche boundary (the pause record carries the
// typed {cause, max_steps, steps_observed} payload — the truthful
// projection, resumable without any planner state), the task stays
// Running (no terminal failure), and an authorised RESUME control on
// the run's steering inbox grants a fresh tranche that drives the task
// to Complete.
func TestRunLoopDriver_TrancheSteps_ParksAndResumesViaControl(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	p := &trancheDriverPlanner{}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:             bus,
		RunLoop:         rl,
		Planner:         p,
		Tasks:           reg,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxStepsRunLoop: 64,
		TrancheSteps:    2,
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if driver.trancheSteps != 2 {
		t.Fatalf("driver.trancheSteps = %d, want 2 (option not plumbed)", driver.trancheSteps)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	taskID := spawnDriverTestTask(t, reg)

	// Wait for the step-tranche park: the pause record appears in the
	// unified Coordinator's List under the run's identity.
	q := identity.Quadruple{Identity: runLoopDriverTestID, RunID: string(taskID)}
	deadline := time.Now().Add(5 * time.Second)
	var snap pauseresume.Pause
	var st pauseresume.Status
	found := false
	for time.Now().Before(deadline) {
		resp, lerr := coord.List(context.Background(), pauseresume.ListRequest{
			Identity: runLoopDriverTestID,
			Filter:   pauseresume.ListFilter{RunIDs: []string{string(taskID)}},
			PageSize: 50,
		})
		if lerr == nil && len(resp.Snapshots) > 0 {
			snap = resp.Snapshots[0]
			if len(resp.Statuses) > 0 {
				st = resp.Statuses[0]
			}
			found = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !found {
		t.Fatal("task run did not park at its step-tranche boundary (no pause record appeared)")
	}
	if snap.Reason != pauseresume.ReasonConstraintsConflict {
		t.Errorf("pause Reason = %q, want %q", snap.Reason, pauseresume.ReasonConstraintsConflict)
	}
	payload, ok := pauseresume.TrancheExceededFromMap(snap.Payload)
	if !ok {
		t.Fatalf("park payload %v is not a TrancheExceededPayload", snap.Payload)
	}
	if payload.MaxSteps != 2 || payload.StepsObserved != 2 {
		t.Errorf("park payload = %+v, want max_steps=2 steps_observed=2", payload)
	}
	if !st.Continuable() {
		t.Fatal("park status not continuable — the projection must truthfully render Continue")
	}
	// The task is parked, not failed: the planner took exactly the
	// tranche's worth of steps and the FSM is still Running.
	if steps := p.stepCount(); steps != 2 {
		t.Errorf("planner steps while parked = %d, want 2", steps)
	}
	if status := waitForTaskStatus(t, reg, taskID, tasks.StatusRunning, 2*time.Second); status != tasks.StatusRunning {
		t.Fatalf("parked task status = %q, want running (no terminal failure)", status)
	}

	// An authorised RESUME grants a fresh tranche; the planner's next
	// step is the terminal Finish, so the task completes.
	in, err := steerReg.Lookup(q)
	if err != nil {
		t.Fatalf("steering inbox lookup: %v", err)
	}
	if err := in.Enqueue(steering.ControlEvent{
		Type:         steering.ControlResume,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: runLoopDriverTestID.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME): %v", err)
	}
	if status := waitForTaskStatus(t, reg, taskID, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		failed, getErr := reg.Get(context.Background(), taskID)
		t.Fatalf("task stuck at %q after RESUME, want complete (task error=%+v, get error=%v)", status, failed.Error, getErr)
	}
	if steps := p.stepCount(); steps != 3 {
		t.Errorf("total planner steps = %d, want 3 (2 in the first tranche + 1 terminal after resume)", steps)
	}
}
