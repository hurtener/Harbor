package steering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

// pausingPlanner emits RequestPause exactly once (step 0), then Finish
// on every later step. It is the unit-test analogue of the
// deterministic PauseStep — the §13 emitting consumer shape.
type pausingPlanner struct {
	scriptedPlanner
	reason planner.PauseReason
}

func (p *pausingPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	p.seenRC = append(p.seenRC, rc)
	step := len(p.seenRC)
	p.mu.Unlock()
	if step == 1 {
		return planner.RequestPause{
			Reason:  p.reason,
			Payload: map[string]any{"gate": "approval"},
		}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// TestRun_RequestPause_RoutesThroughCoordinator asserts the RunLoop
// routes a planner's RequestPause through Coordinator.Request, blocks,
// and resumes via Coordinator.Resume when an APPROVE arrives. This is
// the §13 round-trip exercised at the unit layer against a stub
// Coordinator (the integration test exercises it against the REAL
// Coordinator + a real checkpoint store).
func TestRun_RequestPause_RoutesThroughCoordinator(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &pausingPlanner{reason: planner.PauseApprovalRequired}

	done := make(chan error, 1)
	go func() {
		fin, err := rl.Run(context.Background(), runSpecFor(runA, p))
		if err == nil && fin.Reason != planner.FinishGoal {
			err = errors.New("expected Finish{Goal} after resume")
		}
		done <- err
	}()

	// Wait for the loop to reach the pause boundary (Coordinator.Request
	// recorded). Bounded eventually-style wait.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if req, _ := coord.snapshot(); req >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if req, _ := coord.snapshot(); req != 1 {
		t.Fatalf("Coordinator.Request calls = %d, want 1 (RunLoop did not route RequestPause through the Coordinator)", req)
	}

	// Enqueue an APPROVE — the RunLoop's next drain calls
	// Coordinator.Resume and re-enters the planner.
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
		t.Fatalf("Enqueue(APPROVE): %v", eqErr)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after APPROVE: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish within 2s after APPROVE — the pause→Resume→re-enter round-trip did not close")
	}
	if _, res := coord.snapshot(); res != 1 {
		t.Errorf("Coordinator.Resume calls = %d, want 1", res)
	}
}

// TestRun_RequestPause_RejectTerminatesRun asserts a REJECT advances the
// pause and terminates the run with Finish{ConstraintsConflict}.
func TestRun_RequestPause_RejectTerminatesRun(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &pausingPlanner{reason: planner.PauseApprovalRequired}

	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, err := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- struct {
			fin planner.Finish
			err error
		}{fin, err}
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
		Type:         ControlReject,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"why": "bad plan"},
	}); eqErr != nil {
		t.Fatalf("Enqueue(REJECT): %v", eqErr)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run after REJECT: %v", res.err)
		}
		if res.fin.Reason != planner.FinishConstraintsConflict {
			t.Errorf("Finish.Reason = %q after REJECT, want %q", res.fin.Reason, planner.FinishConstraintsConflict)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish within 2s after REJECT")
	}
	// The REJECT still called Coordinator.Resume (with rejected:true).
	coord.mu.Lock()
	pay := coord.lastResumePay
	coord.mu.Unlock()
	if rejected, _ := pay["rejected"].(bool); !rejected {
		t.Error("REJECT did not stamp rejected:true into the Coordinator.Resume payload")
	}
}

// TestRun_RequestPause_CoordinatorRequestError_Propagates asserts a
// Coordinator.Request failure (e.g. a non-serialisable payload) fails
// the run loud — no silent degradation.
func TestRun_RequestPause_CoordinatorRequestError_Propagates(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	reqErr := errors.New("payload not serialisable")
	coord := &stubCoordinator{requestErr: reqErr}
	rl, err := NewRunLoop(reg, coord, WithRunLoopClock(clk))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	p := &pausingPlanner{reason: planner.PauseApprovalRequired}
	_, runErr := rl.Run(context.Background(), runSpecFor(runA, p))
	if !errors.Is(runErr, reqErr) {
		t.Fatalf("Run err = %v, want it to wrap the Coordinator.Request error", runErr)
	}
}

