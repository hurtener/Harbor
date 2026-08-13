package steering

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// trancheSpecFor builds a RunSpec with tranche pausing enabled: a live
// cumulative Trajectory (so CallTool steps append) and a generous
// MaxSteps ceiling so repeat tranche cycles are never truncated by the
// absolute breaker.
func trancheSpecFor(q identity.Quadruple, p planner.Planner, trancheSteps, maxSteps int) RunSpec {
	return RunSpec{
		Planner:      p,
		TrancheSteps: trancheSteps,
		MaxSteps:     maxSteps,
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "tranche goal",
			Trajectory: &planner.Trajectory{Query: "tranche goal"},
		},
	}
}

// waitForPause polls until the stub Coordinator has recorded at least
// `want` Request calls (bounded, never a fixed sleep).
func waitForPause(t *testing.T, coord *stubCoordinator, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if req, _ := coord.snapshot(); req >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	req, _ := coord.snapshot()
	t.Fatalf("Coordinator.Request calls = %d, want >= %d (run did not park)", req, want)
}

// lastTrancheRequest decodes the stub's most recent PauseRequest and
// asserts it is the step-tranche park shape: constraints_conflict with
// the typed TrancheExceededPayload and the run's live trajectory.
func lastTrancheRequest(t *testing.T, coord *stubCoordinator, wantMax, wantSteps int) {
	t.Helper()
	coord.mu.Lock()
	req := coord.lastRequest
	coord.mu.Unlock()
	if req.Reason != pauseresume.ReasonConstraintsConflict {
		t.Errorf("tranche park Reason = %q, want %q", req.Reason, pauseresume.ReasonConstraintsConflict)
	}
	payload, ok := pauseresume.TrancheExceededFromMap(req.Payload)
	if !ok {
		t.Fatalf("tranche park Payload %v is not a TrancheExceededPayload", req.Payload)
	}
	if payload.Cause != pauseresume.TrancheCauseMaxStepsExceeded || payload.MaxSteps != wantMax || payload.StepsObserved != wantSteps {
		t.Errorf("tranche park payload = %+v, want {cause=max_steps_exceeded max_steps=%d steps_observed=%d}", payload, wantMax, wantSteps)
	}
	if req.Trajectory == nil {
		t.Fatal("tranche park did not checkpoint the run's trajectory")
	}
	if got := len(req.Trajectory.Steps); got != wantSteps {
		t.Errorf("checkpointed trajectory steps = %d, want %d (cumulative, one run)", got, wantSteps)
	}
}

// runOutcome carries the (fin, err) of a goroutine-driven Run so tests
// can await it without blocking the parking assertions.
type runOutcome struct {
	fin planner.Finish
	err error
}

// TestRun_TrancheExhaustion_ParksWithoutTerminalFailure asserts the
// authoritative step-tranche counter parks the run after exactly
// TrancheSteps planner steps — no terminal failure, no further planner
// activity — with the typed payload and the cumulative trajectory
// checkpointed through the Coordinator.
func TestRun_TrancheExhaustion_ParksWithoutTerminalFailure(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()

	waitForPause(t, coord, 1)
	// Parked: the planner was entered exactly the tranche's worth of
	// steps and is NOT re-entered while parked (no further activity).
	if steps := p.stepCount(); steps != 2 {
		t.Fatalf("planner Next calls while parked = %d, want 2 (the tranche cap)", steps)
	}
	// Not terminal: the run is still blocked at the park.
	select {
	case out := <-done:
		t.Fatalf("run terminated while parked: fin=%+v err=%v (a tranche park must not be terminal)", out.fin, out.err)
	case <-time.After(50 * time.Millisecond):
	}
	lastTrancheRequest(t, coord, 2, 2)

	// An authorised RESUME continues the SAME run to its terminal
	// Finish — no new task, no second park (the planner finishes before
	// the next tranche boundary).
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run after RESUME: %v", out.err)
		}
		if out.fin.Reason != planner.FinishGoal {
			t.Errorf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish within 3s after RESUME")
	}
	if req, _ := coord.snapshot(); req != 1 {
		t.Errorf("Coordinator.Request calls = %d, want 1 (no re-park after the finishing resume)", req)
	}
}

