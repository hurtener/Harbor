package steering

// Phase 111c (D-200) RunLoop tests: trajectory threading into
// requestPause, and the timeout-is-terminal contract — a pause reaped
// out-of-band with the typed `timeout` Decision finishes the waiting
// run with Finish{ConstraintsConflict}, never a silent
// unpark-and-continue and never a park-forever.

import (
	"context"
	"errors"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// TestRun_RequestPause_ThreadsTrajectory asserts the RunLoop hands the
// run's live Trajectory to Coordinator.Request (the Phase 111c closure
// of the `Trajectory: nil` gap) so a checkpoint-store-backed
// Coordinator can persist planner state with the pause record.
func TestRun_RequestPause_ThreadsTrajectory(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &pausingPlanner{reason: planner.PauseApprovalRequired}

	tr := &trajectory.Trajectory{Query: "trajectory must reach the coordinator"}
	spec := runSpecFor(runA, p)
	spec.Base.Trajectory = tr

	done := make(chan error, 1)
	go func() {
		_, err := rl.Run(context.Background(), spec)
		done <- err
	}()

	// Bounded eventually-wait for the pause boundary.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, _ := coord.snapshot(); req >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	coord.mu.Lock()
	got := coord.lastRequest.Trajectory
	coord.mu.Unlock()
	if got != tr {
		t.Fatalf("PauseRequest.Trajectory = %p, want the run's live trajectory %p (Trajectory: nil regression)", got, tr)
	}

	// Unblock and drain the run.
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlApprove,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish within 5s of the APPROVE")
	}
}

// TestRun_PauseTimeout_BusWake_TerminatesConstraintsConflict drives the
// full out-of-band timeout path against the REAL Coordinator + REAL
// inmem bus: the run parks on its RequestPause, a sweeper-shaped
// Resume(DecisionTimeout) lands out-of-band, the `pause.resumed` bus
// event wakes the park, and the run terminates with the
// constraints-conflict Finish — DecisionTimeout's terminal semantics
// (the D-071 REJECT posture applied to deadlines).
func TestRun_PauseTimeout_BusWake_TerminatesConstraintsConflict(t *testing.T) {
	red := patternsAudit.New()
	bus := mkBridgeBus(t, red)
	coord := pauseresume.New(pauseresume.WithBus(bus))
	reg := NewRegistry()
	rl, err := NewRunLoop(reg, coord, WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	// Observe pause.requested so the test learns the run's Token.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: runA.TenantID, User: runA.UserID, Session: runA.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	p := &pausingPlanner{reason: planner.PauseAwaitInput}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	var token pauseresume.Token
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(pauseresume.PauseRequestedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want PauseRequestedPayload", ev.Payload)
		}
		token = pauseresume.Token(payload.Token)
	case <-time.After(5 * time.Second):
		t.Fatal("no pause.requested event within 5s")
	}

	// The sweeper-shaped out-of-band reap, under the pause's own scope.
	resumeCtx := ctxWithIdentity(context.Background(), runA)
	if err := coord.Resume(resumeCtx, token, pauseresume.DecisionTimeout, map[string]any{"timed_out": true}); err != nil {
		t.Fatalf("out-of-band Resume(timeout): %v", err)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q (timeout must be terminal)", out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Fatalf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
		if out.fin.Metadata["pause_token"] != string(token) {
			t.Fatalf("Finish.Metadata[pause_token] = %v, want %q", out.fin.Metadata["pause_token"], token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not terminate within 5s of the timeout resume — the park never woke")
	}

	// The planner was NEVER re-entered after the pause (no silent
	// unpark-and-continue).
	p.mu.Lock()
	steps := len(p.seenRC)
	p.mu.Unlock()
	if steps != 1 {
		t.Fatalf("planner Next calls = %d, want 1 (a timed-out pause must not re-enter the planner)", steps)
	}
}

// TestRun_PauseTimeout_FastPathWithoutBus pins the bus-independent
// detection channel: a bus-less RunLoop still observes the out-of-band
// timeout via the Coordinator.Status fast-path at the next park
// boundary and terminates the run.
func TestRun_PauseTimeout_FastPathWithoutBus(t *testing.T) {
	coord := pauseresume.New() // real Coordinator, no bus
	reg := NewRegistry()
	rl, err := NewRunLoop(reg, coord) // no bus on the RunLoop either
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	p := &pausingPlanner{reason: planner.PauseApprovalRequired}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	// Bounded eventually-wait until the pause record exists (Request
	// returned ⇒ the run is at / heading into the park).
	resumeCtx := ctxWithIdentity(context.Background(), runA)
	var token pauseresume.Token
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, lerr := coord.List(resumeCtx, pauseresume.ListRequest{Identity: runA.Identity})
		if lerr != nil {
			t.Fatalf("List: %v", lerr)
		}
		if len(resp.Snapshots) == 1 {
			token = resp.Snapshots[0].Token
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if token == "" {
		t.Fatal("pause record did not appear within 5s")
	}

	// Out-of-band timeout reap, then a non-resume control to wake the
	// park (the fallback Status re-check ticker is deliberately coarse;
	// the wake exercises the next boundary's fast-path check).
	if err := coord.Resume(resumeCtx, token, pauseresume.DecisionTimeout, nil); err != nil {
		t.Fatalf("out-of-band Resume(timeout): %v", err)
	}
	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlInjectContext,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"note": "wake"},
	}); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Fatalf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bus-less run did not terminate after the timeout resume")
	}
}

// TestRun_ResumeControl_LosesRaceToTimeout pins the documented loser
// contract at the RunLoop layer: a legitimate RESUME control whose
// Coordinator.Resume surfaces ErrAlreadyResumed BECAUSE the sweeper's
// timeout reap won the race yields the honest timeout-terminal Finish,
// not a run error. Scripted deterministically against the stub
// Coordinator (Resume fails ErrAlreadyResumed; Status then reports
// resumed/timeout).
func TestRun_ResumeControl_LosesRaceToTimeout(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	coord.resumeErr = pauseresume.ErrAlreadyResumed
	coord.statusAfterResume = &pauseresume.Status{
		State:    pauseresume.StatusResumed,
		Decision: pauseresume.DecisionTimeout,
	}

	p := &pausingPlanner{reason: planner.PauseApprovalRequired}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, _ := coord.snapshot(); req >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run returned an error, want the timeout-terminal Finish: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Fatalf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not terminate after the losing RESUME control")
	}
}

// TestRun_ResumeControl_NonTimeoutAlreadyResumed_StaysLoud guards the
// carve-out's boundary: an ErrAlreadyResumed that is NOT a timeout
// race (Status carries no timeout Decision) still fails the run loud —
// the carve-out never widens into silent error swallowing.
func TestRun_ResumeControl_NonTimeoutAlreadyResumed_StaysLoud(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	coord.resumeErr = pauseresume.ErrAlreadyResumed
	coord.statusAfterResume = &pauseresume.Status{
		State:    pauseresume.StatusResumed,
		Decision: pauseresume.DecisionApprove, // resolved, but not by the sweeper
	}

	p := &pausingPlanner{reason: planner.PauseApprovalRequired}
	done := make(chan error, 1)
	go func() {
		_, rerr := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- rerr
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, _ := coord.snapshot(); req >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	in, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}

	select {
	case rerr := <-done:
		if !errors.Is(rerr, pauseresume.ErrAlreadyResumed) {
			t.Fatalf("Run err = %v, want ErrAlreadyResumed surfaced loud", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not surface the apply failure")
	}
}
