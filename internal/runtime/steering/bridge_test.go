package steering

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// Bridge tests (D-097) — the steering→gate routing path in
// applier.advancePause + routeThroughGate.
//
// These tests exercise the bridge end-to-end against the REAL approval
// gate + the REAL pauseresume.Coordinator. The gate's RunGuarded path
// requires real coordination (it parks via Coordinator.Request, waits
// on the per-pause resolve channel); the steering apply path must
// unblock that waiter without double-resuming.

// bridgeTestID is the documented dummy identity the bridge tests use.
var bridgeTestID = identity.Identity{
	TenantID:  "tenant-bridge",
	UserID:    "user-bridge",
	SessionID: "session-bridge",
}

func mkBridgeBus(t *testing.T, red audit.Redactor) events.EventBus {
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

// bridgeFixture bundles a real gate + the bus + coordinator the test
// drives directly. The bus is exposed so tests can subscribe to
// tool.approval_requested and read the minted pause Token.
type bridgeFixture struct {
	gate  *approval.ApprovalGate
	bus   events.EventBus
	coord pauseresume.Coordinator
}

// mkBridgeFixture constructs a real ApprovalGate + Bus + Coordinator
// triple sharing one set of dependencies. The applier under test MUST
// be constructed with the SAME coord — otherwise the bridge's "skip
// direct Resume" path silently no-ops (the gate's Resume calls one
// Coordinator, the applier's fall-through would call a different one).
func mkBridgeFixture(t *testing.T, policy approval.ApprovalPolicy) *bridgeFixture {
	t.Helper()
	red := patternsAudit.New()
	bus := mkBridgeBus(t, red)
	coord := pauseresume.New(pauseresume.WithBus(bus))
	g, err := approval.NewApprovalGate(approval.GateDeps{
		Policy:      policy,
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
	})
	if err != nil {
		t.Fatalf("approval.NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return &bridgeFixture{gate: g, bus: bus, coord: coord}
}

// alwaysRequirePolicy is a minimal approval policy that always
// requires approval. The gate's pending map will always populate on
// RunGuarded so the bridge has something to resolve.
type alwaysRequirePolicy struct{}

func (alwaysRequirePolicy) ShouldApprove(_ context.Context, _ *approval.ApprovalRequest) (bool, string, error) {
	return true, "bridge-test", nil
}

// waitForApprovalRequested blocks until a tool.approval_requested
// event arrives on the bus or the timeout fires. Returns the minted
// pause Token. The subscription MUST be opened before the RunGuarded
// goroutine starts so it does not race the publish.
func waitForApprovalRequested(t *testing.T, sub events.Subscription, d time.Duration) pauseresume.Token {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed before tool.approval_requested arrived")
		}
		p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
		if !ok {
			t.Fatalf("subscribed event payload type = %T, want approval.ToolApprovalRequestedPayload", ev.Payload)
		}
		return pauseresume.Token(p.PauseToken)
	case <-time.After(d):
		t.Fatal("timeout waiting for tool.approval_requested")
		return ""
	}
}

// subscribeForApprovalRequested opens an identity-filtered bus
// subscription for tool.approval_requested events.
func subscribeForApprovalRequested(t *testing.T, bus events.EventBus, id identity.Identity) (events.Subscription, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return sub, func() {
		sub.Cancel()
		cancel()
	}
}

// TestBridge_NoGates_DirectResume_Backcompat — when no gates are
// configured the apply path is the pre-D-097 direct Coordinator.Resume.
// Asserts the bridge is opt-in and a binary with no approval gates
// behaves exactly as before.
func TestBridge_NoGates_DirectResume_Backcompat(t *testing.T) {
	coord := &stubCoordinator{}
	a := newTestApplier(coord, nil, nil)
	a.gates = nil // explicit

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-no-gates"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": "tok-x", "reason": "ok"},
	}
	// outstandingToken is the RunLoop's own pause token (same value as
	// wire payload here just for simplicity). With no gates configured
	// the bridge is inert and the direct Coordinator.Resume fires.
	if err := a.applyEvent(context.Background(), sc, ev, pauseresume.Token("tok-x")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if coord.resumeCalls != 1 {
		t.Errorf("stubCoordinator Resume calls = %d, want 1 (direct fall-back)", coord.resumeCalls)
	}
	if coord.lastResumeDecision != pauseresume.DecisionApprove {
		t.Errorf("direct Resume Decision = %q, want %q", coord.lastResumeDecision, pauseresume.DecisionApprove)
	}
}

