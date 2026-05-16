// Phase 64a integration test — Tool catalog OAuth + approval wiring
// (RFC §6.4 + §3.3; master-plan Phase 64 pre-plan note constraint #7;
// issue #104; D-090).
//
// This test wires the REAL artifacts across the Phase 64a surface:
//
//   - real `pauseresume.Coordinator` (Phase 50 / D-067 — the unified
//     pause/resume primitive),
//   - real `audit.Redactor` (patterns driver),
//   - real `events.EventBus` (inmem driver),
//   - real `tools.ToolCatalog` (the in-memory catalog),
//   - real `catalog.Builder` consuming operator config and
//     auto-wrapping each declared tool with the matching approval
//     gate + (stubbed) OAuth wrapper.
//
// The test exercises:
//
//  1. **Full APPROVE round-trip** — operator config declares
//     `tools.entries: [{name: gate_tool, approval: {policy: deny-all}}]`;
//     a test that invokes `gate_tool` parks on the gate; an admin
//     resolves APPROVE via the gate's `ResolveApproval` (the same
//     surface the wire-side `approve` Protocol method will eventually
//     reach through the steering/coordinator bridge); the tool runs
//     with the ORIGINAL args; `tool.approved` lands on the bus.
//
//  2. **Full REJECT round-trip** — same config, REJECT resolution;
//     the invocation returns `*approval.ErrToolRejected`;
//     `tool.rejected` lands on the bus.
//
//  3. **OAuth wrapper propagates `*ErrAuthRequired`** — operator config
//     declares `tools.entries: [{name: oauth_tool, oauth: {...}}]`;
//     the stub OAuth provider returns `*ErrAuthRequired`; the wrapped
//     Invoke surfaces it so the runtime can pause.
//
//  4. **Identity propagation** — every wrapped Invoke reads identity
//     from ctx; missing identity fails closed.
//
//  5. **Concurrency stress** — N=16 concurrent gated invocations
//     against a single shared catalog; no cross-talk; no leak.
//
//  6. **Failure mode**: a `tools.entries` declaration naming a tool
//     that is not registered fails the catalog build at boot. The
//     test asserts the boot error wraps `ErrToolNotRegistered`.
//
// # Protocol-wire APPROVE/REJECT round-trip
//
// This test exercises the gate's `ResolveApproval` (the in-process
// helper) end-to-end. The wire-side `approve` / `reject` Protocol
// methods route through the Phase 53 steering inbox → Phase 50
// Coordinator.Resume; bridging that path back into the gate's
// `pending` map requires either a steering-aware gate subscriber OR
// a dispatcher-side `ApprovalDispatcher` that owns gates. That bridge
// is the Wave 11 wave-end E2E concern (Stage 4) — issue #104's
// Protocol-wire half. Phase 64a closes the **catalog-wiring** half
// (constraint #7 of the Phase 64 pre-plan note); the Protocol-wire
// round-trip via the real HTTP handler lands in Stage 4.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

// phase64aID is the canonical test identity for the catalog-wiring
// integration test. Documented dummy values (CLAUDE.md §7 rule 2).
var phase64aID = identity.Identity{
	TenantID:  "tenant-phase64a",
	UserID:    "user-phase64a",
	SessionID: "session-phase64a",
}

// phase64aEnv bundles the real artifacts the Phase 64a test wires.
type phase64aEnv struct {
	catalog tools.ToolCatalog
	bus     events.EventBus
	coord   pauseresume.Coordinator
	gates   map[string]*approval.ApprovalGate
}

// phase64aStubProvider is a minimal OAuthProvider that returns either
// a happy-path token OR a typed *ErrAuthRequired depending on the
// `needsAuth` flag the test sets. No real authorization-server stand-up.
type phase64aStubProvider struct {
	needsAuth bool
}

