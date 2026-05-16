package steering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// ---------------------------------------------------------------------------
// Test stubs. Per CLAUDE.md §17, integration tests use real drivers at the
// seam — but these are UNIT tests of the RunLoop's control flow in
// isolation, so narrow stubs that record calls are appropriate here. The
// real-driver wiring is exercised in test/integration/phase53_*.
// ---------------------------------------------------------------------------

// scriptedPlanner returns a pre-scripted sequence of Decisions, one per
// Next call. It records every RunContext it was handed so a test can
// assert what RunContext.Control the planner observed.
type scriptedPlanner struct {
	mu         sync.Mutex
	script     []scriptStep
	idx        int
	seenRC     []planner.RunContext
	defaultDec planner.Decision // returned once the script is exhausted
}

type scriptStep struct {
	dec planner.Decision
	err error
}

func (p *scriptedPlanner) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seenRC = append(p.seenRC, rc)
	if p.idx < len(p.script) {
		step := p.script[p.idx]
		p.idx++
		return step.dec, step.err
	}
	if p.defaultDec != nil {
		return p.defaultDec, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (p *scriptedPlanner) controlAt(step int) planner.ControlSignals {
	p.mu.Lock()
	defer p.mu.Unlock()
	if step >= len(p.seenRC) {
		return planner.ControlSignals{}
	}
	return p.seenRC[step].Control
}

func (p *scriptedPlanner) stepCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seenRC)
}

// stubCoordinator records Request / Resume calls and returns scripted
// results. It is NOT a re-implementation of pause coordination — it just
// records that the RunLoop reached the ONE Coordinator.
type stubCoordinator struct {
	mu                 sync.Mutex
	requestCalls       int
	resumeCalls        int
	lastResumePay      map[string]any
	lastResumeDecision pauseresume.Decision
	issueToken         pauseresume.Token
	requestErr         error
	resumeErr          error
	resumedTokens      []pauseresume.Token
}

func (c *stubCoordinator) Request(_ context.Context, req pauseresume.PauseRequest) (pauseresume.Pause, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestCalls++
	if c.requestErr != nil {
		return pauseresume.Pause{}, c.requestErr
	}
	tok := c.issueToken
	if tok == "" {
		tok = pauseresume.Token("stub-token")
	}
	return pauseresume.Pause{Token: tok, Reason: req.Reason, Identity: req.Identity}, nil
}

func (c *stubCoordinator) Resume(_ context.Context, token pauseresume.Token, decision pauseresume.Decision, payload map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resumeCalls++
	c.lastResumePay = payload
	c.lastResumeDecision = decision
	c.resumedTokens = append(c.resumedTokens, token)
	return c.resumeErr
}

func (c *stubCoordinator) Status(_ context.Context, _ pauseresume.Token) (pauseresume.Status, error) {
	return pauseresume.Status{}, nil
}

func (c *stubCoordinator) snapshot() (req, res int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requestCalls, c.resumeCalls
}

// newTestRunLoop builds a RunLoop with a stub Coordinator and a fake
// clock. Returns the RunLoop, the Registry (so the test can enqueue
// events), and the stub Coordinator.
func newTestRunLoop(t *testing.T, opts ...RunLoopOption) (*RunLoop, *Registry, *stubCoordinator) {
	t.Helper()
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := &stubCoordinator{}
	allOpts := append([]RunLoopOption{WithRunLoopClock(clk)}, opts...)
	rl, err := NewRunLoop(reg, coord, allOpts...)
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}
	return rl, reg, coord
}

// runSpecFor builds a minimal valid RunSpec for the given run quadruple.
func runSpecFor(q identity.Quadruple, p planner.Planner) RunSpec {
	return RunSpec{
		Planner: p,
		Base: planner.RunContext{
			Quadruple: q,
			Goal:      "test goal",
		},
		MaxSteps: 16,
	}
}

// ---------------------------------------------------------------------------
// Construction.
// ---------------------------------------------------------------------------

func TestNewRunLoop_NilRegistry_FailsLoud(t *testing.T) {
	_, err := NewRunLoop(nil, &stubCoordinator{})
	if !errors.Is(err, ErrRunLoopMisconfigured) {
		t.Fatalf("NewRunLoop(nil registry) err = %v, want ErrRunLoopMisconfigured", err)
	}
}

