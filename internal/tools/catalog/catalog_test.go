package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// protocolAuthWithScopes attaches the admin scope to ctx so the
// approval gate's ResolveApproval check passes.
func protocolAuthWithScopes(ctx context.Context) context.Context {
	return protocolauth.WithScopes(ctx, []protocolauth.Scope{protocolauth.ScopeAdmin})
}

// catalogTestID is the canonical test identity. Documented dummy
// values per CLAUDE.md §7 rule 2.
var catalogTestID = identity.Identity{
	TenantID:  "tenant-catalog-test",
	UserID:    "user-catalog-test",
	SessionID: "session-catalog-test",
}

// stubOAuthProvider returns a configurable response from Token. Used
// to drive the OAuth wrapper's both happy + ErrAuthRequired paths
// without standing up a real authorization-server httptest.Server.
type stubOAuthProvider struct {
	tokenErr error
}

func (s *stubOAuthProvider) Token(_ context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if s.tokenErr != nil {
		return auth.Token{}, s.tokenErr
	}
	return auth.Token{
		AccessToken: "stub-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
	}, nil
}

func (s *stubOAuthProvider) InitiateFlow(_ context.Context, _ tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}

func (s *stubOAuthProvider) CompleteFlow(_ context.Context, _, _ string) (auth.Token, error) {
	return auth.Token{}, nil
}

func (s *stubOAuthProvider) Revoke(_ context.Context, _ tools.ToolSourceID) error { return nil }
func (s *stubOAuthProvider) Close(_ context.Context) error                        { return nil }

// buildCatalogEnv builds the standard test environment: a real
// in-memory ToolCatalog with two pre-registered tools, a real
// Coordinator + Bus + Redactor for the gate wiring.
func buildCatalogEnv(t *testing.T) (tools.ToolCatalog, pauseresume.Coordinator, events.EventBus, *auditpatterns.Driver) {
	t.Helper()
	cat := tools.NewCatalog()
	mustRegisterEcho(t, cat, "echo_tool")
	mustRegisterEcho(t, cat, "delete_doc")
	mustRegisterEcho(t, cat, "github_read")
	coord := pauseresume.New()
	red := auditpatterns.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return cat, coord, bus, red
}

// mustRegisterEcho registers a tool that echoes its args back unchanged.
func mustRegisterEcho(t *testing.T, cat tools.ToolCatalog, name string) {
	t.Helper()
	d := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        name,
			Description: "echo: returns the args verbatim",
			Transport:   tools.TransportInProcess,
			Source:      tools.ToolSourceID("test-source"),
		},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: string(args)}, nil
		},
	}
	if err := cat.Register(d); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

func ctxWithID(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// ctxWithAdmin returns a ctx carrying the test identity AND the
// admin scope (needed by ApprovalGate.ResolveApproval).
func ctxWithAdmin(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return protocolAuthWithScopes(ctxWithID(t, id))
}

// TestApply_NilCatalog_FailsLoud — fail-loud at boot when Deps.Catalog is nil.
func TestApply_NilCatalog_FailsLoud(t *testing.T) {
	b := catalog.New(nil, catalog.Deps{})
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrCatalogRequired) {
		t.Fatalf("err=%v, want ErrCatalogRequired", err)
	}
}

// TestApply_EmptyEntries_NoOp — empty entries list is a valid call
// that does nothing.
func TestApply_EmptyEntries_NoOp(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	b := catalog.New(nil, catalog.Deps{Catalog: cat})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Originally-registered descriptor is still resolvable.
	if _, ok := cat.Resolve("echo_tool"); !ok {
		t.Fatal("echo_tool descriptor missing after no-op Apply")
	}
}

// TestApply_UnknownTool_FailsLoud — an entry naming a tool that is
// not registered fails the build closed.
func TestApply_UnknownTool_FailsLoud(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "no_such_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog:     cat,
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
	})
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrToolNotRegistered) {
		t.Fatalf("err=%v, want ErrToolNotRegistered", err)
	}
	if !strings.Contains(err.Error(), "no_such_tool") {
		t.Errorf("err=%q missing offending tool name", err)
	}
}

// TestApply_UnknownPolicy_FailsLoud — an unknown approval policy name
// fails the build closed.
func TestApply_UnknownPolicy_FailsLoud(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: "bogus-policy"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog:     cat,
		Coordinator: coord,
		Bus:         bus,
		Redactor:    red,
	})
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrUnknownApprovalPolicy) {
		t.Fatalf("err=%v, want ErrUnknownApprovalPolicy", err)
	}
	if !strings.Contains(err.Error(), "bogus-policy") {
		t.Errorf("err=%q missing offending policy name", err)
	}
}

