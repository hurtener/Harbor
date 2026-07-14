package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/annotate"
	"github.com/hurtener/Harbor/internal/tools/approval"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestE2E_ToolsAnnotator_RealDrivers wires the production per-tool Annotator
// across REAL subsystems — the events bus (inmem), the StateStore-backed
// approval-policy store, the tools/auth OAuth Provider + TokenStore, and a
// real ToolCatalog — behind the tools/protocol Service, and proves the whole
// seam is alive end-to-end (CLAUDE.md §17.1):
//
//   - tools.metrics reflects real recorded invocations (identity-scoped);
//   - a Bound OAuth binding narrows the tools.list oauth_statuses facet, and
//     tools.revoke_oauth flips it back to Required (the admin write path);
//   - tools.set_approval_policy persists through tools/approval and
//     tools.describe reads the new policy back (the admin write round-trip);
//   - cross-session isolation: session A's metrics never reach session B;
//   - a failure mode: metrics on an unknown tool fails loud.
//
// Runs under -race (the file-level gate).
func TestE2E_ToolsAnnotator_RealDrivers(t *testing.T) {
	ctx := context.Background()
	red := auditpatterns.New()

	// Real event bus with a replay ring (the metrics read substrate).
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		ReplayBufferSize:         512,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(ctx) })

	// Real StateStore — backs BOTH the approval-policy store and the OAuth
	// token store.
	st, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })

	// Real tools/auth Provider over a real sealed TokenStore.
	kek := make([]byte, toolauth.KEKSizeBytes)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealer, err := toolauth.NewAESGCMSealer(kek)
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	tokenStore, err := toolauth.NewTokenStore(st, sealer)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}
	const ghSource = tools.ToolSourceID("gh")
	provider, err := toolauth.NewProvider([]toolauth.OAuthConfig{{
		Source:       ghSource,
		BindingScope: toolauth.ScopeUser,
		ServerURL:    "https://oauth.example.test",
		RedirectURI:  "https://app.example.test/callback",
	}}, toolauth.ProviderDeps{
		Store:       tokenStore,
		Bus:         bus,
		Redactor:    red,
		Coordinator: pauseresume.New(),
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	// Real catalog: one OAuth-bound MCP tool + one plain in-proc tool.
	cat := tools.NewCatalog()
	register := func(name string, src tools.ToolSourceID, tr tools.TransportKind, se tools.SideEffect) {
		if err := cat.Register(tools.ToolDescriptor{
			Tool:   tools.Tool{Name: name, Description: name, Source: src, Transport: tr, SideEffects: se, Loading: tools.LoadingAlways},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil },
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	register("gh_tool", ghSource, tools.TransportMCP, tools.SideEffectExternal)
	register("plain_tool", "", tools.TransportInProcess, tools.SideEffectRead)

	// Real approval-policy store.
	policyStore, err := approval.NewStatePolicyStore(st)
	if err != nil {
		t.Fatalf("NewStatePolicyStore: %v", err)
	}

	// The production Annotator over ALL real deps.
	annotator, err := annotate.NewAnnotator(annotate.Deps{
		Catalog:             cat,
		Approval:            policyStore,
		Events:              bus,
		OAuth:               annotate.NewProviderOAuthReader(map[string]toolauth.OAuthProvider{"gh": provider}),
		HeavyThresholdBytes: 32 * 1024,
	})
	if err != nil {
		t.Fatalf("NewAnnotator: %v", err)
	}

	proj, err := toolsprotocol.NewCatalogProjector(cat, toolsprotocol.WithAnnotator(annotator))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj, toolsprotocol.WithBus(bus), toolsprotocol.WithRedactor(red))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	idA := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-A"}
	scopeA := prototypes.IdentityScope{Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID}
	ctxA, err := identity.With(ctx, idA)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// --- Metrics reflect real recorded invocations -------------------------
	now := time.Now().UTC()
	for range 4 {
		if err := bus.Publish(ctxA, events.Event{
			Type: tools.EventTypeToolCompleted, Identity: identity.Quadruple{Identity: idA},
			OccurredAt: now.Add(-time.Minute), Payload: tools.ToolCompletedPayload{ToolName: "gh_tool"},
		}); err != nil {
			t.Fatalf("publish completed: %v", err)
		}
	}
	if err := bus.Publish(ctxA, events.Event{
		Type: tools.EventTypeToolFailed, Identity: identity.Quadruple{Identity: idA},
		OccurredAt: now.Add(-30 * time.Second), Payload: tools.ToolFailedPayload{ToolName: "gh_tool", ErrorClass: "boom"},
	}); err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	m, err := svc.Metrics(ctxA, prototypes.ToolMetricsRequest{Identity: scopeA, ID: "gh_tool", Window: prototypes.ToolWindow1h})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Invocations != 5 || m.Failures != 1 {
		t.Fatalf("gh_tool metrics = inv %d / fail %d, want 5 / 1", m.Invocations, m.Failures)
	}
	if m.ErrorRate1h <= 0 {
		t.Fatalf("ErrorRate1h = %v, want > 0 (real recorded failure)", m.ErrorRate1h)
	}

	// --- OAuth: bind → facet narrows → revoke → Required -------------------
	// Directly seed a live token binding (the CompleteFlow terminal state)
	// under session A's identity.
	if err := tokenStore.Put(ctxA, toolauth.Token{
		Source:       ghSource,
		BindingScope: toolauth.ScopeUser,
		TenantID:     idA.TenantID,
		UserID:       idA.UserID,
		AccessToken:  "access-xyz",
		TokenType:    "Bearer",
		ExpiresAt:    now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("tokenStore.Put: %v", err)
	}
	// The oauth_statuses:[Bound] facet now narrows to gh_tool alone.
	boundList, err := svc.List(ctxA, prototypes.ToolListRequest{
		Identity: scopeA,
		Filter:   prototypes.ToolFilter{OAuthStatuses: []prototypes.ToolOAuthStatus{prototypes.ToolOAuthBound}},
	})
	if err != nil {
		t.Fatalf("List(oauth Bound): %v", err)
	}
	if len(boundList.Tools) != 1 || boundList.Tools[0].ID != "gh_tool" {
		t.Fatalf("Bound facet = %+v, want exactly gh_tool", boundList.Tools)
	}
	if boundList.AggregatesPartial {
		t.Fatal("aggregates marked partial with the annotator wired — the flip did not happen")
	}
	// Revoke through the admin write path.
	rev, err := svc.RevokeOAuth(ctxA, prototypes.ToolRevokeOAuthRequest{Identity: scopeA, ID: "gh_tool"}, true)
	if err != nil {
		t.Fatalf("RevokeOAuth: %v", err)
	}
	if rev.RevokedCount < 1 {
		t.Fatalf("RevokedCount = %d, want >= 1 (a real binding was revoked)", rev.RevokedCount)
	}
	// After revoke, gh_tool reads Required (the facet flips).
	reqList, err := svc.List(ctxA, prototypes.ToolListRequest{
		Identity: scopeA,
		Filter:   prototypes.ToolFilter{OAuthStatuses: []prototypes.ToolOAuthStatus{prototypes.ToolOAuthRequired}},
	})
	if err != nil {
		t.Fatalf("List(oauth Required): %v", err)
	}
	if len(reqList.Tools) != 1 || reqList.Tools[0].ID != "gh_tool" {
		t.Fatalf("Required facet after revoke = %+v, want exactly gh_tool", reqList.Tools)
	}
	// A SECOND revoke of the now-unbound-but-CONFIGURED source reports
	// RevokedCount 0 — no real binding remains to revoke, so the idempotent
	// terminal delete is not over-counted as a revocation.
	rev2, err := svc.RevokeOAuth(ctxA, prototypes.ToolRevokeOAuthRequest{Identity: scopeA, ID: "gh_tool"}, true)
	if err != nil {
		t.Fatalf("RevokeOAuth (second, unbound): %v", err)
	}
	if rev2.RevokedCount != 0 {
		t.Fatalf("RevokedCount on an unbound-but-configured source = %d, want 0 (idempotent no-op, not an over-count)", rev2.RevokedCount)
	}

	// --- Approval-policy admin write round-trips ---------------------------
	if _, err := svc.SetApprovalPolicy(ctxA, prototypes.ToolSetApprovalPolicyRequest{
		Identity: scopeA, ID: "gh_tool", Policy: prototypes.ToolApprovalGated,
	}, true); err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	desc, err := svc.Describe(ctxA, prototypes.ToolDescribeRequest{Identity: scopeA, ID: "gh_tool"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc.Tool.ApprovalPolicy != prototypes.ToolApprovalGated {
		t.Fatalf("described policy = %q, want gated (the persisted admin write)", desc.Tool.ApprovalPolicy)
	}

	// --- Cross-session isolation -------------------------------------------
	idB := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-B"}
	scopeB := prototypes.IdentityScope{Tenant: idB.TenantID, User: idB.UserID, Session: idB.SessionID}
	ctxB, err := identity.With(ctx, idB)
	if err != nil {
		t.Fatalf("identity.With B: %v", err)
	}
	mB, err := svc.Metrics(ctxB, prototypes.ToolMetricsRequest{Identity: scopeB, ID: "gh_tool", Window: prototypes.ToolWindow1h})
	if err != nil {
		t.Fatalf("Metrics B: %v", err)
	}
	if mB.Invocations != 0 || mB.Failures != 0 {
		t.Fatalf("session B saw session A's metrics: %+v — cross-session bleed", mB)
	}
	// Session B's approval policy is unaffected by session A's set.
	descB, err := svc.Describe(ctxB, prototypes.ToolDescribeRequest{Identity: scopeB, ID: "gh_tool"})
	if err != nil {
		t.Fatalf("Describe B: %v", err)
	}
	if descB.Tool.ApprovalPolicy != prototypes.ToolApprovalAuto {
		t.Fatalf("session B policy = %q, want auto — cross-session bleed", descB.Tool.ApprovalPolicy)
	}

	// --- Failure mode: metrics on an unknown tool fails loud ---------------
	if _, err := svc.Metrics(ctxA, prototypes.ToolMetricsRequest{Identity: scopeA, ID: "no-such-tool"}); err == nil {
		t.Fatal("Metrics on an unknown tool should fail loud (ErrToolNotFound), not return a fabricated zero")
	}
}
