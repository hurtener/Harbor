package steering

// D-192 mid-step dispatch tests — the per-step goroutine + mid-step
// inbox drain that lets a planner-dispatched approval-gated tool
// resume. The gate / coordinator / bus / redactor on the seam are the
// REAL artifacts (the bridge_test.go fixtures); only the planner and
// the ToolExecutor are test-local scripts, which is appropriate for
// unit tests of the RunLoop's control flow (the full-stack wiring —
// real catalog + Phase 64a builder + deterministic planner — lives in
// test/integration/approval_midstep_test.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// dispatchTestTimeout bounds every channel receive in this file — a
// real-time deadline, never a synchronisation sleep.
const dispatchTestTimeout = 5 * time.Second

// gatedExecutor is a minimal test-local ToolExecutor whose CallTool
// dispatch routes through a REAL approval gate's RunGuarded — the
// exact shape the production approval wrapper composes
// (catalog.WrapWithApproval → gate.RunGuarded → inner Invoke).
type gatedExecutor struct {
	gate *approval.ApprovalGate
	ran  atomic.Bool
}

func (e *gatedExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, decision planner.Decision) (any, any, error) {
	ct, ok := decision.(planner.CallTool)
	if !ok {
		return nil, nil, fmt.Errorf("gatedExecutor: unexpected decision %T", decision)
	}
	args, err := e.gate.RunGuarded(ctx, &approval.ApprovalRequest{
		Tool:     tools.Tool{Name: ct.Tool},
		Args:     ct.Args,
		Identity: rc.Quadruple.Identity,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("gatedExecutor: gate: %w", err)
	}
	e.ran.Store(true)
	obs := map[string]any{"approved_args": string(args)}
	return obs, obs, nil
}

// blockingExecutor parks its FIRST invocation until released (or ctx
// cancels); subsequent invocations return immediately. It lets a test
// deterministically hold one step's decision execution in flight while
// controls are enqueued.
type blockingExecutor struct {
	calls   atomic.Int64
	entered chan struct{} // closed when the first ExecuteDecision is entered
	release chan struct{} // close to unblock the first invocation
}

func (e *blockingExecutor) ExecuteDecision(ctx context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
	if e.calls.Add(1) > 1 {
		return "immediate", "immediate", nil
	}
	close(e.entered)
	select {
	case <-e.release:
		return "released", "released", nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// midStepQ builds a per-test run quadruple on the bridge-test identity
// so the bridge fixtures (gate / bus / coordinator) compose directly.
func midStepQ(runID string) identity.Quadruple {
	return identity.Quadruple{Identity: bridgeTestID, RunID: runID}
}

// enqueueApprovalControl enqueues an APPROVE / REJECT carrying the
// gate's wire token onto the run's inbox — the same path the Protocol
// edge's dispatchControl uses.
func enqueueApprovalControl(t *testing.T, reg *Registry, q identity.Quadruple, typ ControlType, token string) {
	t.Helper()
	in, err := reg.Lookup(q)
	if err != nil {
		t.Fatalf("Registry.Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type:         typ,
		Identity:     q,
		CallerScope:  ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"token": token, "reason": "dispatch-test"},
	}); err != nil {
		t.Fatalf("Inbox.Enqueue(%s): %v", typ, err)
	}
}

// assertGoroutineBaseline polls (bounded) until the goroutine count is
// back within a small tolerance of the captured baseline.
func assertGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak after dispatch: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}

// TestRun_MidStepApprove_UnblocksGatedDecision — the D-192 canonical
// path. A planner-dispatched CallTool parks inside a REAL gate's
// RunGuarded; an APPROVE enqueued on the steering inbox (the Protocol
// edge's path) is drained MID-STEP and routed through the D-097
// bridge; the gated decision unblocks; the run finishes. Before D-192
// this deadlocked: the synchronous ExecuteDecision blocked the only
// goroutine that drains the inbox.
func TestRun_MidStepApprove_UnblocksGatedDecision(t *testing.T) {
	baseline := runtime.NumGoroutine()
	fx := mkBridgeFixture(t)
	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	reg := NewRegistry()
	rl, err := NewRunLoop(reg, fx.coord,
		WithApprovalGates(map[string]*approval.ApprovalGate{"gated-tool": fx.gate}))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := midStepQ("run-midstep-approve")
	exec := &gatedExecutor{gate: fx.gate}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "gated-tool", Args: json.RawMessage(`{"input":"midstep"}`)}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	spec := runSpecFor(q, p)
	spec.ToolExecutor = exec
	spec.Base.Trajectory = &planner.Trajectory{}

	type runResult struct {
		fin planner.Finish
		err error
	}
	resCh := make(chan runResult, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), spec)
		resCh <- runResult{fin: fin, err: rerr}
	}()

	token := waitForApprovalRequested(t, sub)
	enqueueApprovalControl(t, reg, q, ControlApprove, string(token))

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("Run: %v", res.err)
		}
		if res.fin.Reason != planner.FinishGoal {
			t.Errorf("Finish.Reason = %q, want %q", res.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(dispatchTestTimeout):
		t.Fatal("Run did not finish after mid-step APPROVE — the D-192 mid-step drain did not fire")
	}
	if !exec.ran.Load() {
		t.Error("gated tool body did not run after APPROVE")
	}
	// The observation the planner would see carries the ORIGINAL args.
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 1 {
		t.Fatalf("trajectory steps = %d, want 1", len(steps))
	}
	obs, ok := steps[0].Observation.(map[string]any)
	if !ok || obs["approved_args"] != `{"input":"midstep"}` {
		t.Errorf("step observation = %#v, want approved_args round-trip", steps[0].Observation)
	}
	// The mid-step-consumed APPROVE landed in the control history
	// exactly once with no error — and was NOT re-applied at the next
	// boundary (a re-apply would have failed the run loud with
	// ErrNoOutstandingPause).
	var approves int
	for _, ac := range rl.ControlHistory(q.SessionID) {
		if ac.Type == ControlApprove {
			approves++
			if ac.Err != nil {
				t.Errorf("APPROVE history entry carries err %v, want nil", ac.Err)
			}
		}
	}
	if approves != 1 {
		t.Errorf("APPROVE history entries = %d, want exactly 1 (consumed mid-step is consumed)", approves)
	}
	assertGoroutineBaseline(t, baseline)
}