// TestApply_UnknownOAuthProvider_FailsLoud — an entry naming an OAuth
// provider that is not in Deps.OAuthProviders fails closed.
func TestApply_UnknownOAuthProvider_FailsLoud(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", OAuth: &config.ToolOAuthConfig{Provider: "no-such-provider", BindingScope: "user"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{},
		},
	})
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrUnknownOAuthProvider) {
		t.Fatalf("err=%v, want ErrUnknownOAuthProvider", err)
	}
	if !strings.Contains(err.Error(), "no-such-provider") {
		t.Errorf("err=%q missing offending provider name", err)
	}
}

// TestApply_MissingApprovalDeps_FailsLoud — declaring approval without
// providing Coordinator/Bus/Redactor fails closed.
func TestApply_MissingApprovalDeps_FailsLoud(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, // no Coordinator/Bus/Redactor
	})
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrCoordinatorRequired) {
		t.Fatalf("err=%v, want ErrCoordinatorRequired", err)
	}
}

// TestApply_ApprovalWrapper_ApproveCycle — the wrapped tool's Invoke
// routes through the gate, the gate APPROVE resolves, and the inner
// tool runs with the original args.
func TestApply_ApprovalWrapper_ApproveCycle(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	applied := map[string]*approval.ApprovalGate{}
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{
			Policy: "deny-all", Reason: "test: human review",
		}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
		AppliedGates: applied,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gate := applied["echo_tool"]
	if gate == nil {
		t.Fatal("AppliedGates did not capture the gate")
	}
	// Resolve the wrapped descriptor.
	d, ok := cat.Resolve("echo_tool")
	if !ok {
		t.Fatal("echo_tool not in catalog after Apply")
	}
	// Subscribe BEFORE kicking Invoke. The bus is fan-out: a subscriber
	// that arrives after the publish never sees the event. Under load
	// (preflight gate, parallel packages) the goroutine's Invoke can
	// reach the gate's publish step before Subscribe completes — the
	// previous ordering flaked under the preflight smoke and surfaced
	// as "no approval requested event" (§17.6).
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  catalogTestID.TenantID,
		User:    catalogTestID.UserID,
		Session: catalogTestID.SessionID,
		Types:   []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()
	// Drive Invoke in a goroutine; resolve from the test thread.
	type out struct {
		res tools.ToolResult
		err error
	}
	resCh := make(chan out, 1)
	args := json.RawMessage(`{"k":"v"}`)
	go func() {
		r, err := d.Invoke(ctxWithID(t, catalogTestID), args)
		resCh <- out{res: r, err: err}
	}()
	var token pauseresume.Token
	for range 50 {
		select {
		case ev := <-sub.Events():
			p, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
			if !ok {
				continue
			}
			token = pauseresume.Token(p.PauseToken)
		case <-time.After(50 * time.Millisecond):
		}
		if token != "" {
			break
		}
	}
	if token == "" {
		t.Fatal("did not observe a tool.approval_requested event with a pause token")
	}
	// Resolve as admin.
	adminCtx := ctxWithAdmin(t, catalogTestID)
	if err := gate.ResolveApproval(adminCtx, token, approval.DecisionApprove, "ok"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("wrapped Invoke err: %v", res.err)
		}
		if got, _ := res.res.Value.(string); got != string(args) {
			t.Errorf("wrapped Invoke returned %q, want %q (the gate must pass through original args)", got, string(args))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped Invoke did not return after approval")
	}
}