func TestRun_Tranche_RequiresCancellationCapabilityBeforePlanning(t *testing.T) {
	base := pauseresume.New()
	coord := struct{ pauseresume.Coordinator }{Coordinator: base}
	reg := NewRegistry()
	rl, err := NewRunLoop(reg, coord)
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	_, err = rl.Run(context.Background(), trancheSpecFor(runA, &scriptedPlanner{}, 1, 4))
	if !errors.Is(err, pauseresume.ErrTrancheCancellerRequired) {
		t.Fatalf("Run without tranche capability: err=%v, want ErrTrancheCancellerRequired", err)
	}
}

func TestRun_Tranche_CancelCleanupFailureStillReturnsTerminalCancellation(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	coord.cancelTrancheErr = &pauseresume.TrancheCancellationError{Err: errors.New("checkpoint delete failed")}
	p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "noop"}}}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 1, 8))
		done <- runOutcome{fin: fin, err: err}
	}()
	waitForPause(t, coord, 1)
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID}); err != nil {
		t.Fatalf("Enqueue(CANCEL): %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Run error = %v, want nil after cleanup failure", got.err)
		}
		if got.fin.Reason != planner.FinishCancelled {
			t.Fatalf("Finish.Reason = %q, want %q", got.fin.Reason, planner.FinishCancelled)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish after CANCEL")
	}
	if calls, cancelled := coord.trancheSnapshot(pauseresume.Token("stub-token")); calls != 1 || !cancelled {
		t.Fatalf("tranche cancellation = calls %d, cancelled %v; want one terminal cancellation", calls, cancelled)
	}
}

func TestRun_Tranche_CancelPreConsumptionErrorReturnsLoudly(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	wantErr := errors.New("tranche token not consumed")
	coord.cancelTrancheErr = wantErr
	p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "noop"}}}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 1, 8))
		done <- runOutcome{fin: fin, err: err}
	}()
	waitForPause(t, coord, 1)
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{Type: ControlCancel, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID}); err != nil {
		t.Fatalf("Enqueue(CANCEL): %v", err)
	}
	select {
	case got := <-done:
		if !errors.Is(got.err, wantErr) {
			t.Fatalf("Run error = %v, want %v", got.err, wantErr)
		}
		if got.fin.Reason != "" {
			t.Fatalf("Finish.Reason = %q, want empty on pre-consumption error", got.fin.Reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after pre-consumption cancellation error")
	}
	if calls, cancelled := coord.trancheSnapshot(pauseresume.Token("stub-token")); calls != 1 || cancelled {
		t.Fatalf("tranche cancellation = calls %d, cancelled %v; want one call without terminal cancellation", calls, cancelled)
	}
}

func TestRun_Tranche_CancelWithResumeApproveRejectBatch_TerminalizesInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first ControlType
		last  ControlType
	}{
		{name: "cancel-before-resume", first: ControlCancel, last: ControlResume},
		{name: "resume-before-cancel", first: ControlResume, last: ControlCancel},
		{name: "cancel-before-approve", first: ControlCancel, last: ControlApprove},
		{name: "approve-before-cancel", first: ControlApprove, last: ControlCancel},
		{name: "cancel-before-reject", first: ControlCancel, last: ControlReject},
		{name: "reject-before-cancel", first: ControlReject, last: ControlCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rl, reg, coord := newTestRunLoop(t)
			p := &scriptedPlanner{script: []scriptStep{{dec: planner.CallTool{Tool: "noop"}}}}
			out := make(chan runOutcome, 1)
			go func() {
				fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 1, 8))
				out <- runOutcome{fin: fin, err: err}
			}()
			waitForPause(t, coord, 1)
			in, err := reg.Lookup(runA)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			for _, typ := range []ControlType{tc.first, tc.last} {
				if err := in.Enqueue(ControlEvent{Type: typ, Identity: runA, CallerScope: ScopeOwnerUser, CallerTenant: runA.TenantID}); err != nil {
					t.Fatalf("Enqueue(%s): %v", typ, err)
				}
			}
			select {
			case got := <-out:
				if got.err != nil {
					t.Errorf("Run error = %v, want nil for terminal cancellation", got.err)
				}
				if got.fin.Reason != planner.FinishCancelled {
					t.Errorf("Finish.Reason = %q, want %q", got.fin.Reason, planner.FinishCancelled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("run did not terminalize after CANCEL/RESUME batch")
			}
			if calls, resumes := coord.snapshot(); calls != 1 || resumes != 0 {
				t.Errorf("Coordinator calls = request %d, resume %d; want request 1, resume 0", calls, resumes)
			}
			if calls, cancelled := coord.trancheSnapshot(pauseresume.Token("stub-token")); calls != 1 || !cancelled {
				t.Errorf("tranche cancellation = calls %d, cancelled %v; want one terminal cancellation", calls, cancelled)
			}
		})
	}
}

