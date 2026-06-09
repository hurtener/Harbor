// D-192 integration test — planner-dispatched approval-gated CallTool
// resumes via the steering inbox (the deadlock fix).
//
// Before D-192 this exact choreography deadlocked: the run-loop
// goroutine called ToolExecutor.ExecuteDecision synchronously
// (runloop.go), the approval wrapper's Invoke parked inside
// gate.RunGuarded until ResolveApproval, and the ONLY production
// caller of ResolveApproval was the D-097 bridge — fired from the
// run loop's own inbox drain, which the blocked step was preventing.
// No working HITL choreography existed anywhere in the tree for a
// planner-dispatched gated tool. D-192 dispatches the decision on a
// per-step goroutine and keeps draining the inbox mid-step, routing
// approval-bridge-eligible APPROVE / REJECT controls through the
// (unchanged) D-097 bridge.
//
// Real artifacts on every seam (§17.3 — no mocks at the boundary):
//
//   - real `tools.NewCatalog` + the Phase 64a `catalog.Builder`
//     applying a `deny-all` approval entry (`WrapWithApproval` in the
//     Invoke path),
//   - real `approval.ApprovalGate` (built by the Builder; surfaced
//     via `Deps.AppliedGates`),
//   - real `pauseresume.New` Coordinator,
//   - real in-mem `events.EventBus` + real patterns `audit.Redactor`,
//   - real `deterministic.DeterministicPlanner` emitting the CallTool,
//   - real `steering.Registry` + `steering.RunLoop` with
//     `WithApprovalGates`; the APPROVE / REJECT is enqueued on the
//     run's steering Inbox — the same path the Protocol edge's
//     dispatchControl uses.
//
// KNOWN PRODUCTION GAP (§17.6): the ONE stand-in below is the
// ToolExecutor. The production executor (`devToolExecutor`) is
// unexported in `cmd/harbor` (package main), so it cannot be driven
// from an integration test outside that package — the
// SDK-friction audit pinned this as Pattern 1 / P1
// (docs/notes/sdk-friction-audit.md); promoting the executor into an
// importable runtime package is a separate wave. The test-local
// executor below dispatches CallTool through the REAL catalog
// descriptor's Invoke (so the real approval wrapper + gate are in the
// invocation path), mirroring `devToolExecutor.callTool`'s
// resolve-then-Invoke shape minus the D-026 heavy-content projection
// (irrelevant to the deadlock under test).

package integration

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

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/deterministic"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

// d192ID is the canonical documented dummy identity for this test
// (CLAUDE.md §7 rule 2).
var d192ID = identity.Identity{
	TenantID:  "tenant-d192",
	UserID:    "user-d192",
	SessionID: "session-d192",
}

const d192Tool = "d192_guarded_echo"

// d192Timeout bounds every blocking receive — a real-time deadline,
// never a synchronisation sleep (§17.4).
const d192Timeout = 10 * time.Second

// d192Env bundles the real artifacts the test wires.
type d192Env struct {
	cat      tools.ToolCatalog
	bus      events.EventBus
	coord    pauseresume.Coordinator
	gates    map[string]*approval.ApprovalGate
	steerReg *steering.Registry
	runLoop  *steering.RunLoop
	// toolRuns counts underlying tool-body executions (post-gate).
	toolRuns *atomic.Int64
	// seenIdentity records the identity the tool body read from ctx —
	// the §17.3 identity-propagation probe.
	seenIdentity *atomic.Value
}