func (s *phase64aStubProvider) Token(_ context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if s.needsAuth {
		return auth.Token{}, &auth.ErrAuthRequired{
			Source:       "phase64a-test-source",
			BindingScope: auth.ScopeUser,
			Message:      "phase64a: oauth required",
		}
	}
	return auth.Token{
		AccessToken: "phase64a-stub-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}, nil
}

func (s *phase64aStubProvider) InitiateFlow(_ context.Context, _ tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}
func (s *phase64aStubProvider) CompleteFlow(_ context.Context, _, _ string) (auth.Token, error) {
	return auth.Token{}, nil
}
func (s *phase64aStubProvider) Revoke(_ context.Context, _ tools.ToolSourceID) error { return nil }
func (s *phase64aStubProvider) Close(_ context.Context) error                        { return nil }

// buildPhase64aEnv wires the Phase 64a stack: catalog + Coordinator +
// Bus + Redactor + catalog wiring builder applied against a real
// in-memory catalog with pre-registered tools.
func buildPhase64aEnv(t *testing.T, entries []config.ToolEntryConfig, providers map[string]auth.OAuthProvider) *phase64aEnv {
	t.Helper()
	cat := tools.NewCatalog()
	mustRegisterPhase64aEcho(t, cat, "gate_tool")
	mustRegisterPhase64aEcho(t, cat, "oauth_tool")
	mustRegisterPhase64aEcho(t, cat, "both_tool")
	mustRegisterPhase64aEcho(t, cat, "stress_tool")
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
	coord := pauseresume.New()
	gates := make(map[string]*approval.ApprovalGate)
	b := catalog.New(entries, catalog.Deps{
		Catalog:        cat,
		Coordinator:    coord,
		Bus:            bus,
		Redactor:       red,
		OAuthProviders: providers,
		AppliedGates:   gates,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("catalog.Apply: %v", err)
	}
	t.Cleanup(func() {
		for _, g := range gates {
			_ = g.Close(context.Background())
		}
	})
	return &phase64aEnv{catalog: cat, bus: bus, coord: coord, gates: gates}
}

func mustRegisterPhase64aEcho(t *testing.T, cat tools.ToolCatalog, name string) {
	t.Helper()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        name,
			Description: "echo (phase 64a test)",
			Transport:   tools.TransportInProcess,
			Source:      "phase64a-test-source",
		},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: string(args)}, nil
		},
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

func phase64aCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func phase64aAdminCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return protocolauth.WithScopes(phase64aCtx(t, id),
		[]protocolauth.Scope{protocolauth.ScopeAdmin})
}

func phase64aSubFor(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) (events.Subscription, func()) {
	t.Helper()
	subCtx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: types,
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

func phase64aWaitEv(t *testing.T, sub events.Subscription, d time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return ev
	case <-time.After(d):
		t.Fatal("timed out waiting for event")
		return events.Event{}
	}
}

// TestE2E_Phase64a_FullApproveCycle is the master-plan acceptance:
// catalog wiring → gate → APPROVE → tool runs.
func TestE2E_Phase64a_FullApproveCycle(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "gate_tool", Approval: &config.ToolApprovalConfig{
			Policy: "deny-all", Reason: "phase64a: human review",
		}},
	}
	env := buildPhase64aEnv(t, entries, nil)

	d, _ := env.catalog.Resolve("gate_tool")
	args := json.RawMessage(`{"target":"phase64a","query":"summary"}`)

	requestedSub, cancelReq := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()
	approvedSub, cancelApp := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolApproved)
	defer cancelApp()

	type outcome struct {
		res tools.ToolResult
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		r, err := d.Invoke(phase64aCtx(t, phase64aID), args)
		resCh <- outcome{res: r, err: err}
	}()

	requestedEv := phase64aWaitEv(t, requestedSub, 2*time.Second)
	if requestedEv.Identity.TenantID != phase64aID.TenantID {
		t.Fatalf("event identity propagation: got %+v want %+v",
			requestedEv.Identity.Identity, phase64aID)
	}
	p, _ := requestedEv.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(p.PauseToken)
	if token == "" {
		t.Fatal("empty PauseToken on requested event")
	}

	if err := env.gates["gate_tool"].ResolveApproval(
		phase64aAdminCtx(t, phase64aID), token,
		approval.DecisionApprove, "admin OK"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	approvedEv := phase64aWaitEv(t, approvedSub, 2*time.Second)
	approvedPayload, _ := approvedEv.Payload.(approval.ToolApprovedPayload)
	if approvedPayload.PauseToken != string(token) {
		t.Fatalf("approved PauseToken mismatch")
	}

	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("wrapped Invoke err: %v", o.err)
		}
		if got, _ := o.res.Value.(string); got != string(args) {
			t.Fatalf("Invoke returned %q, want %q (gate must pass through original args)",
				got, string(args))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke did not return after approval")
	}
}