func TestNewRunLoop_NilCoordinator_FailsLoud(t *testing.T) {
	_, err := NewRunLoop(NewRegistry(), nil)
	if !errors.Is(err, ErrRunLoopMisconfigured) {
		t.Fatalf("NewRunLoop(nil coordinator) err = %v, want ErrRunLoopMisconfigured", err)
	}
}

// ---------------------------------------------------------------------------
// Run — basic control flow.
// ---------------------------------------------------------------------------

func TestRun_NilPlanner_FailsLoud(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	_, err := rl.Run(context.Background(), RunSpec{Base: planner.RunContext{Quadruple: runA}})
	if !errors.Is(err, ErrNoPlanner) {
		t.Fatalf("Run(nil planner) err = %v, want ErrNoPlanner", err)
	}
}

func TestRun_IncompleteIdentity_FailsClosed(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t"}, RunID: "r"}
	_, err := rl.Run(context.Background(), RunSpec{
		Planner: &scriptedPlanner{},
		Base:    planner.RunContext{Quadruple: bad},
	})
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("Run(incomplete identity) err = %v, want ErrIdentityRequired", err)
	}
}

func TestRun_PlannerFinishesImmediately(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	fin, err := rl.Run(context.Background(), runSpecFor(runA, p))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want %q", fin.Reason, planner.FinishGoal)
	}
	if p.stepCount() != 1 {
		t.Errorf("planner step count = %d, want 1", p.stepCount())
	}
}

func TestRun_RetiresInboxOnExit(t *testing.T) {
	rl, reg, _ := newTestRunLoop(t)
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	if _, err := rl.Run(context.Background(), runSpecFor(runA, p)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// After Run, the inbox is retired — a Lookup fails closed.
	if _, err := reg.Lookup(runA); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("after Run, Lookup err = %v, want ErrInboxNotFound (inbox not retired)", err)
	}
}

func TestRun_RetiresInboxEvenOnPlannerError(t *testing.T) {
	rl, reg, _ := newTestRunLoop(t)
	plannerErr := errors.New("planner blew up")
	p := &scriptedPlanner{script: []scriptStep{{err: plannerErr}}}
	_, err := rl.Run(context.Background(), runSpecFor(runA, p))
	if err == nil {
		t.Fatal("Run should have surfaced the planner error")
	}
	if !errors.Is(err, plannerErr) {
		t.Errorf("Run err = %v, want it to wrap the planner error", err)
	}
	if _, lerr := reg.Lookup(runA); !errors.Is(lerr, ErrInboxNotFound) {
		t.Errorf("after a failed Run, inbox not retired: Lookup err = %v", lerr)
	}
}

func TestRun_NilDecision_FailsLoud(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	p := &scriptedPlanner{script: []scriptStep{{dec: nil, err: nil}}}
	_, err := rl.Run(context.Background(), runSpecFor(runA, p))
	if err == nil {
		t.Fatal("Run should fail loud on a (nil, nil) planner return — silent degradation forbidden (§13)")
	}
}

func TestRun_MaxStepsExceeded(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	// A planner that never finishes — always returns CallTool.
	p := &scriptedPlanner{defaultDec: planner.CallTool{Tool: "noop"}}
	spec := runSpecFor(runA, p)
	spec.MaxSteps = 5
	_, err := rl.Run(context.Background(), spec)
	if !errors.Is(err, ErrMaxStepsExceeded) {
		t.Fatalf("Run err = %v, want ErrMaxStepsExceeded", err)
	}
	if p.stepCount() != 5 {
		t.Errorf("planner step count = %d, want 5 (MaxSteps)", p.stepCount())
	}
}

func TestRun_ContextCancelledAtBoundary(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal}}
	_, err := rl.Run(ctx, runSpecFor(runA, p))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(cancelled ctx) err = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Drain-between-steps: the planner sees the drained Control on the NEXT step.
// ---------------------------------------------------------------------------