// TestRun_CancelWhilePaused_TerminatesRun asserts a CANCEL that arrives
// while a run is paused terminates it with Finish{Cancelled} — there is
// no point waiting for a resume that will never come.
func TestRun_CancelWhilePaused_TerminatesRun(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	p := &pausingPlanner{reason: planner.PauseApprovalRequired}

	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, err := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- struct {
			fin planner.Finish
			err error
		}{fin, err}
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
		Type:         ControlCancel,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"hard": false},
	}); eqErr != nil {
		t.Fatalf("Enqueue(CANCEL): %v", eqErr)
	}
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Run after CANCEL-while-paused: %v", res.err)
		}
		if res.fin.Reason != planner.FinishCancelled {
			t.Errorf("Finish.Reason = %q, want %q (CANCEL while paused terminates the run)", res.fin.Reason, planner.FinishCancelled)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish within 2s after CANCEL-while-paused")
	}
}

// TestRun_InjectContextWhilePaused_SurvivesToNextStep asserts a
// non-resume control applied while a run is paused (INJECT_CONTEXT)
// accumulates onto the run's base and reaches the planner on the step
// after the eventual RESUME.
func TestRun_InjectContextWhilePaused_SurvivesToNextStep(t *testing.T) {
	rl, reg, coord := newTestRunLoop(t)
	// A planner that pauses on step 0, then on step 1 (post-resume)
	// records the Control it sees and finishes.
	p := &pauseThenObservePlanner{}

	done := make(chan error, 1)
	go func() {
		_, err := rl.Run(context.Background(), runSpecFor(runA, p))
		done <- err
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
	// While paused: an INJECT_CONTEXT (does NOT resume the pause).
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlInjectContext,
		Identity:     runA,
		CallerScope:  ScopeSessionUser,
		CallerTenant: runA.TenantID,
		Payload:      map[string]any{"note": "injected during pause"},
	}); eqErr != nil {
		t.Fatalf("Enqueue(INJECT_CONTEXT): %v", eqErr)
	}
	// Then a RESUME.
	if eqErr := in.Enqueue(ControlEvent{
		Type:         ControlResume,
		Identity:     runA,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: runA.TenantID,
	}); eqErr != nil {
		t.Fatalf("Enqueue(RESUME): %v", eqErr)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish within 2s")
	}
	got := p.observedControl()
	if len(got.InjectedContext) != 1 || got.InjectedContext[0]["note"] != "injected during pause" {
		t.Errorf("post-resume step did not see the context injected during the pause: %+v", got.InjectedContext)
	}
}

type pauseThenObservePlanner struct {
	scriptedPlanner
	observed planner.ControlSignals
	step     int
}

func (p *pauseThenObservePlanner) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.step == 0 {
		p.step++
		return planner.RequestPause{Reason: planner.PauseApprovalRequired}, nil
	}
	p.observed = rc.Control
	p.step++
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (p *pauseThenObservePlanner) observedControl() planner.ControlSignals {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.observed
}

// ---------------------------------------------------------------------------
// Options + ControlHistory + lifecycle-event emission.
// ---------------------------------------------------------------------------

func TestRunLoopOptions_AreApplied(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := &stubCoordinator{}
	tr := &stubTaskRegistry{}
	bus := &recordingBus{}
	hookCalled := false
	hook := func(context.Context, string) error { hookCalled = true; return nil }

	rl, err := NewRunLoop(reg, coord,
		WithTaskRegistry(tr),
		WithRunLoopBus(bus),
		WithHardCancelHook(hook),
		WithRunLoopClock(clk),
		WithMaxControlHistory(7),
	)
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	if rl.applier.taskRegistry == nil {
		t.Error("WithTaskRegistry not applied")
	}
	if rl.applier.hardCancelHook == nil {
		t.Error("WithHardCancelHook not applied")
	}
	if rl.bus == nil {
		t.Error("WithRunLoopBus not applied")
	}
	if rl.history.cap != 7 {
		t.Errorf("WithMaxControlHistory not applied: cap = %d, want 7", rl.history.cap)
	}
	// Fire the hook to mark it covered.
	_ = rl.applier.hardCancel(context.Background(), "run-x")
	if !hookCalled {
		t.Error("hard-cancel hook was not invoked")
	}
}

// recordingBus is a minimal EventBus that records published events.
type recordingBus struct {
	mu        sync.Mutex
	published []events.Event
}

func (b *recordingBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, ev)
	return nil
}
func (b *recordingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("not implemented")
}
func (b *recordingBus) Close(context.Context) error { return nil }

func (b *recordingBus) typesPublished() []events.EventType {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]events.EventType, 0, len(b.published))
	for _, ev := range b.published {
		out = append(out, ev.Type)
	}
	return out
}