// TestRun_TrancheResume_GrantsFreshTranche_PreservesOneRun asserts
// repeat pause→resume cycles keep ONE run and ONE CUMULATIVE trajectory:
// each authorised resume grants a fresh tranche (the counter resets to
// the cap's worth of steps) while the checkpointed trajectory grows
// monotonically across cycles.
func TestRun_TrancheResume_GrantsFreshTranche_PreservesOneRun(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()

	// Cycle 1: park after the first tranche (2 steps, 2 trajectory steps).
	waitForPause(t, coord, 1)
	lastTrancheRequest(t, coord, 2, 2)
	if steps := p.stepCount(); steps != 2 {
		t.Fatalf("cycle-1 planner steps = %d, want 2", steps)
	}

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME #1): %v", err)
	}

	// Cycle 2: a fresh tranche grants two MORE steps, then parks again.
	// The trajectory is CUMULATIVE (4 steps) — one run, never reset.
	waitForPause(t, coord, 2)
	lastTrancheRequest(t, coord, 2, 2)
	if steps := p.stepCount(); steps != 4 {
		t.Fatalf("cycle-2 planner steps = %d, want 4 (2 per tranche × 2 cycles)", steps)
	}

	if err := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME #2): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishGoal {
			t.Errorf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish within 3s after the second RESUME")
	}
	if steps := p.stepCount(); steps != 5 {
		t.Errorf("total planner steps = %d, want 5 (the scripted Finish)", steps)
	}
}

// TestRun_TranchePlannerPause_DoesNotResetTranche asserts a
// planner-emitted RequestPause mid-tranche does NOT grant a fresh
// tranche: its resume leaves the tranche counter intact, so the run
// parks after the tranche's remaining steps, not a full re-grant.
func TestRun_TranchePlannerPause_DoesNotResetTranche(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.RequestPause{Reason: planner.PauseAwaitInput, Payload: map[string]any{"why": "operator input"}}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 3, 64))
		done <- runOutcome{fin: fin, err: err}
	}()

	// The planner's own pause arrives first (request #1).
	waitForPause(t, coord, 1)
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME planner pause): %v", err)
	}

	// The run resumes mid-tranche (counter intact at 2), takes one more
	// step, and parks at the tranche boundary (request #2) — NOT after a
	// fresh 3-step re-grant.
	waitForPause(t, coord, 2)
	if steps := p.stepCount(); steps != 3 {
		t.Fatalf("planner steps at the tranche park = %d, want 3 (2 before the planner pause + 1 after — the planner pause did NOT reset the tranche)", steps)
	}
	lastTrancheRequest(t, coord, 3, 3)

	if err := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(RESUME tranche park): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishGoal {
			t.Errorf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish within 3s after the tranche-park RESUME")
	}
}

// TestRun_TrancheReject_TerminatesTyped asserts a REJECT on a tranche
// park keeps the typed terminal behavior: Finish{ConstraintsConflict}
// (the REJECT posture — a rejected step-budget continuation is a
// constraint the planner cannot resolve).
func TestRun_TrancheReject_TerminatesTyped(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()
	waitForPause(t, coord, 1)

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         ControlReject,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"why": "stop the work"},
	}); err != nil {
		t.Fatalf("Enqueue(REJECT): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run after REJECT: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Errorf("Finish.Reason = %q, want %q (typed terminal behavior retained)", out.fin.Reason, planner.FinishConstraintsConflict)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not terminate within 3s after REJECT")
	}
	if _, res := coord.snapshot(); res != 1 {
		t.Errorf("Coordinator.Resume calls = %d, want 1 (the REJECT advanced the pause)", res)
	}
}