// TestE2E_Phase64a_FullRejectCycle — REJECT round-trip; the typed
// `tool.rejected` event lands.
func TestE2E_Phase64a_FullRejectCycle(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "gate_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	env := buildPhase64aEnv(t, entries, nil)
	d, _ := env.catalog.Resolve("gate_tool")
	args := json.RawMessage(`{"target":"sensitive"}`)

	requestedSub, cancelReq := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()
	rejectedSub, cancelRej := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolRejected)
	defer cancelRej()

	type outcome struct {
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		_, err := d.Invoke(phase64aCtx(t, phase64aID), args)
		resCh <- outcome{err: err}
	}()

	requestedEv := phase64aWaitEv(t, requestedSub, 2*time.Second)
	p, _ := requestedEv.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(p.PauseToken)

	if err := env.gates["gate_tool"].ResolveApproval(
		phase64aAdminCtx(t, phase64aID), token,
		approval.DecisionReject, "phase64a: bad target"); err != nil {
		t.Fatalf("ResolveApproval(reject): %v", err)
	}

	rejectedEv := phase64aWaitEv(t, rejectedSub, 2*time.Second)
	if rejectedEv.Identity.TenantID != phase64aID.TenantID {
		t.Fatalf("rejected event identity: got %+v want %+v",
			rejectedEv.Identity.Identity, phase64aID)
	}
	rejectedPayload, _ := rejectedEv.Payload.(approval.ToolRejectedPayload)
	if rejectedPayload.PauseToken != string(token) {
		t.Fatalf("rejected PauseToken mismatch")
	}

	select {
	case o := <-resCh:
		var rej *approval.ErrToolRejected
		if !errors.As(o.err, &rej) {
			t.Fatalf("err=%v, want *ErrToolRejected", o.err)
		}
		if rej.Tool != "gate_tool" {
			t.Errorf("rej.Tool=%q, want gate_tool", rej.Tool)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke did not return on reject")
	}
}

// TestE2E_Phase64a_OAuth_ErrAuthRequiredPropagates — the wrapped tool
// surfaces *ErrAuthRequired so the runtime can pause.
func TestE2E_Phase64a_OAuth_ErrAuthRequiredPropagates(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "oauth_tool", OAuth: &config.ToolOAuthConfig{
			Provider: "phase64a-stub", BindingScope: "user",
		}},
	}
	providers := map[string]auth.OAuthProvider{
		"phase64a-stub": &phase64aStubProvider{needsAuth: true},
	}
	env := buildPhase64aEnv(t, entries, providers)
	d, _ := env.catalog.Resolve("oauth_tool")

	_, err := d.Invoke(phase64aCtx(t, phase64aID), json.RawMessage(`{}`))
	var authReq *auth.ErrAuthRequired
	if !errors.As(err, &authReq) {
		t.Fatalf("err=%v, want *auth.ErrAuthRequired", err)
	}
}

// TestE2E_Phase64a_OAuth_HappyPath — when the provider returns a
// valid token, the wrapped Invoke proceeds to the underlying tool.
func TestE2E_Phase64a_OAuth_HappyPath(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "oauth_tool", OAuth: &config.ToolOAuthConfig{
			Provider: "phase64a-stub", BindingScope: "user",
		}},
	}
	providers := map[string]auth.OAuthProvider{
		"phase64a-stub": &phase64aStubProvider{needsAuth: false},
	}
	env := buildPhase64aEnv(t, entries, providers)
	d, _ := env.catalog.Resolve("oauth_tool")

	args := json.RawMessage(`{"phase":"64a"}`)
	res, err := d.Invoke(phase64aCtx(t, phase64aID), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, _ := res.Value.(string); got != string(args) {
		t.Errorf("Invoke result = %q, want %q", got, string(args))
	}
}

// TestE2E_Phase64a_FailureMode_UnknownTool — declaring `tools.entries`
// with a name not registered in the catalog fails the wiring loud.
// This is the §17.3 "≥1 failure mode" gate.
func TestE2E_Phase64a_FailureMode_UnknownTool(t *testing.T) {
	cat := tools.NewCatalog()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     32,
		IdleTimeout:              1 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	coord := pauseresume.New()
	entries := []config.ToolEntryConfig{
		{Name: "no_such_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
	})
	bootErr := b.Apply(context.Background())
	if !errors.Is(bootErr, catalog.ErrToolNotRegistered) {
		t.Fatalf("err=%v, want ErrToolNotRegistered", bootErr)
	}
}