// TestBridge_Approve_RoutesThroughGate — the canonical happy path. A
// RunGuarded call parks via the gate; an APPROVE arrives on the
// steering apply path; the bridge routes it through gate.ResolveApproval;
// the wrapped goroutine unblocks with the original args.
func TestBridge_Approve_RoutesThroughGate(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	// Open the subscription BEFORE starting the RunGuarded goroutine
	// so we never race the publish.
	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	// Build the apply path. The applier's coord MUST be the same one
	// the gate uses.
	a := &applier{
		coord: fx.coord,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	// Park via the real gate.
	originalArgs := json.RawMessage(`{"input":"bridge-approve"}`)
	type outcome struct {
		args json.RawMessage
		err  error
	}
	resCh := make(chan outcome, 1)
	go func() {
		args, err := fx.gate.RunGuarded(bridgeCallerCtx(t), &approval.ApprovalRequest{
			Tool:     tools.Tool{Name: "bridge-tool"},
			Args:     originalArgs,
			Identity: bridgeTestID,
		})
		resCh <- outcome{args: args, err: err}
	}()

	token := waitForApprovalRequested(t, sub, 2*time.Second)

	// Fire the apply path with an APPROVE control carrying the
	// gate's token in the wire payload (the canonical wire shape).
	// outstandingToken is empty so the direct Resume fall-back path
	// fails-loud on no-outstanding-pause — that's intentional: the
	// bridge handled the gate resolution; nothing to direct-resume.
	// Actually for the bridge-only test we pass a synthetic
	// outstandingToken (different from the gate's token) so the
	// direct path fall-through (when wireToken == token edge case
	// fires) has a token to work with. Tests for the wireToken==token
	// edge case live in TestBridge_NoDoubleResume.
	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-approve"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": string(token), "reason": "looks good"},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	// outstandingToken is the gate's token so the wireToken==token
	// fast-path skips the direct Resume — the gate already resumed.
	if err := a.applyEvent(ctx, sc, ev, token); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	// The wrapped goroutine unblocks with the original args.
	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("RunGuarded err: %v", o.err)
		}
		if string(o.args) != string(originalArgs) {
			t.Errorf("RunGuarded args = %s, want %s", o.args, originalArgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return after APPROVE routed through bridge")
	}
}