// TestRun_TrancheCancel_TerminatesTyped asserts a CANCEL arriving while
// the run is tranche-parked terminates it with Finish{Cancelled} — the
// same typed terminal behavior as any pause.
func TestRun_TrancheCancel_TerminatesTyped(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()
	waitForPause(t, coord, 1)

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         ControlCancel,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"hard": false},
	}); err != nil {
		t.Fatalf("Enqueue(CANCEL): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run after CANCEL: %v", out.err)
		}
		if out.fin.Reason != planner.FinishCancelled {
			t.Errorf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishCancelled)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not terminate within 3s after CANCEL-while-tranche-parked")
	}
}

// TestRun_TrancheTimeout_TerminatesTyped asserts the max-park sweeper's
// typed timeout resume keeps the timeout-terminal behavior on a tranche
// park: Finish{ConstraintsConflict} with steering_reason=pause_timeout,
// and the planner is never re-entered after the reap.
func TestRun_TrancheTimeout_TerminatesTyped(t *testing.T) {
	reg := NewRegistry()
	coord := &stubCoordinator{statusTimeoutAfterCalls: 1}
	rl, err := NewRunLoop(reg, coord)
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Errorf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Errorf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tranche-parked run did not terminate via the timeout fast-path")
	}
	if steps := p.stepCount(); steps != 2 {
		t.Errorf("planner Next calls = %d, want 2 (the timed-out tranche must not re-enter the planner)", steps)
	}
}

// TestRun_TrancheDuplicateResume_FailsClosed asserts a second RESUME of
// the same tranche pause (two RESUMEs drained in one batch) fails the
// run LOUD with the coordinator's ErrAlreadyResumed — never a silent
// double-apply or a repeat park. Uses the REAL Coordinator so the
// fail-closed sentinel is the shipped one.
func TestRun_TrancheDuplicateResume_FailsClosed(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := pauseresume.New()
	rl, err := NewRunLoop(reg, coord, WithRunLoopClock(clk))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	done := make(chan runOutcome, 1)
	go func() {
		fin, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 2, 64))
		done <- runOutcome{fin: fin, err: err}
	}()

	// Wait for the tranche park via the real Coordinator's List.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, lerr := coord.List(context.Background(), pauseresume.ListRequest{
			Identity: runA.Identity,
			PageSize: 50,
		})
		if lerr == nil && len(resp.Snapshots) > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	resp, lerr := coord.List(context.Background(), pauseresume.ListRequest{
		Identity: runA.Identity,
		PageSize: 50,
	})
	if lerr != nil || len(resp.Snapshots) == 0 {
		t.Fatalf("tranche park not observable via Coordinator.List (err=%v, snapshots=%d)", lerr, len(resp.Snapshots))
	}

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	// Two RESUMEs for the SAME pause, drained in one batch: the first
	// advances the pause, the second must fail closed.
	for i := range 2 {
		if err := in.Enqueue(ControlEvent{
			Type:         ControlResume,
			Identity:     runA,
			CallerScope:  ScopeOwnerUser,
			CallerTenant: runA.TenantID,
		}); err != nil {
			t.Fatalf("Enqueue(RESUME #%d): %v", i+1, err)
		}
	}
	select {
	case out := <-done:
		if !errors.Is(out.err, pauseresume.ErrAlreadyResumed) {
			t.Fatalf("Run err = %v, want it to wrap %v (duplicate resume must fail closed)", out.err, pauseresume.ErrAlreadyResumed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not surface the duplicate-resume failure within 3s")
	}
}

// TestRun_TrancheDisabled_LegacyTerminalBreaker asserts TrancheSteps ≤ 0
// is byte-identical to the legacy behavior: a run that never reaches a
// terminal Finish terminates loud with ErrMaxStepsExceeded (the absolute
// breaker), never a park.
func TestRun_TrancheDisabled_LegacyTerminalBreaker(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	p := &scriptedPlanner{defaultDec: planner.CallTool{Tool: "noop"}}
	_, err := rl.Run(context.Background(), trancheSpecFor(runA, p, 0, 4))
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("Run err = %v, want ErrMaxStepsExceeded (legacy terminal ceiling preserved when tranche pausing is disabled)", err)
	}
}