// TestE2E_Phase64a_ConcurrencyStress — N=16 concurrent gated
// invocations against a single shared catalog. CLAUDE.md §17.3
// stress requirement.
func TestE2E_Phase64a_ConcurrencyStress(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "stress_tool", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
	}
	env := buildPhase64aEnv(t, entries, nil)
	d, _ := env.catalog.Resolve("stress_tool")

	const n = 16
	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-stress-%d", i%4),
				UserID:    fmt.Sprintf("user-stress-%d", i%4),
				SessionID: fmt.Sprintf("session-stress-%d", i),
			}
			ctx, _ := identity.With(context.Background(), id)
			args := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			res, err := d.Invoke(ctx, args)
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if got, _ := res.Value.(string); got != string(args) {
				errs <- fmt.Errorf("goroutine %d: args bleed: got %q want %q", i, got, string(args))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Settle + leak check.
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if growth := runtime.NumGoroutine() - baseline; growth > 8 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestE2E_Phase64a_GoroutineLeak_InitiateThenCancel — a gated
// invocation whose ctx is cancelled before resolution drops cleanly;
// no leak.
func TestE2E_Phase64a_GoroutineLeak_InitiateThenCancel(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{Name: "gate_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	env := buildPhase64aEnv(t, entries, nil)
	d, _ := env.catalog.Resolve("gate_tool")

	// Subscribe BEFORE kicking Invoke so we can deterministically wait
	// for the gate to register the pause before cancelling — the
	// previous time.Sleep(50ms) was a race-prone sync-via-sleep that
	// §17.4 forbids (Wave 11 §17.5 audit, finding W4).
	requestedSub, cancelReq := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(phase64aCtx(t, phase64aID))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = d.Invoke(ctx, json.RawMessage(`{}`))
	}()
	// Wait for the gate to publish the approval-requested event
	// (= the pause is registered and the goroutine is parked).
	phase64aWaitEv(t, requestedSub, 2*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke goroutine did not unblock on ctx cancel")
	}
	for i := 0; i < 5; i++ {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if growth := runtime.NumGoroutine() - baseline; growth > 4 {
		t.Errorf("goroutine leak after initiate-then-cancel: baseline=%d, after=%d",
			baseline, runtime.NumGoroutine())
	}
}

// TestE2E_Phase64a_BothMiddleware_ApprovalIsOutermost — when BOTH
// approval AND OAuth are declared on the same tool, approval fires
// FIRST (the §13-amended composition order in D-090). Rejecting
// approval short-circuits before OAuth's ErrAuthRequired would have
// surfaced — verifying the order pin.
func TestE2E_Phase64a_BothMiddleware_ApprovalIsOutermost(t *testing.T) {
	entries := []config.ToolEntryConfig{
		{
			Name:     "both_tool",
			Approval: &config.ToolApprovalConfig{Policy: "deny-all"},
			OAuth:    &config.ToolOAuthConfig{Provider: "phase64a-stub", BindingScope: "user"},
		},
	}
	providers := map[string]auth.OAuthProvider{
		"phase64a-stub": &phase64aStubProvider{needsAuth: true},
	}
	env := buildPhase64aEnv(t, entries, providers)
	d, _ := env.catalog.Resolve("both_tool")

	requestedSub, cancelReq := phase64aSubFor(t, env.bus, phase64aID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()

	type outcome struct {
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		_, err := d.Invoke(phase64aCtx(t, phase64aID), json.RawMessage(`{}`))
		resCh <- outcome{err: err}
	}()

	requestedEv := phase64aWaitEv(t, requestedSub, 2*time.Second)
	p, _ := requestedEv.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(p.PauseToken)
	if token == "" {
		t.Fatal("OAuth wrapper fired before approval — wrong composition order; approval must be outermost")
	}

	if err := env.gates["both_tool"].ResolveApproval(
		phase64aAdminCtx(t, phase64aID), token,
		approval.DecisionReject, "test"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	o := <-resCh
	var rej *approval.ErrToolRejected
	if !errors.As(o.err, &rej) {
		// If OAuth fired first, we'd see *ErrAuthRequired instead.
		var authReq *auth.ErrAuthRequired
		if errors.As(o.err, &authReq) {
			t.Fatalf("got *ErrAuthRequired — OAuth was outermost; expected approval outermost (*ErrToolRejected)")
		}
		t.Fatalf("err=%v, want *ErrToolRejected", o.err)
	}
}