func TestRun_DrainProjectsControlOntoNextStep(t *testing.T) {
	rl, reg, _ := newTestRunLoop(t)
	// Step 0: planner emits CallTool (so the loop continues to step 1).
	// Step 1: planner emits Finish.
	p := &scriptedPlanner{
		script: []scriptStep{
			{dec: planner.CallTool{Tool: "noop"}},
			{dec: planner.Finish{Reason: planner.FinishCancelled}},
		},
	}
	// Enqueue a CANCEL onto the run's inbox BEFORE Run starts. The
	// inbox is Opened inside Run, so we drive Run on a goroutine and
	// enqueue via the Registry once the inbox exists. Simpler: enqueue
	// AFTER step 0 by scripting — but the cleanest deterministic test is
	// to pre-open the inbox ourselves is not possible (Run Opens it).
	//
	// Instead: enqueue from a guard that the planner's step-0 Decide
	// triggers. We use a scripted planner whose step 0 enqueues a CANCEL
	// via a closure. Wrap the planner.
	enq := &enqueueOnStepPlanner{
		inner:  p,
		reg:    reg,
		q:      runA,
		onStep: 0, // after step 0's Next returns, enqueue a CANCEL
	}
	fin, err := rl.Run(context.Background(), runSpecFor(runA, enq))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fin.Reason != planner.FinishCancelled {
		t.Errorf("Finish.Reason = %q, want %q", fin.Reason, planner.FinishCancelled)
	}
	// Step 0's RunContext.Control must be empty — the CANCEL was
	// enqueued AFTER step 0's Next. Step 1's Control must show Cancelled.
	if p.controlAt(0).Cancelled {
		t.Error("step 0 saw Cancelled=true — a control enqueued after step 0 leaked into step 0 (drain-between-steps violated)")
	}
	if !p.controlAt(1).Cancelled {
		t.Error("step 1 did not see Cancelled=true — the drained CANCEL was not projected onto the next step")
	}
}

// enqueueOnStepPlanner wraps a planner and enqueues a CANCEL onto the
// run's inbox immediately after a given step's Next returns. This
// deterministically simulates "a control event arrived between steps"
// without any sleep / race.
type enqueueOnStepPlanner struct {
	inner  *scriptedPlanner
	reg    *Registry
	q      identity.Quadruple
	onStep int
	step   int
}

func (e *enqueueOnStepPlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	dec, err := e.inner.Next(ctx, rc)
	if e.step == e.onStep {
		in, lerr := e.reg.Lookup(e.q)
		if lerr == nil {
			_ = in.Enqueue(ControlEvent{
				Type:         ControlCancel,
				Identity:     e.q,
				CallerScope:  ScopeOwnerUser,
				CallerTenant: e.q.TenantID,
				Payload:      map[string]any{"hard": false},
			})
		}
	}
	e.step++
	return dec, err
}

// ---------------------------------------------------------------------------
// Inbox.WaitForEvent — the non-busy-wait surface the RunLoop uses while
// a pause is outstanding.
// ---------------------------------------------------------------------------

func TestInbox_WaitForEvent_UnblocksOnEnqueue(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- in.WaitForEvent(context.Background()) }()
	// Enqueue from another goroutine — WaitForEvent must unblock.
	if eqErr := in.Enqueue(validEvent(runA)); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("WaitForEvent err = %v, want nil", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForEvent did not unblock within 2s after Enqueue")
	}
}

func TestInbox_WaitForEvent_ReturnsImmediatelyWhenQueued(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if eqErr := in.Enqueue(validEvent(runA)); eqErr != nil {
		t.Fatalf("Enqueue: %v", eqErr)
	}
	// An already-queued event makes WaitForEvent return immediately.
	if werr := in.WaitForEvent(context.Background()); werr != nil {
		t.Fatalf("WaitForEvent with a queued event err = %v, want nil", werr)
	}
}

func TestInbox_WaitForEvent_UnblocksOnRetire(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- in.WaitForEvent(context.Background()) }()
	if rerr := reg.Retire(runA); rerr != nil {
		t.Fatalf("Retire: %v", rerr)
	}
	select {
	case werr := <-done:
		if !errors.Is(werr, ErrInboxNotFound) {
			t.Fatalf("WaitForEvent after Retire err = %v, want ErrInboxNotFound", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForEvent did not unblock within 2s after Retire")
	}
}

func TestInbox_WaitForEvent_HonoursContextCancel(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- in.WaitForEvent(ctx) }()
	cancel()
	select {
	case werr := <-done:
		if !errors.Is(werr, context.Canceled) {
			t.Fatalf("WaitForEvent err = %v, want context.Canceled", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForEvent did not unblock within 2s after ctx cancel")
	}
}