// buildD192Env wires the full real stack: catalog + Phase 64a builder
// (deny-all approval entry) + gate + coordinator + bus + redactor +
// steering registry + run loop.
func buildD192Env(t *testing.T) *d192Env {
	t.Helper()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	coord := pauseresume.New(pauseresume.WithBus(bus))

	cat := tools.NewCatalog()
	toolRuns := &atomic.Int64{}
	seenIdentity := &atomic.Value{}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        d192Tool,
			Description: "echo behind a deny-all approval gate (D-192 test)",
			Transport:   tools.TransportInProcess,
			Source:      "d192-test-source",
		},
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			toolRuns.Add(1)
			if id, ok := identity.From(ctx); ok {
				seenIdentity.Store(id)
			}
			return tools.ToolResult{Value: map[string]any{"echo": string(args)}}, nil
		},
	}); err != nil {
		t.Fatalf("catalog.Register: %v", err)
	}

	// Phase 64a builder: the deny-all approval entry wraps the tool
	// with the REAL gate via WrapWithApproval; the constructed gate
	// surfaces through AppliedGates — the same map production hands
	// the RunLoop (D-097).
	gates := map[string]*approval.ApprovalGate{}
	b := catalog.New([]config.ToolEntryConfig{
		{
			Name: d192Tool,
			Approval: &config.ToolApprovalConfig{
				Policy: "deny-all",
				Reason: "d192-integration",
			},
		},
	}, catalog.Deps{
		Catalog:      cat,
		Coordinator:  coord,
		Bus:          bus,
		Redactor:     red,
		AppliedGates: gates,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("catalog Builder.Apply: %v", err)
	}
	t.Cleanup(func() {
		for _, g := range gates {
			_ = g.Close(context.Background())
		}
	})

	steerReg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(steerReg, coord,
		steering.WithApprovalGates(gates),
		steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}
	return &d192Env{
		cat:          cat,
		bus:          bus,
		coord:        coord,
		gates:        gates,
		steerReg:     steerReg,
		runLoop:      rl,
		toolRuns:     toolRuns,
		seenIdentity: seenIdentity,
	}
}

// d192Executor is the test-local ToolExecutor (see the top-of-file
// KNOWN PRODUCTION GAP note — the production executor is unexported
// in cmd/harbor). It dispatches CallTool through the REAL catalog
// descriptor's Invoke, so the real approval wrapper + gate sit in the
// invocation path.
type d192Executor struct {
	cat tools.ToolCatalog
}

func (e *d192Executor) ExecuteDecision(ctx context.Context, _ planner.RunContext, decision planner.Decision) (any, any, error) {
	ct, ok := decision.(planner.CallTool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %T", steering.ErrDecisionShapeUnsupported, decision)
	}
	desc, ok := e.cat.Resolve(ct.Tool)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %q", tools.ErrToolNotFound, ct.Tool)
	}
	result, err := desc.Invoke(ctx, ct.Args)
	if err != nil {
		return nil, nil, fmt.Errorf("tool %q invoke: %w", ct.Tool, err)
	}
	return result.Value, result.Value, nil
}

// d192Planner builds a REAL deterministic planner: step 0 emits
// CallTool against the gated tool; once the trajectory carries that
// step's observation, the Finish step claims.
func d192Planner(t *testing.T) *deterministic.DeterministicPlanner {
	t.Helper()
	p, err := deterministic.NewDeterministicPlanner(deterministic.WithSteps(
		&deterministic.CallToolStep{
			Tool: d192Tool,
			ArgsBuilder: func(planner.RunContext) (json.RawMessage, error) {
				return json.RawMessage(`{"input":"d192"}`), nil
			},
			When: func(rc planner.RunContext) bool {
				return rc.Trajectory == nil || len(rc.Trajectory.Steps) == 0
			},
		},
		&deterministic.FinishStep{Reason: planner.FinishGoal},
	))
	if err != nil {
		t.Fatalf("deterministic.NewDeterministicPlanner: %v", err)
	}
	return p
}

// d192RunSpec builds the RunSpec for one run quadruple.
func d192RunSpec(env *d192Env, t *testing.T, q identity.Quadruple) steering.RunSpec {
	t.Helper()
	return steering.RunSpec{
		Planner: d192Planner(t),
		Base: planner.RunContext{
			Quadruple:  q,
			Goal:       "exercise the D-192 gated dispatch",
			Trajectory: &planner.Trajectory{},
		},
		MaxSteps:     8,
		ToolExecutor: &d192Executor{cat: env.cat},
	}
}

// d192SubscribeApprovalRequested opens the identity-filtered bus
// subscription for tool.approval_requested BEFORE the run starts so
// the publish is never raced.
func d192SubscribeApprovalRequested(t *testing.T, bus events.EventBus) (events.Subscription, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  d192ID.TenantID,
		User:    d192ID.UserID,
		Session: d192ID.SessionID,
		Types:   []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return sub, func() { sub.Cancel(); cancel() }
}

