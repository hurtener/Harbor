package approval

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
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
)

// testID is the canonical unit-test identity. Documented dummy values
// (CLAUDE.md §7 rule 2).
var testID = identity.Identity{
	TenantID:  "tenant-approval-test",
	UserID:    "user-approval-test",
	SessionID: "session-approval-test",
}

func mkTestBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     32,
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

func mkAdminCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	base, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return protocolauth.WithScopes(base, []protocolauth.Scope{protocolauth.ScopeAdmin})
}

func mkConsoleFleetCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	base, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return protocolauth.WithScopes(base, []protocolauth.Scope{protocolauth.ScopeConsoleFleet})
}

func mkPlainCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func mkGate(t *testing.T, policy ApprovalPolicy) (*ApprovalGate, events.EventBus) {
	t.Helper()
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	coord := pauseresume.New()
	g, err := NewApprovalGate(GateDeps{
		Policy:      policy,
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return g, bus
}

func subscribeTo(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) (events.Subscription, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   types,
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

func waitEvent(t *testing.T, sub events.Subscription) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return events.Event{}
	}
}

// --- constructor invariants ----------------------------------------------

func TestNewApprovalGate_RejectsNilPolicy(t *testing.T) {
	red := patternsAudit.New()
	coord := pauseresume.New()
	bus := mkTestBus(t, red)
	_, err := NewApprovalGate(GateDeps{Coordinator: coord, Bus: bus, Redactor: red})
	if !errors.Is(err, ErrPolicyRequired) {
		t.Fatalf("nil Policy: got %v want ErrPolicyRequired", err)
	}
}

func TestNewApprovalGate_RejectsNilCoordinator(t *testing.T) {
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	_, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Bus: bus, Redactor: red})
	if !errors.Is(err, ErrCoordinatorRequired) {
		t.Fatalf("nil Coordinator: got %v want ErrCoordinatorRequired", err)
	}
}

func TestNewApprovalGate_RejectsNilBus(t *testing.T) {
	red := patternsAudit.New()
	coord := pauseresume.New()
	_, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Coordinator: coord, Redactor: red})
	if !errors.Is(err, ErrBusRequired) {
		t.Fatalf("nil Bus: got %v want ErrBusRequired", err)
	}
}

func TestNewApprovalGate_RejectsNilRedactor(t *testing.T) {
	red := patternsAudit.New()
	coord := pauseresume.New()
	bus := mkTestBus(t, red)
	_, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Coordinator: coord, Bus: bus})
	if !errors.Is(err, ErrRedactorRequired) {
		t.Fatalf("nil Redactor: got %v want ErrRedactorRequired", err)
	}
}

func TestNewApprovalGate_HappyPath(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	if g == nil {
		t.Fatal("NewApprovalGate returned nil")
	}
}

// --- RunGuarded short-circuit (policy says no approval needed) ----------

func TestRunGuarded_ShortCircuitsWhenPolicyApproves(t *testing.T) {
	g, bus := mkGate(t, AlwaysApprovePolicy{})

	// Subscribe to all three event types — none should fire when the
	// policy short-circuits.
	sub, cancelSub := subscribeTo(t, bus, testID,
		EventTypeToolApprovalRequested, EventTypeToolApproved, EventTypeToolRejected)
	defer cancelSub()

	ctx := mkPlainCtx(t, testID)
	args := json.RawMessage(`{"k":"v"}`)
	out, err := g.RunGuarded(ctx, &ApprovalRequest{
		Tool:     tools.Tool{Name: "summarize"},
		Args:     args,
		Identity: testID,
	})
	if err != nil {
		t.Fatalf("RunGuarded: %v", err)
	}
	if string(out) != string(args) {
		t.Fatalf("Args: got %s want %s", out, args)
	}
	// No bus emit should land within 100ms (bus has SubscriberBufferSize=32).
	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected event on short-circuit path: %s", ev.Type)
	case <-time.After(100 * time.Millisecond):
		// expected — no emit on the short-circuit path
	}
	if g.pendingLen() != 0 {
		t.Fatalf("pendingLen: %d want 0", g.pendingLen())
	}
}