// TestBridge_Reject_RoutesThroughGate — the rejection path. The
// wrapped goroutine returns *approval.ErrToolRejected.
func TestBridge_Reject_RoutesThroughGate(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	a := &applier{
		coord: fx.coord,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	type outcome struct {
		args json.RawMessage
		err  error
	}
	resCh := make(chan outcome, 1)
	go func() {
		args, err := fx.gate.RunGuarded(bridgeCallerCtx(t), &approval.ApprovalRequest{
			Tool:     tools.Tool{Name: "bridge-tool"},
			Args:     json.RawMessage(`{"input":"bridge-reject"}`),
			Identity: bridgeTestID,
		})
		resCh <- outcome{args: args, err: err}
	}()

	token := waitForApprovalRequested(t, sub, 2*time.Second)

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-reject"}
	ev := ControlEvent{
		Type:     ControlReject,
		Identity: q,
		Payload:  map[string]any{"token": string(token), "reason": "looks bad"},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	if err := a.applyEvent(ctx, sc, ev, token); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	select {
	case o := <-resCh:
		var rej *approval.ErrToolRejected
		if !errors.As(o.err, &rej) {
			t.Fatalf("RunGuarded err = %v, want *ErrToolRejected", o.err)
		}
		if rej.Reason != "looks bad" {
			t.Errorf("rej.Reason = %q, want %q", rej.Reason, "looks bad")
		}
		if o.args != nil {
			t.Errorf("RunGuarded args on REJECT = %s, want nil", o.args)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return after REJECT routed through bridge")
	}
}

// TestBridge_NoDoubleResume — the load-bearing assertion. If the
// bridge fell through after routing through the gate AND the apply
// path ALSO called Coordinator.Resume, the second call would
// pauseresume.ErrAlreadyResumed and the apply path would surface that
// as an error. This test pins option A (gate-owned resume, skip the
// direct call) by asserting NO error fires on the apply path AND the
// Coordinator's Status reflects exactly one resume.
func TestBridge_NoDoubleResume(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	a := &applier{
		coord: fx.coord,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	resCh := make(chan error, 1)
	go func() {
		_, err := fx.gate.RunGuarded(bridgeCallerCtx(t), &approval.ApprovalRequest{
			Tool:     tools.Tool{Name: "bridge-tool"},
			Args:     json.RawMessage(`{}`),
			Identity: bridgeTestID,
		})
		resCh <- err
	}()
	token := waitForApprovalRequested(t, sub, 2*time.Second)

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-no-double"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": string(token), "reason": "go"},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	// outstandingToken is the gate's token — the wireToken==token
	// fast-path fires and the direct Coordinator.Resume is SKIPPED
	// (option A). A double-resume would surface as ErrAlreadyResumed
	// here.
	if err := a.applyEvent(ctx, sc, ev, token); err != nil {
		t.Fatalf("applyEvent: %v (a double-resume manifests as ErrAlreadyResumed here — see D-097)", err)
	}

	// Drain the RunGuarded.
	select {
	case err := <-resCh:
		if err != nil {
			t.Fatalf("RunGuarded err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not unblock")
	}

	// Confirm the Coordinator's record is in StatusResumed — the gate
	// ran Coordinator.Resume exactly once and the apply path did NOT
	// double-resume.
	st, err := fx.coord.Status(context.Background(), token)
	if err != nil {
		t.Fatalf("coord.Status: %v", err)
	}
	if st.State != pauseresume.StatusResumed {
		t.Errorf("post-bridge coord.Status state = %q, want %q (gate ran Coordinator.Resume exactly once)",
			st.State, pauseresume.StatusResumed)
	}
}

// TestBridge_NoWirePayloadToken_DirectResumeOnRunLoopToken — when
// the wire APPROVE payload carries NO `token` key (the canonical
// shape for an APPROVE / REJECT against the RunLoop's own
// `RequestPause` — OAuth flow, A2A AUTH_REQUIRED, etc.), the bridge
// is inert; the direct path resumes the RunLoop's outstandingToken.
func TestBridge_NoWirePayloadToken_DirectResumeOnRunLoopToken(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	stub := &stubCoordinator{}
	a := &applier{
		coord: stub,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-no-wire-token"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"reason": "OAuth-style"}, // no "token" key
	}
	ctx := ctxWithIdentity(context.Background(), q)
	if err := a.applyEvent(ctx, sc, ev, pauseresume.Token("runloop-pause-token")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if stub.resumeCalls != 1 {
		t.Errorf("stubCoordinator Resume calls = %d, want 1 (direct path fired)", stub.resumeCalls)
	}
	if stub.lastResumeDecision != pauseresume.DecisionApprove {
		t.Errorf("direct Resume Decision = %q, want %q", stub.lastResumeDecision, pauseresume.DecisionApprove)
	}
	if len(stub.resumedTokens) != 1 || stub.resumedTokens[0] != "runloop-pause-token" {
		t.Errorf("direct Resume tokens = %v, want [runloop-pause-token]", stub.resumedTokens)
	}
}

// TestBridge_WireTokenNotOwnedByAnyGate_FallsThroughAndResumesRunLoop —
// when the wire payload carries a `token` but no gate owns it (a
// stale token; typo; OAuth pause whose payload happens to include a
// `token` key), routeThroughGate returns (false, nil) and the direct
// path resumes the RunLoop's outstandingToken with the wire payload.
func TestBridge_WireTokenNotOwnedByAnyGate_FallsThroughAndResumesRunLoop(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	stub := &stubCoordinator{}
	a := &applier{
		coord: stub,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-fallthrough"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": "not-a-gate-token", "reason": "OAuth-style"},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	if err := a.applyEvent(ctx, sc, ev, pauseresume.Token("runloop-pause-token")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if stub.resumeCalls != 1 {
		t.Errorf("stubCoordinator Resume calls = %d, want 1 (direct path fired)", stub.resumeCalls)
	}
	if stub.lastResumeDecision != pauseresume.DecisionApprove {
		t.Errorf("direct Resume Decision = %q, want %q", stub.lastResumeDecision, pauseresume.DecisionApprove)
	}
}

// TestBridge_ResumeStaysOnDirectPath — a plain RESUME (operator-level
// "advance the pause" with no approval semantics) must NOT route
// through any gate, even when gates are configured. Pins the
// approval/RESUME boundary the bridge respects.
func TestBridge_ResumeStaysOnDirectPath(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	stub := &stubCoordinator{}
	a := &applier{
		coord: stub,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-resume"}
	ev := ControlEvent{
		Type:     ControlResume,
		Identity: q,
		Payload:  map[string]any{},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	if err := a.applyEvent(ctx, sc, ev, pauseresume.Token("tok-resume")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	// Direct Coordinator.Resume fired with DecisionResume.
	if stub.resumeCalls != 1 {
		t.Fatalf("Resume calls = %d, want 1", stub.resumeCalls)
	}
	if stub.lastResumeDecision != pauseresume.DecisionResume {
		t.Errorf("plain RESUME Decision = %q, want %q", stub.lastResumeDecision, pauseresume.DecisionResume)
	}
}

// TestBridge_NilGateInMap_Skipped — defence-in-depth: a nil pointer
// in the gates map must not panic the bridge. The bridge skips nil
// entries; routeThroughGate returns (false, nil); the direct path
// resumes the RunLoop's outstandingToken.
func TestBridge_NilGateInMap_Skipped(t *testing.T) {
	stub := &stubCoordinator{}
	a := &applier{
		coord: stub,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": nil},
	}

	sc := &stepControl{}
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-nilgate"}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": "tok-x", "reason": "x"},
	}
	ctx := ctxWithIdentity(context.Background(), q)
	if err := a.applyEvent(ctx, sc, ev, pauseresume.Token("runloop-tok")); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}
	if stub.resumeCalls != 1 {
		t.Fatalf("Resume calls = %d, want 1 (fall-through after nil-gate skip)", stub.resumeCalls)
	}
}

// TestRunLoop_WithApprovalGates_OptionWires — the public option
// surface wires the map onto the applier.
func TestRunLoop_WithApprovalGates_OptionWires(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})
	gates := map[string]*approval.ApprovalGate{"bridge-tool": fx.gate}
	rl, _, _ := newTestRunLoop(t, WithApprovalGates(gates))
	if rl.applier == nil {
		t.Fatal("RunLoop.applier nil")
	}
	if len(rl.applier.gates) != 1 || rl.applier.gates["bridge-tool"] == nil {
		t.Errorf("RunLoop.applier.gates = %v, want one entry for bridge-tool", rl.applier.gates)
	}
}

// TestRunLoop_WithApprovalGates_NilMap_Tolerated — a nil map disables
// the bridge without crashing. Tests the nil-safe contract documented
// on WithApprovalGates.
func TestRunLoop_WithApprovalGates_NilMap_Tolerated(t *testing.T) {
	rl, _, _ := newTestRunLoop(t, WithApprovalGates(nil))
	if rl.applier.gates != nil {
		t.Errorf("RunLoop.applier.gates = %v, want nil (nil-safe contract)", rl.applier.gates)
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// bridgeCallerCtx returns a ctx with identity attached. The gate's
// RunGuarded validates identity via the request; ctx-side scopes are
// not needed for RunGuarded (the bridge stamps admin scope inside the
// routeThroughGate helper before calling ResolveApproval).
func bridgeCallerCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), bridgeTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestBridge_IdentityElevation_DoesNotLeakBackToCaller — W3 from the
// Wave 11.5 §17.5 audit. D-097's "Identity flow" clause: the bridge
// stamps admin scope on a DERIVED ctx so gate.ResolveApproval's scope
// check passes; that elevation MUST NOT propagate back to the caller's
// ctx. Verifies:
//
//  1. A caller ctx with identity I + NO scopes drives an APPROVE
//     successfully (the gate's scope check passes — proves the bridge
//     elevated).
//  2. After applyEvent returns, the caller's original ctx still carries
//     ZERO scopes (proves the elevation was scoped to the bridge call).
//  3. The identity tuple flowing into the gate is the same I from the
//     caller's ctx (proves the bridge preserves the (tenant,user,session)
//     triple across the elevation).
func TestBridge_IdentityElevation_DoesNotLeakBackToCaller(t *testing.T) {
	fx := mkBridgeFixture(t, alwaysRequirePolicy{})

	sub, cancelSub := subscribeForApprovalRequested(t, fx.bus, bridgeTestID)
	defer cancelSub()

	a := &applier{
		coord: fx.coord,
		gates: map[string]*approval.ApprovalGate{"bridge-tool": fx.gate},
	}

	// Park via the real gate, using a CALLER ctx with identity + NO
	// scopes. If the bridge does not elevate, the gate's defence-in-
	// depth scope check fails and ResolveApproval returns scope error;
	// the RunGuarded goroutine never unblocks.
	originalArgs := json.RawMessage(`{"input":"bridge-identity"}`)
	type outcome struct {
		args json.RawMessage
		err  error
	}
	resCh := make(chan outcome, 1)
	go func() {
		args, err := fx.gate.RunGuarded(bridgeCallerCtx(t), &approval.ApprovalRequest{
			Tool:     tools.Tool{Name: "bridge-tool"},
			Args:     originalArgs,
			Identity: bridgeTestID,
		})
		resCh <- outcome{args: args, err: err}
	}()

	token := waitForApprovalRequested(t, sub, 2*time.Second)

	// Caller ctx: identity attached, ZERO scopes. This is the shape
	// the steering apply path would receive after Phase 54's
	// Inbox.Enqueue handed off a control event — the scope was vetted
	// at the wire edge; the apply-path ctx itself carries no scopes
	// until the bridge stamps them on its derived ctx.
	q := identity.Quadruple{Identity: bridgeTestID, RunID: "run-bridge-identity"}
	callerCtx := ctxWithIdentity(context.Background(), q)
	if before, ok := protocolauth.ScopesFrom(callerCtx); ok && len(before) != 0 {
		t.Fatalf("test precondition: caller ctx has scopes %v, want none", before)
	}

	sc := &stepControl{}
	ev := ControlEvent{
		Type:     ControlApprove,
		Identity: q,
		Payload:  map[string]any{"token": string(token), "reason": "elevated by bridge"},
	}
	if err := a.applyEvent(callerCtx, sc, ev, token); err != nil {
		t.Fatalf("applyEvent: %v", err)
	}

	// 1. The gate resolved — proves the bridge's elevation worked
	//    (without admin scope on the bridge's derived ctx, the gate's
	//    scope check rejects).
	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("RunGuarded err (bridge did not elevate?): %v", o.err)
		}
		if string(o.args) != string(originalArgs) {
			t.Errorf("RunGuarded args = %s, want %s", o.args, originalArgs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return — bridge elevation likely failed")
	}

	// 2. The caller's ctx is UNCHANGED. context.WithValue returns a
	//    new ctx; the original is immutable by construction. Asserting
	//    explicitly so a future refactor that accidentally swaps in a
	//    mutable storage (e.g. a sync.Map keyed by ctx) trips this.
	if after, ok := protocolauth.ScopesFrom(callerCtx); ok && len(after) != 0 {
		t.Errorf("caller ctx scopes mutated after applyEvent: got %v, want none", after)
	}

	// 3. The identity triple in the caller's ctx is the same I. The
	//    bridge's derived ctx inherited it; nothing should have
	//    swapped it out.
	gotID := identity.MustFrom(callerCtx)
	if gotID != bridgeTestID {
		t.Errorf("identity triple drifted: got %+v, want %+v", gotID, bridgeTestID)
	}
}
