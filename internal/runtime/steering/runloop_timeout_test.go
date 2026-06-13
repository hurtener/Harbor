package steering

// Phase 111c (D-200) RunLoop tests: trajectory threading into
// requestPause, and the timeout-is-terminal contract — a pause reaped
// out-of-band with the typed `timeout` Decision finishes the waiting
// run with Finish{ConstraintsConflict}, never a silent
// unpark-and-continue and never a park-forever.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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
	case <-time.After(30 * time.Second):
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
	case <-time.After(30 * time.Second):
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
	case <-time.After(30 * time.Second):
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
	case <-time.After(30 * time.Second):
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
	case <-time.After(30 * time.Second):
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
	case <-time.After(30 * time.Second):
		t.Fatal("run did not surface the apply failure")
	}
}

// TestRun_PauseTimeout_StatusRecheckBackstop pins the
// delivery-independent backstop branch (Wave C checkpoint audit): on a
// BUS-LESS RunLoop whose pause times out only AFTER the run is parked,
// the ONLY wake channel is the Status re-check ticker
// (`case <-recheck.C`). The stub Coordinator scripts the ordering
// deterministically — the park's two fast-path Status checks (calls 1
// and 2) see a live pause; from call 3 on (the ticker's checks) the
// pause reports resumed/timeout — so a pass proves the ticker branch
// terminates the run, not a fast path. The interval is injected small
// (WithPauseStatusRecheckInterval) so the backstop is exercisable
// without a 30s wall-clock wait.
func TestRun_PauseTimeout_StatusRecheckBackstop(t *testing.T) {
	reg := NewRegistry()
	coord := &stubCoordinator{statusTimeoutAfterCalls: 3}
	rl, err := NewRunLoop(reg, coord,
		WithPauseStatusRecheckInterval(10*time.Millisecond))
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

	// No control is ever enqueued and there is no bus: only the
	// re-check ticker can wake the park.
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q (ticker backstop must be timeout-terminal)",
				out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Fatalf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
	case <-time.After(30 * time.Second):
		t.Fatal("bus-less parked run did not terminate via the Status re-check ticker")
	}

	coord.mu.Lock()
	calls := coord.statusCalls
	coord.mu.Unlock()
	if calls < 3 {
		t.Fatalf("Status calls = %d, want >= 3 (the ticker branch must have fired; 2 = fast paths only)", calls)
	}
	if steps := p.stepCount(); steps != 1 {
		t.Fatalf("planner Next calls = %d, want 1 (a timed-out pause must not re-enter the planner)", steps)
	}
}

// TestRun_PauseTimeout_BusSubscribeFailure_WarnsAndFallsBack pins the
// degradation contract on the park's bus-subscription failure (Wave C
// checkpoint audit): a CLOSED bus must not error the park — the run
// falls back to the Status re-check ticker (and still terminates
// timeout-terminal), and the degradation is surfaced at Warn with the
// run's identity attributes (CLAUDE.md §5 — unexpected but recovered,
// never silent).
func TestRun_PauseTimeout_BusSubscribeFailure_WarnsAndFallsBack(t *testing.T) {
	red := patternsAudit.New()
	bus := mkBridgeBus(t, red)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close(bus): %v", err)
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	reg := NewRegistry()
	coord := &stubCoordinator{statusTimeoutAfterCalls: 3}
	rl, err := NewRunLoop(reg, coord,
		WithRunLoopBus(bus), // closed: Subscribe fails at park entry
		WithRunLoopLogger(logger),
		WithPauseStatusRecheckInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

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

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v (a failed bus subscription must degrade, not error)", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishConstraintsConflict)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run with a failed bus subscription did not terminate via the re-check fallback")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "could not subscribe to the bus") {
		t.Fatalf("no Warn for the degraded park; log output:\n%s", logged)
	}
	if !strings.Contains(logged, "run_id="+runA.RunID) || !strings.Contains(logged, "tenant_id="+runA.TenantID) {
		t.Fatalf("Warn line missing identity attributes; log output:\n%s", logged)
	}
}