// TestRun_MidStepReject_SurfacesRejectionObservation — a mid-step
// REJECT unblocks the gated decision with *approval.ErrToolRejected;
// the tool body never runs; the rejection surfaces as the step's
// error-shaped observation and the planner re-plans (here: finishes).
func TestRun_MidStepReject_SurfacesRejectionObservation(t *testing.T) {
	fx := mkBridgeFixture(t)
	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	reg := NewRegistry()
	rl, err := NewRunLoop(reg, fx.coord,
		WithApprovalGates(map[string]*approval.ApprovalGate{"gated-tool": fx.gate}))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := midStepQ("run-midstep-reject")
	exec := &gatedExecutor{gate: fx.gate}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "gated-tool", Args: json.RawMessage(`{"input":"reject-me"}`)}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	spec := runSpecFor(q, p)
	spec.ToolExecutor = exec
	spec.Base.Trajectory = &planner.Trajectory{}

	resCh := make(chan error, 1)
	go func() {
		_, rerr := rl.Run(context.Background(), spec)
		resCh <- rerr
	}()

	token := waitForApprovalRequested(t, sub)
	enqueueApprovalControl(t, reg, q, ControlReject, string(token))

	select {
	case rerr := <-resCh:
		if rerr != nil {
			t.Fatalf("Run: %v", rerr)
		}
	case <-time.After(dispatchTestTimeout):
		t.Fatal("Run did not finish after mid-step REJECT")
	}
	if exec.ran.Load() {
		t.Error("gated tool body ran despite REJECT")
	}
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 1 {
		t.Fatalf("trajectory steps = %d, want 1", len(steps))
	}
	obs, ok := steps[0].Observation.(map[string]any)
	if !ok {
		t.Fatalf("step observation type = %T, want map[string]any error payload", steps[0].Observation)
	}
	msg, _ := obs["error"].(string)
	if !strings.Contains(msg, "rejected") {
		t.Errorf("rejection observation = %q, want it to name the rejection", msg)
	}
}