// --- RunGuarded validation -------------------------------------------------

func TestRunGuarded_RejectsNilRequest(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	_, err := g.RunGuarded(mkPlainCtx(t, testID), nil)
	if err == nil {
		t.Fatal("RunGuarded(nil): want err, got nil")
	}
}

func TestRunGuarded_RejectsEmptyToolName(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	_, err := g.RunGuarded(mkPlainCtx(t, testID), &ApprovalRequest{
		Identity: testID,
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("want non-nil err, got %v", err)
	}
}

func TestRunGuarded_FailsClosedOnMissingIdentity(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	_, err := g.RunGuarded(mkPlainCtx(t, testID), &ApprovalRequest{
		Tool: tools.Tool{Name: "x"},
	})
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("missing identity: got %v want ErrIdentityRequired", err)
	}
}

func TestRunGuarded_FailsClosedOnCancelledCtx(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	ctx, cancel := context.WithCancel(mkPlainCtx(t, testID))
	cancel()
	_, err := g.RunGuarded(ctx, &ApprovalRequest{
		Tool:     tools.Tool{Name: "x"},
		Identity: testID,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: got %v want context.Canceled", err)
	}
}

// --- RunGuarded policy-failure fail-loud ----------------------------------

type failingPolicy struct{ err error }

func (p failingPolicy) ShouldApprove(_ context.Context, _ *ApprovalRequest) (bool, string, error) {
	return false, "", p.err
}

func TestRunGuarded_PolicyError_FailsLoud(t *testing.T) {
	boom := errors.New("policy: config corrupt")
	g, _ := mkGate(t, failingPolicy{err: boom})
	_, err := g.RunGuarded(mkPlainCtx(t, testID), &ApprovalRequest{
		Tool:     tools.Tool{Name: "x"},
		Identity: testID,
	})
	if !errors.Is(err, ErrPolicyFailed) {
		t.Fatalf("policy error: got %v want ErrPolicyFailed", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("policy error: want wrapped boom, got %v", err)
	}
}

// --- RunGuarded redactor-failure fail-loud --------------------------------

// failingRedactor returns an error from Redact — the gate must NOT
// silently emit raw args. CLAUDE.md §7 rule 6 + §13.
type failingRedactor struct{ err error }

func (r failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, r.err
}

func TestRunGuarded_RedactorError_FailsLoud(t *testing.T) {
	boom := errors.New("redactor: rule failed")
	coord := pauseresume.New()
	bus := mkTestBus(t, patternsAudit.New())
	g, err := NewApprovalGate(GateDeps{
		Policy:      AlwaysDenyPolicy{},
		Coordinator: coord,
		Bus:         bus,
		Redactor:    failingRedactor{err: boom},
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	_, err = g.RunGuarded(mkPlainCtx(t, testID), &ApprovalRequest{
		Tool:     tools.Tool{Name: "x"},
		Args:     json.RawMessage(`{"k":"v"}`),
		Identity: testID,
	})
	if err == nil {
		t.Fatal("redactor error: want non-nil err")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("redactor error: want wrapped boom, got %v", err)
	}
}

// --- RunGuarded + ResolveApproval round-trip: APPROVE ---------------------

func TestRunGuarded_ApproveRoundTrip(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})

	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()
	approvedSub, cancelApp := subscribeTo(t, bus, testID, EventTypeToolApproved)
	defer cancelApp()

	ctx := mkPlainCtx(t, testID)
	args := json.RawMessage(`{"q":"hello"}`)
	req := &ApprovalRequest{
		Tool:     tools.Tool{Name: "summarize"},
		Args:     args,
		Identity: testID,
		Tags:     []string{"sensitive"},
	}

	type outcome struct {
		err error
		out json.RawMessage
	}
	resCh := make(chan outcome, 1)
	go func() {
		out, err := g.RunGuarded(ctx, req)
		resCh <- outcome{out: out, err: err}
	}()

	// Wait for the tool.approval_requested event so we know the gate
	// has registered the pending entry.
	ev := waitEvent(t, requestedSub)
	payload, ok := ev.Payload.(ToolApprovalRequestedPayload)
	if !ok {
		t.Fatalf("payload type: got %T", ev.Payload)
	}
	if payload.Tool != "summarize" {
		t.Fatalf("payload.Tool: got %q", payload.Tool)
	}
	if payload.PauseToken == "" {
		t.Fatal("payload.PauseToken empty")
	}
	token := pauseresume.Token(payload.PauseToken)

	// Resolve as admin.
	if err := g.ResolveApproval(mkAdminCtx(t, testID), token, DecisionApprove, "looks ok"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	// Wait for tool.approved event.
	approvedEv := waitEvent(t, approvedSub)
	approvedPayload, ok := approvedEv.Payload.(ToolApprovedPayload)
	if !ok {
		t.Fatalf("approved payload type: got %T", approvedEv.Payload)
	}
	if approvedPayload.Tool != "summarize" || approvedPayload.PauseToken != string(token) {
		t.Fatalf("approved payload mismatch: %+v", approvedPayload)
	}
	if approvedPayload.ApproverReason != "looks ok" {
		t.Fatalf("approver reason: got %q", approvedPayload.ApproverReason)
	}

	// Collect the RunGuarded outcome.
	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("RunGuarded err: %v", o.err)
		}
		if string(o.out) != string(args) {
			t.Fatalf("RunGuarded args: got %s want %s", o.out, args)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return")
	}

	if g.pendingLen() != 0 {
		t.Fatalf("pendingLen: %d want 0", g.pendingLen())
	}
}

// --- RunGuarded + ResolveApproval round-trip: REJECT ----------------------

func TestRunGuarded_RejectRoundTrip(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})

	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()
	rejectedSub, cancelRej := subscribeTo(t, bus, testID, EventTypeToolRejected)
	defer cancelRej()

	ctx := mkPlainCtx(t, testID)
	args := json.RawMessage(`{"q":"hello"}`)
	req := &ApprovalRequest{
		Tool:     tools.Tool{Name: "delete_prod"},
		Args:     args,
		Identity: testID,
		Tags:     []string{"sensitive", "write:prod"},
	}

	type outcome struct {
		err error
		out json.RawMessage
	}
	resCh := make(chan outcome, 1)
	go func() {
		out, err := g.RunGuarded(ctx, req)
		resCh <- outcome{out: out, err: err}
	}()

	ev := waitEvent(t, requestedSub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	if err := g.ResolveApproval(mkConsoleFleetCtx(t, testID), token, DecisionReject, "not approved"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	rejectedEv := waitEvent(t, rejectedSub)
	rejectedPayload, ok := rejectedEv.Payload.(ToolRejectedPayload)
	if !ok {
		t.Fatalf("rejected payload type: got %T", rejectedEv.Payload)
	}
	if rejectedPayload.Reason != "not approved" {
		t.Fatalf("rejected reason: got %q", rejectedPayload.Reason)
	}

	select {
	case o := <-resCh:
		var rejErr *ErrToolRejected
		if !errors.As(o.err, &rejErr) {
			t.Fatalf("RunGuarded err: want *ErrToolRejected, got %v", o.err)
		}
		if rejErr.Tool != "delete_prod" {
			t.Fatalf("rejErr.Tool: %q", rejErr.Tool)
		}
		if rejErr.Reason != "not approved" {
			t.Fatalf("rejErr.Reason: %q", rejErr.Reason)
		}
		if rejErr.Identity != testID {
			t.Fatalf("rejErr.Identity: got %+v want %+v", rejErr.Identity, testID)
		}
		if !errors.Is(o.err, ErrToolRejectedSentinel) {
			t.Fatal("errors.Is(err, sentinel): want true")
		}
		if o.out != nil {
			t.Fatalf("RunGuarded out on reject: want nil, got %s", o.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return")
	}
}

// --- RunGuarded ctx-cancel before resolution ------------------------------

func TestRunGuarded_CtxCancel_BeforeResolution(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	ctx, cancel := context.WithCancel(mkPlainCtx(t, testID))
	type outcome struct{ err error }
	resCh := make(chan outcome, 1)
	go func() {
		_, err := g.RunGuarded(ctx, &ApprovalRequest{
			Tool:     tools.Tool{Name: "x"},
			Args:     json.RawMessage(`{}`),
			Identity: testID,
		})
		resCh <- outcome{err: err}
	}()
	// Wait for the pause to register, then cancel.
	_ = waitEvent(t, requestedSub)
	cancel()
	select {
	case o := <-resCh:
		if !errors.Is(o.err, ErrApprovalCancelled) {
			t.Fatalf("err: want ErrApprovalCancelled, got %v", o.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return on ctx cancel")
	}
	if g.pendingLen() != 0 {
		t.Fatalf("pendingLen: %d want 0", g.pendingLen())
	}
}

// --- ResolveApproval scope gating -----------------------------------------

func TestResolveApproval_RejectsUnscopedCaller(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	ctx := mkPlainCtx(t, testID)
	go func() {
		_, _ = g.RunGuarded(ctx, &ApprovalRequest{
			Tool:     tools.Tool{Name: "x"},
			Args:     json.RawMessage(`{}`),
			Identity: testID,
		})
	}()
	ev := waitEvent(t, requestedSub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	// Unscoped ctx — no admin / console:fleet claim. The protocol/auth
	// middleware is the only thing that sets scopes; a ctx that has
	// not been through it carries no scopes.
	err := g.ResolveApproval(mkPlainCtx(t, testID), token, DecisionApprove, "")
	if !errors.Is(err, ErrApprovalScopeRequired) {
		t.Fatalf("unscoped Resolve: got %v want ErrApprovalScopeRequired", err)
	}

	// Clean up the still-pending entry by approving as admin.
	if err := g.ResolveApproval(mkAdminCtx(t, testID), token, DecisionApprove, ""); err != nil {
		t.Fatalf("admin Resolve: %v", err)
	}
}

// --- ResolveApproval invalid decision -------------------------------------

func TestResolveApproval_RejectsPendingDecision(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	err := g.ResolveApproval(mkAdminCtx(t, testID), pauseresume.Token("x"), DecisionPending, "")
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("Pending decision: got %v want ErrInvalidDecision", err)
	}
}

// --- ResolveApproval cross-identity rejection -----------------------------

func TestResolveApproval_CrossIdentity_Rejected(t *testing.T) {
	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}

	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	coord := pauseresume.New()
	g, err := NewApprovalGate(GateDeps{
		Policy: AlwaysDenyPolicy{}, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	// Subscribe both — use Admin filter to catch both identities.
	adminCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	sub, err := bus.Subscribe(adminCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe(admin): %v", err)
	}
	defer sub.Cancel()

	ctxA := mkPlainCtx(t, idA)
	go func() {
		_, _ = g.RunGuarded(ctxA, &ApprovalRequest{
			Tool:     tools.Tool{Name: "x"},
			Args:     json.RawMessage(`{}`),
			Identity: idA,
		})
	}()
	ev := waitEvent(t, sub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	// Tenant B admin tries to resolve — Coordinator scope check
	// rejects via ErrScopeMismatch.
	err = g.ResolveApproval(mkAdminCtx(t, idB), token, DecisionApprove, "")
	if !errors.Is(err, pauseresume.ErrScopeMismatch) {
		t.Fatalf("cross-identity Resolve: got %v want ErrScopeMismatch", err)
	}

	// Clean up by resolving from the correct identity.
	if err := g.ResolveApproval(mkAdminCtx(t, idA), token, DecisionApprove, ""); err != nil {
		t.Fatalf("correct-identity Resolve: %v", err)
	}
}

// --- ResolveApproval unknown / already-resolved token --------------------

func TestResolveApproval_UnknownToken(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	err := g.ResolveApproval(mkAdminCtx(t, testID), pauseresume.Token("not-real"), DecisionApprove, "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("unknown token: got %v want ErrApprovalNotFound", err)
	}
}

func TestResolveApproval_DoubleResolve_NotFoundOnSecondCall(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	ctx := mkPlainCtx(t, testID)
	go func() {
		_, _ = g.RunGuarded(ctx, &ApprovalRequest{
			Tool:     tools.Tool{Name: "x"},
			Args:     json.RawMessage(`{}`),
			Identity: testID,
		})
	}()
	ev := waitEvent(t, requestedSub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	if err := g.ResolveApproval(mkAdminCtx(t, testID), token, DecisionApprove, ""); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	// Second resolve — entry has been removed from the pending map.
	err := g.ResolveApproval(mkAdminCtx(t, testID), token, DecisionApprove, "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("second Resolve: got %v want ErrApprovalNotFound", err)
	}
}

// --- Gate Close idempotency + post-close calls ---------------------------

func TestGate_Close_Idempotent(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	if err := g.Close(context.Background()); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := g.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRunGuarded_AfterClose_ReturnsClosed(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	_ = g.Close(context.Background())
	_, err := g.RunGuarded(mkPlainCtx(t, testID), &ApprovalRequest{
		Tool: tools.Tool{Name: "x"}, Identity: testID,
	})
	if !errors.Is(err, ErrGateClosed) {
		t.Fatalf("post-close RunGuarded: got %v want ErrGateClosed", err)
	}
}

func TestResolveApproval_AfterClose_ReturnsClosed(t *testing.T) {
	g, _ := mkGate(t, AlwaysDenyPolicy{})
	_ = g.Close(context.Background())
	err := g.ResolveApproval(mkAdminCtx(t, testID), pauseresume.Token("x"), DecisionApprove, "")
	if !errors.Is(err, ErrGateClosed) {
		t.Fatalf("post-close Resolve: got %v want ErrGateClosed", err)
	}
}

// --- TaggedPolicy end-to-end: tag triggers approval -----------------------

func TestRunGuarded_TaggedPolicy_TriggersOnTag(t *testing.T) {
	g, bus := mkGate(t, TaggedPolicy{RequireTags: []string{"sensitive"}})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	// Tag matches → gate parks.
	ctx := mkPlainCtx(t, testID)
	type out struct{ err error }
	resCh := make(chan out, 1)
	go func() {
		_, err := g.RunGuarded(ctx, &ApprovalRequest{
			Tool: tools.Tool{Name: "x"}, Args: json.RawMessage(`{}`),
			Identity: testID, Tags: []string{"sensitive"},
		})
		resCh <- out{err}
	}()
	ev := waitEvent(t, requestedSub)
	payload, _ := ev.Payload.(ToolApprovalRequestedPayload)
	if payload.Reason != "policy: tagged" {
		t.Fatalf("Reason: got %q want %q", payload.Reason, "policy: tagged")
	}
	if err := g.ResolveApproval(mkAdminCtx(t, testID), pauseresume.Token(payload.PauseToken), DecisionApprove, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	<-resCh
}

// --- goroutine-leak: initiate-then-cancel ---------------------------------

func TestRunGuarded_InitiateThenCancel_NoGoroutineLeak(t *testing.T) {
	g, bus := mkGate(t, AlwaysDenyPolicy{})
	requestedSub, cancelReq := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelReq()

	baseline := runtime.NumGoroutine()
	for range 25 {
		ctx, cancel := context.WithCancel(mkPlainCtx(t, testID))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = g.RunGuarded(ctx, &ApprovalRequest{
				Tool: tools.Tool{Name: "x"}, Args: json.RawMessage(`{}`),
				Identity: testID,
			})
		}()
		_ = waitEvent(t, requestedSub)
		cancel()
		<-done
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtime.NumGoroutine() - baseline
	if leak > 5 {
		t.Fatalf("goroutine leak: leaked=%d (baseline=%d, now=%d)",
			leak, baseline, runtime.NumGoroutine())
	}
}