// TestConcurrentReuse_RunLoop_TranchePausing is the mandatory
// concurrent-reuse stress for the tranche path: N≥100 runs against ONE
// shared RunLoop + ONE shared real Coordinator, each run parking at its
// step-tranche boundary and being resumed by its own resumer goroutine.
// Asserts under -race: no races, no context bleed (each Finish carries
// its own run_id), no cross-cancellation, and the goroutine baseline is
// restored (no leaks — the parked waits and resumers all join).
func TestConcurrentReuse_RunLoop_TranchePausing(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := pauseresume.New()
	rl, err := NewRunLoop(reg, coord, WithRunLoopClock(clk))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	const N = 120
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(N * 2) // N runs + N resumers
	var (
		mu       sync.Mutex
		failures []string
		finished int
	)
	for i := range N {
		q := identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tranche-tenant-%d", i),
				UserID:    fmt.Sprintf("tranche-user-%d", i),
				SessionID: fmt.Sprintf("tranche-session-%d", i),
			},
			RunID: fmt.Sprintf("tranche-run-%d", i),
		}
		runID := q.RunID
		p := &scriptedPlanner{script: []scriptStep{
			{dec: planner.CallTool{Tool: "noop"}},
			{dec: planner.CallTool{Tool: "noop"}},
			{dec: planner.Finish{Reason: planner.FinishGoal, Metadata: map[string]any{"run_id": runID}}},
		}}

		// The run goroutine: drives the tranche, parks, resumes, finishes.
		go func() {
			defer wg.Done()
			fin, rerr := rl.Run(context.Background(), trancheSpecFor(q, p, 2, 64))
			mu.Lock()
			defer mu.Unlock()
			if rerr != nil {
				failures = append(failures, fmt.Sprintf("run %s: Run err = %v", runID, rerr))
				return
			}
			if got, _ := fin.Metadata["run_id"].(string); got != runID {
				failures = append(failures, fmt.Sprintf("run %s: Finish carried run_id %q — context bleed", runID, got))
				return
			}
			finished++
		}()

		// The resumer goroutine: waits for THIS run's tranche pause to
		// appear in the real Coordinator, then enqueues the authorised
		// RESUME that grants the fresh tranche.
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				resp, lerr := coord.List(context.Background(), pauseresume.ListRequest{
					Identity: q.Identity,
					Filter:   pauseresume.ListFilter{RunIDs: []string{runID}},
					PageSize: 50,
				})
				if lerr == nil && len(resp.Snapshots) > 0 {
					in, ierr := reg.Lookup(q)
					if ierr != nil {
						mu.Lock()
						failures = append(failures, fmt.Sprintf("run %s: inbox lookup: %v", runID, ierr))
						mu.Unlock()
						return
					}
					if eerr := in.Enqueue(ControlEvent{
						Type:         ControlResume,
						Identity:     q,
						CallerScope:  ScopeOwnerUser,
						CallerTenant: q.TenantID,
					}); eerr != nil {
						mu.Lock()
						failures = append(failures, fmt.Sprintf("run %s: enqueue RESUME: %v", runID, eerr))
						mu.Unlock()
					}
					return
				}
				runtime.Gosched()
				time.Sleep(2 * time.Millisecond)
			}
			mu.Lock()
			failures = append(failures, fmt.Sprintf("run %s: resumer never observed the tranche pause", runID))
			mu.Unlock()
		}()
	}

	wg.Wait()
	assertNoGoroutineLeak(t, baseline)
	mu.Lock()
	defer mu.Unlock()
	if len(failures) > 0 {
		t.Fatalf("concurrent tranche runs had failures: %v", failures)
	}
	if finished != N {
		t.Errorf("finished runs = %d, want %d", finished, N)
	}
}