// TestRun_MidStepDrain_DefersNonApprovalControls_AppliedOnceAtNextBoundary
// — controls that are NOT approval-bridge-eligible keep their
// step-boundary semantics: drained mid-step, they are deferred and
// applied exactly once at the next boundary (no drop, no double-apply).
func TestRun_MidStepDrain_DefersNonApprovalControls_AppliedOnceAtNextBoundary(t *testing.T) {
	rl, reg, _ := newTestRunLoop(t)

	q := midStepQ("run-midstep-defer")
	exec := &blockingExecutor{entered: make(chan struct{}), release: make(chan struct{})}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "noop"}},
		{dec: planner.CallTool{Tool: "noop2"}},
		{dec: planner.Finish{Reason: planner.FinishGoal}},
	}}
	spec := runSpecFor(q, p)
	spec.ToolExecutor = exec

	resCh := make(chan error, 1)
	go func() {
		_, rerr := rl.Run(context.Background(), spec)
		resCh <- rerr
	}()

	// Hold step 0's execution in flight, then enqueue two non-approval
	// controls. The mid-step drain MUST defer them (not consume them).
	select {
	case <-exec.entered:
	case <-time.After(dispatchTestTimeout):
		t.Fatal("executor was never entered")
	}
	in, err := reg.Lookup(q)
	if err != nil {
		t.Fatalf("Registry.Lookup: %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type: ControlInjectContext, Identity: q,
		CallerScope: ScopeSessionUser, CallerTenant: q.TenantID,
		Payload: map[string]any{"note": "mid-step"},
	}); err != nil {
		t.Fatalf("Enqueue(INJECT_CONTEXT): %v", err)
	}
	if err := in.Enqueue(ControlEvent{
		Type: ControlUserMessage, Identity: q,
		CallerScope: ScopeSessionUser, CallerTenant: q.TenantID,
		Payload: map[string]any{"message": "hello mid-step"},
	}); err != nil {
		t.Fatalf("Enqueue(USER_MESSAGE): %v", err)
	}
	// Wait (bounded) for the mid-step drain to have picked the events
	// off the inbox — proving the deferral path (not the boundary
	// drain) is what carried them.
	deadline := time.Now().Add(2 * time.Second)
	for in.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := in.Len(); got != 0 {
		t.Fatalf("inbox.Len() = %d after mid-step drain window, want 0 (mid-step drain never fired)", got)
	}
	close(exec.release)

	select {
	case rerr := <-resCh:
		if rerr != nil {
			t.Fatalf("Run: %v", rerr)
		}
	case <-time.After(dispatchTestTimeout):
		t.Fatal("Run did not finish")
	}

	// Step 0 saw no signals (enqueued after step 0's Next). Step 1 —
	// the first boundary after the mid-step drain — sees BOTH deferred
	// controls. Step 2 sees neither (applied exactly once).
	if c := p.controlAt(0); len(c.InjectedContext) != 0 || len(c.UserMessages) != 0 {
		t.Errorf("step 0 control = %+v, want empty", c)
	}
	c1 := p.controlAt(1)
	if len(c1.InjectedContext) != 1 || len(c1.UserMessages) != 1 {
		t.Errorf("step 1 control = %+v, want 1 injected context + 1 user message (deferred mid-step controls applied at the next boundary)", c1)
	}
	c2 := p.controlAt(2)
	if len(c2.InjectedContext) != 0 || len(c2.UserMessages) != 0 {
		t.Errorf("step 2 control = %+v, want empty (no double-apply of deferred controls)", c2)
	}
}

// TestRun_CancelWhileGatedMidStep_AbortsCleanly — cancelling the run
// ctx while a gated decision is parked aborts the in-flight RunGuarded
// (it honours ctx), joins the per-step goroutine, and surfaces the
// cancellation at the next step boundary. No goroutine outlives the
// run.
func TestRun_CancelWhileGatedMidStep_AbortsCleanly(t *testing.T) {
	baseline := runtime.NumGoroutine()
	fx := mkBridgeFixture(t)
	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	reg := NewRegistry()
	rl, err := NewRunLoop(reg, fx.coord,
		WithApprovalGates(map[string]*approval.ApprovalGate{"gated-tool": fx.gate}))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := midStepQ("run-midstep-cancel")
	exec := &gatedExecutor{gate: fx.gate}
	p := &scriptedPlanner{script: []scriptStep{
		{dec: planner.CallTool{Tool: "gated-tool", Args: json.RawMessage(`{"input":"cancel-me"}`)}},
	}}
	spec := runSpecFor(q, p)
	spec.ToolExecutor = exec

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resCh := make(chan error, 1)
	go func() {
		_, rerr := rl.Run(ctx, spec)
		resCh <- rerr
	}()

	// Wait until the gated decision is parked, then cancel the run.
	_ = waitForApprovalRequested(t, sub)
	cancel()

	select {
	case rerr := <-resCh:
		if !errors.Is(rerr, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", rerr)
		}
	case <-time.After(dispatchTestTimeout):
		t.Fatal("Run did not return after ctx cancel while gated — the in-flight gated decision was not aborted")
	}
	if exec.ran.Load() {
		t.Error("gated tool body ran despite cancellation")
	}
	assertGoroutineBaseline(t, baseline)
}