// d192WaitForToken blocks until tool.approval_requested arrives and
// returns the gate-minted pause token.
func d192WaitForToken(t *testing.T, sub events.Subscription) string {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before tool.approval_requested")
		}
		p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want ToolApprovalRequestedPayload", ev.Payload)
		}
		return p.PauseToken
	case <-time.After(d192Timeout):
		t.Fatal("timeout waiting for tool.approval_requested")
		return ""
	}
}

// d192Enqueue enqueues an APPROVE / REJECT carrying the gate token on
// the run's steering inbox — the identical path the Protocol edge's
// dispatchControl takes for the wire-side approve / reject methods.
func d192Enqueue(t *testing.T, reg *steering.Registry, q identity.Quadruple, typ steering.ControlType, token string) {
	t.Helper()
	in, err := reg.Lookup(q)
	if err != nil {
		t.Fatalf("steering.Registry.Lookup: %v", err)
	}
	if err := in.Enqueue(steering.ControlEvent{
		Type:         typ,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"token": token, "reason": "d192-integration"},
	}); err != nil {
		t.Fatalf("Inbox.Enqueue(%s): %v", typ, err)
	}
}

// d192AssertBaseline polls (bounded) until the goroutine count returns
// to within tolerance of baseline.
func d192AssertBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+4 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}

// TestE2E_ApprovalGatedCallTool_ApproveUnblocksRun — the canonical
// HITL choreography: a real deterministic planner emits CallTool
// against an approval-gated tool; the gate parks the dispatch via the
// unified Coordinator; an APPROVE enqueued on the steering inbox is
// drained MID-STEP (D-192) and routed through the D-097 bridge; the
// tool runs with the original args and the run finishes.
func TestE2E_ApprovalGatedCallTool_ApproveUnblocksRun(t *testing.T) {
	baseline := runtime.NumGoroutine()
	env := buildD192Env(t)
	sub, cancelSub := d192SubscribeApprovalRequested(t, env.bus)
	defer cancelSub()

	q := identity.Quadruple{Identity: d192ID, RunID: "run-d192-approve"}
	spec := d192RunSpec(env, t, q)

	type runResult struct {
		fin planner.Finish
		err error
	}
	resCh := make(chan runResult, 1)
	go func() {
		fin, err := env.runLoop.Run(context.Background(), spec)
		resCh <- runResult{fin: fin, err: err}
	}()

	token := d192WaitForToken(t, sub)
	d192Enqueue(t, env.steerReg, q, steering.ControlApprove, token)

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("Run: %v", res.err)
		}
		if res.fin.Reason != planner.FinishGoal {
			t.Errorf("Finish.Reason = %q, want %q", res.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(d192Timeout):
		t.Fatal("run did not finish after APPROVE — the approval-gated CallTool deadlocked (the pre-D-192 bug)")
	}

	if got := env.toolRuns.Load(); got != 1 {
		t.Errorf("tool body executions = %d, want 1", got)
	}
	// Identity propagation (§17.3): the tool body read the run's
	// triple from ctx through every layer (runloop → executor →
	// approval wrapper → descriptor Invoke).
	seen, _ := env.seenIdentity.Load().(identity.Identity)
	if seen != d192ID {
		t.Errorf("tool saw identity %+v, want %+v", seen, d192ID)
	}
	// The tool result flowed back onto the trajectory as the step's
	// observation.
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 1 {
		t.Fatalf("trajectory steps = %d, want 1", len(steps))
	}
	obs, ok := steps[0].Observation.(map[string]any)
	if !ok || obs["echo"] != `{"input":"d192"}` {
		t.Errorf("step observation = %#v, want the gated tool's echo of the ORIGINAL args", steps[0].Observation)
	}
	d192AssertBaseline(t, baseline)
}