func TestRun_EmitsControlLifecycleEvents(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := &stubCoordinator{}
	bus := &recordingBus{}
	rl, err := NewRunLoop(reg, coord, WithRunLoopClock(clk), WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	// A wrapper that enqueues a REDIRECT after step 0, so a drained
	// control event flows through and emits control.received +
	// control.applied.
	inner := &scriptedPlanner{
		script: []scriptStep{
			{dec: planner.CallTool{Tool: "noop"}},
			{dec: planner.Finish{Reason: planner.FinishGoal}},
		},
	}
	enq := &enqueueOnStepPlanner{inner: inner, reg: reg, q: runA, onStep: 0}
	if _, err := rl.Run(context.Background(), runSpecFor(runA, enq)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	types := bus.typesPublished()
	var sawReceived, sawApplied bool
	for _, ty := range types {
		if ty == EventTypeControlReceived {
			sawReceived = true
		}
		if ty == EventTypeControlApplied {
			sawApplied = true
		}
	}
	if !sawReceived {
		t.Error("RunLoop did not emit control.received for a drained control event")
	}
	if !sawApplied {
		t.Error("RunLoop did not emit control.applied for an applied control event")
	}
}

func TestRun_RecordsControlHistory(t *testing.T) {
	rl, reg, _ := newTestRunLoop(t)
	inner := &scriptedPlanner{
		script: []scriptStep{
			{dec: planner.CallTool{Tool: "noop"}},
			{dec: planner.Finish{Reason: planner.FinishCancelled}},
		},
	}
	enq := &enqueueOnStepPlanner{inner: inner, reg: reg, q: runA, onStep: 0}
	if _, err := rl.Run(context.Background(), runSpecFor(runA, enq)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	hist := rl.ControlHistory(runA.SessionID)
	if len(hist) != 1 {
		t.Fatalf("ControlHistory len = %d, want 1 (the drained CANCEL)", len(hist))
	}
	if hist[0].Type != ControlCancel {
		t.Errorf("history[0].Type = %q, want CANCEL", hist[0].Type)
	}
	if hist[0].RunID != runA.RunID {
		t.Errorf("history[0].RunID = %q, want %q", hist[0].RunID, runA.RunID)
	}
}

func TestMergeSignals_FoldsCarryOver(t *testing.T) {
	carry := planner.ControlSignals{
		Cancelled:       false,
		InjectedContext: []map[string]any{{"a": 1}},
		UserMessages:    []string{"carry msg"},
		RedirectGoal:    "carry goal",
	}
	fresh := planner.ControlSignals{
		PauseRequested:  true,
		InjectedContext: []map[string]any{{"b": 2}},
		UserMessages:    []string{"fresh msg"},
	}
	out := mergeSignals(carry, fresh)
	if !out.PauseRequested {
		t.Error("merged signals lost fresh.PauseRequested")
	}
	if len(out.InjectedContext) != 2 {
		t.Errorf("merged InjectedContext len = %d, want 2 (carry + fresh)", len(out.InjectedContext))
	}
	// Carry-over comes first (FIFO order preserved).
	if out.InjectedContext[0]["a"] != 1 {
		t.Error("merged InjectedContext does not preserve carry-first FIFO order")
	}
	if len(out.UserMessages) != 2 || out.UserMessages[0] != "carry msg" {
		t.Errorf("merged UserMessages = %v, want [carry msg fresh msg]", out.UserMessages)
	}
	// RedirectGoal: fresh empty → carry-over used.
	if out.RedirectGoal != "carry goal" {
		t.Errorf("merged RedirectGoal = %q, want carry goal (fresh was empty)", out.RedirectGoal)
	}
}

func TestMergeAccumulatedSignals_PersistsOntoBase(t *testing.T) {
	base := &planner.RunContext{}
	sc := &stepControl{goal: "redirected"}
	sc.signals.InjectedContext = []map[string]any{{"x": 1}}
	mergeAccumulatedSignals(base, sc)
	if base.Goal != "redirected" {
		t.Errorf("base.Goal = %q, want redirected", base.Goal)
	}
	if len(base.Control.InjectedContext) != 1 {
		t.Errorf("base.Control.InjectedContext len = %d, want 1", len(base.Control.InjectedContext))
	}
}

// keep tasks import used by stubTaskRegistry typing assertions.
var _ tasks.TaskRegistry = (*stubTaskRegistry)(nil)