// TestApply_ApprovalWrapper_RejectCycle — the wrapped tool's Invoke
// returns *approval.ErrToolRejected when the gate rejects.
func TestApply_ApprovalWrapper_RejectCycle(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	applied := map[string]*approval.ApprovalGate{}
	entries := []config.ToolEntryConfig{
		{Name: "delete_doc", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
		AppliedGates: applied,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gate := applied["delete_doc"]
	d, _ := cat.Resolve("delete_doc")
	// Subscribe BEFORE kicking Invoke (fan-out bus; see HappyPath note).
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: catalogTestID.TenantID, User: catalogTestID.UserID, Session: catalogTestID.SessionID,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	type out struct {
		err error
	}
	resCh := make(chan out, 1)
	go func() {
		_, err := d.Invoke(ctxWithID(t, catalogTestID), json.RawMessage(`{}`))
		resCh <- out{err: err}
	}()
	var token pauseresume.Token
	for range 50 {
		select {
		case ev := <-sub.Events():
			p, _ := ev.Payload.(approval.ToolApprovalRequestedPayload)
			token = pauseresume.Token(p.PauseToken)
		case <-time.After(50 * time.Millisecond):
		}
		if token != "" {
			break
		}
	}
	if token == "" {
		t.Fatal("no approval requested event")
	}
	if err := gate.ResolveApproval(ctxWithAdmin(t, catalogTestID), token, approval.DecisionReject, "policy: bad"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	select {
	case res := <-resCh:
		var rejected *approval.ErrToolRejected
		if !errors.As(res.err, &rejected) {
			t.Fatalf("err=%v, want *ErrToolRejected", res.err)
		}
		if rejected.Tool != "delete_doc" {
			t.Errorf("rejected.Tool=%q, want delete_doc", rejected.Tool)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped Invoke did not return on reject")
	}
}

// TestApply_OAuthWrapper_HappyPath — the wrapped tool's Invoke
// completes when the provider returns a token.
func TestApply_OAuthWrapper_HappyPath(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{}, // returns a valid token
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")
	args := json.RawMessage(`{"repo":"hurtener/Harbor"}`)
	res, err := d.Invoke(ctxWithID(t, catalogTestID), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, _ := res.Value.(string); got != string(args) {
		t.Errorf("Invoke result = %q, want %q", got, string(args))
	}
}

// TestApply_OAuthWrapper_ErrAuthRequiredPropagates — the wrapped
// tool's Invoke returns the typed *ErrAuthRequired so the runtime can
// catch + pause.
func TestApply_OAuthWrapper_ErrAuthRequiredPropagates(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	prov := &stubOAuthProvider{
		tokenErr: &auth.ErrAuthRequired{
			Source: tools.ToolSourceID("test-source"),
		},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": prov,
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")
	_, err := d.Invoke(ctxWithID(t, catalogTestID), json.RawMessage(`{}`))
	var authReq *auth.ErrAuthRequired
	if !errors.As(err, &authReq) {
		t.Fatalf("err=%v, want *auth.ErrAuthRequired", err)
	}
}

// TestApply_OAuthWrapper_NoIdentity_FailsLoud — calling the wrapped
// Invoke without identity in ctx fails loud (defence in depth — the
// provider also enforces this).
func TestApply_OAuthWrapper_NoIdentity_FailsLoud(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "github_read", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat,
		OAuthProviders: map[string]auth.OAuthProvider{
			"github": &stubOAuthProvider{},
		},
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("github_read")
	_, err := d.Invoke(context.Background(), json.RawMessage(`{}`)) // NO identity
	if !errors.Is(err, auth.ErrIdentityRequired) {
		t.Fatalf("err=%v, want auth.ErrIdentityRequired", err)
	}
}

// TestApply_ApprovalWrapper_NoIdentity_FailsLoud — calling the wrapped
// Invoke without identity in ctx fails loud.
func TestApply_ApprovalWrapper_NoIdentity_FailsLoud(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: "deny-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("echo_tool")
	_, err := d.Invoke(context.Background(), json.RawMessage(`{}`)) // NO identity
	if !errors.Is(err, approval.ErrIdentityRequired) {
		t.Fatalf("err=%v, want approval.ErrIdentityRequired", err)
	}
}

// TestApply_AlreadyApplied_FailsLoud — Builder is one-shot.
func TestApply_AlreadyApplied_FailsLoud(t *testing.T) {
	cat, _, _, _ := buildCatalogEnv(t)
	b := catalog.New(nil, catalog.Deps{Catalog: cat})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	err := b.Apply(context.Background())
	if !errors.Is(err, catalog.ErrAlreadyApplied) {
		t.Fatalf("err=%v, want ErrAlreadyApplied", err)
	}
}

// TestApply_BothMiddleware_ApprovalIsOutermost — the composition
// order pins approval outside OAuth. Verified by:
//   - Configure approval (deny-all) AND OAuth (returning ErrAuthRequired
//     on Token) on the same tool.
//   - The approval gate fires FIRST (we see the approval request event).
//   - Rejecting approval returns *ErrToolRejected (NOT *ErrAuthRequired).
//
// If the order were reversed, OAuth's ErrAuthRequired would propagate
// upward BEFORE the approval gate had a chance to fire.
func TestApply_BothMiddleware_ApprovalIsOutermost(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	applied := map[string]*approval.ApprovalGate{}
	prov := &stubOAuthProvider{
		tokenErr: &auth.ErrAuthRequired{Source: tools.ToolSourceID("test-source")},
	}
	entries := []config.ToolEntryConfig{
		{
			Name:     "delete_doc",
			Approval: &config.ToolApprovalConfig{Policy: "deny-all"},
			OAuth:    &config.ToolOAuthConfig{Provider: "github", BindingScope: "user"},
		},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
		OAuthProviders: map[string]auth.OAuthProvider{"github": prov},
		AppliedGates:   applied,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gate := applied["delete_doc"]
	d, _ := cat.Resolve("delete_doc")
	// Subscribe BEFORE kicking Invoke (fan-out bus; see HappyPath note).
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: catalogTestID.TenantID, User: catalogTestID.UserID, Session: catalogTestID.SessionID,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	type out struct {
		err error
	}
	resCh := make(chan out, 1)
	go func() {
		_, err := d.Invoke(ctxWithID(t, catalogTestID), json.RawMessage(`{}`))
		resCh <- out{err: err}
	}()
	var token pauseresume.Token
	for range 50 {
		select {
		case ev := <-sub.Events():
			p, _ := ev.Payload.(approval.ToolApprovalRequestedPayload)
			token = pauseresume.Token(p.PauseToken)
		case <-time.After(50 * time.Millisecond):
		}
		if token != "" {
			break
		}
	}
	if token == "" {
		t.Fatal("approval request did not fire — OAuth wrapper must be inside approval (order is approval outermost)")
	}
	// Reject — we expect *ErrToolRejected, NOT *ErrAuthRequired.
	// (If OAuth were outermost, the call would never have reached
	// the approval gate — it would have errored with ErrAuthRequired.)
	if err := gate.ResolveApproval(ctxWithAdmin(t, catalogTestID), token, approval.DecisionReject, "test"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	res := <-resCh
	var rejected *approval.ErrToolRejected
	if !errors.As(res.err, &rejected) {
		t.Fatalf("err=%v, want *ErrToolRejected (approval is outermost)", res.err)
	}
}

// TestValidateTools_PolicyAllowlistMirrors_ApprovalPackage — the
// config-package's `allowedApprovalPolicies` set MUST stay in
// lockstep with the bundled `approval.ApprovalPolicy` implementations.
// Drift between the two (a new policy added to one but not the other)
// is the smell this guard pins down.
func TestValidateTools_PolicyAllowlistMirrors_ApprovalPackage(t *testing.T) {
	// Each name in the config allowlist MUST resolve via
	// catalog.resolveApprovalPolicy (the unexported function whose
	// equivalent test surface IS this test — we drive it via
	// catalog.New + Apply).
	for _, name := range []string{"deny-all", "approve-all", "tagged"} {
		cat, coord, bus, red := buildCatalogEnv(t)
		ents := []config.ToolEntryConfig{
			{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: name, RequireTags: []string{"x"}}},
		}
		b := catalog.New(ents, catalog.Deps{
			Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
		})
		if err := b.Apply(context.Background()); err != nil {
			t.Errorf("policy %q: Apply: %v (config allowlist names a policy the builder cannot resolve)", name, err)
		}
	}
}

// TestValidateTools_BindingScopeAllowlistMirrors_AuthPackage — same
// guard for OAuth binding scopes.
func TestValidateTools_BindingScopeAllowlistMirrors_AuthPackage(t *testing.T) {
	for _, scope := range []string{"user", "agent"} {
		cat, _, _, _ := buildCatalogEnv(t)
		ents := []config.ToolEntryConfig{
			{Name: "echo_tool", OAuth: &config.ToolOAuthConfig{Provider: "github", BindingScope: scope}},
		}
		b := catalog.New(ents, catalog.Deps{
			Catalog: cat,
			OAuthProviders: map[string]auth.OAuthProvider{
				"github": &stubOAuthProvider{},
			},
		})
		if err := b.Apply(context.Background()); err != nil {
			t.Errorf("binding_scope %q: Apply: %v (config allowlist names a scope the builder cannot resolve)", scope, err)
		}
	}
}

// TestApply_AppliedGates_NilMapDoesNotPanic — passing AppliedGates=nil
// is supported and a no-op.
func TestApply_AppliedGates_NilMapDoesNotPanic(t *testing.T) {
	cat, coord, bus, red := buildCatalogEnv(t)
	entries := []config.ToolEntryConfig{
		{Name: "echo_tool", Approval: &config.ToolApprovalConfig{Policy: "approve-all"}},
	}
	b := catalog.New(entries, catalog.Deps{
		Catalog: cat, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err := b.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	d, _ := cat.Resolve("echo_tool")
	_, err := d.Invoke(ctxWithID(t, catalogTestID), json.RawMessage(`{}`))
	if err != nil {
		t.Errorf("approve-all Invoke err: %v", err)
	}
}