// TestE2E_ApprovalGatedCallTool_RejectSurfacesRejection — the failure
// mode (§17.3): a REJECT enqueued on the steering inbox unblocks the
// gated dispatch with *approval.ErrToolRejected; the tool body never
// runs; the rejection surfaces as the step's error observation and the
// planner finishes; tool.rejected lands on the bus.
func TestE2E_ApprovalGatedCallTool_RejectSurfacesRejection(t *testing.T) {
	env := buildD192Env(t)
	sub, cancelSub := d192SubscribeApprovalRequested(t, env.bus)
	defer cancelSub()

	// Also watch for the gate's tool.rejected emission.
	rejCtx, rejCancel := context.WithCancel(context.Background())
	defer rejCancel()
	rejSub, err := env.bus.Subscribe(rejCtx, events.Filter{
		Tenant:  d192ID.TenantID,
		User:    d192ID.UserID,
		Session: d192ID.SessionID,
		Types:   []events.EventType{approval.EventTypeToolRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe(tool.rejected): %v", err)
	}
	defer rejSub.Cancel()

	q := identity.Quadruple{Identity: d192ID, RunID: "run-d192-reject"}
	spec := d192RunSpec(env, t, q)

	resCh := make(chan error, 1)
	go func() {
		_, rerr := env.runLoop.Run(context.Background(), spec)
		resCh <- rerr
	}()

	token := d192WaitForToken(t, sub)
	d192Enqueue(t, env.steerReg, q, steering.ControlReject, token)

	select {
	case rerr := <-resCh:
		if rerr != nil {
			t.Fatalf("Run: %v", rerr)
		}
	case <-time.After(d192Timeout):
		t.Fatal("run did not finish after REJECT")
	}

	if got := env.toolRuns.Load(); got != 0 {
		t.Errorf("tool body executions = %d, want 0 (rejected call must not run)", got)
	}
	steps := spec.Base.Trajectory.Steps
	if len(steps) != 1 {
		t.Fatalf("trajectory steps = %d, want 1", len(steps))
	}
	obs, ok := steps[0].Observation.(map[string]any)
	if !ok {
		t.Fatalf("observation type = %T, want map[string]any error payload", steps[0].Observation)
	}
	msg, _ := obs["error"].(string)
	if !strings.Contains(msg, "rejected") {
		t.Errorf("rejection observation = %q, want it to name the rejection", msg)
	}
	select {
	case ev := <-rejSub.Events():
		p, ok := ev.Payload.(approval.ToolRejectedPayload)
		if !ok {
			t.Fatalf("tool.rejected payload type = %T", ev.Payload)
		}
		if p.Tool != d192Tool {
			t.Errorf("tool.rejected names %q, want %q", p.Tool, d192Tool)
		}
	case <-time.After(d192Timeout):
		t.Fatal("tool.rejected never landed on the bus")
	}
}

// TestE2E_ApprovalGatedCallTool_CancelWhileGated_AbortsCleanly —
// cancelling the run ctx while the gated dispatch is parked aborts
// RunGuarded (it honours ctx), the per-step goroutine is joined, the
// run surfaces context.Canceled, and the goroutine baseline restores
// (no orphaned goroutine).
func TestE2E_ApprovalGatedCallTool_CancelWhileGated_AbortsCleanly(t *testing.T) {
	baseline := runtime.NumGoroutine()
	env := buildD192Env(t)
	sub, cancelSub := d192SubscribeApprovalRequested(t, env.bus)
	defer cancelSub()

	q := identity.Quadruple{Identity: d192ID, RunID: "run-d192-cancel"}
	spec := d192RunSpec(env, t, q)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resCh := make(chan error, 1)
	go func() {
		_, rerr := env.runLoop.Run(ctx, spec)
		resCh <- rerr
	}()

	// The gate parked the dispatch (the pause is minted); now cancel
	// the run.
	_ = d192WaitForToken(t, sub)
	cancel()

	select {
	case rerr := <-resCh:
		if !errors.Is(rerr, context.Canceled) {
			t.Fatalf("Run err = %v, want context.Canceled", rerr)
		}
	case <-time.After(d192Timeout):
		t.Fatal("run did not return after ctx cancel while gated — orphaned dispatch")
	}
	if got := env.toolRuns.Load(); got != 0 {
		t.Errorf("tool body executions = %d, want 0 (cancelled before approval)", got)
	}
	d192AssertBaseline(t, baseline)
}
