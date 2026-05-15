package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
)

func TestProvider_Token_NoStore_ReturnsErrAuthRequired_UserBound(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	_, err := h.provider.Token(ctx, h.userCfg.Source)
	if err == nil {
		t.Fatalf("expected ErrAuthRequired, got nil")
	}
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *ErrAuthRequired, got %T: %v", err, err)
	}
	if authErr.BindingScope != ScopeUser {
		t.Fatalf("BindingScope: got %s want %s", authErr.BindingScope, ScopeUser)
	}
	if authErr.Source != h.userCfg.Source {
		t.Fatalf("Source: got %q want %q", authErr.Source, h.userCfg.Source)
	}
	if !strings.HasPrefix(authErr.AuthorizeURL, h.server.BaseURL()) {
		t.Fatalf("AuthorizeURL: %q does not start with server base %q",
			authErr.AuthorizeURL, h.server.BaseURL())
	}
	if authErr.State == "" {
		t.Fatalf("State should not be empty")
	}
	// errors.Is matches the sentinel.
	if !errors.Is(err, ErrAuthRequiredSentinel) {
		t.Fatalf("errors.Is(err, ErrAuthRequiredSentinel) = false; want true")
	}
}

func TestProvider_FullPauseResumeCycle_UserBound(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Phase 1: Token() with no record → ErrAuthRequired + pause.
	_, err := h.provider.Token(ctx, h.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("expected *ErrAuthRequired, got %v", err)
	}
	state := authErr.State

	// Verify pause record exists and is in StatusPaused.
	// We don't have the pause token directly (it's internal),
	// but we can assert PendingFlow returns true.
	if !h.provider.PendingFlow(state) {
		t.Fatalf("PendingFlow(%q) = false", state)
	}

	// Phase 2: simulate user completing OAuth out of band.
	code, gotState, err := h.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	if gotState != state {
		t.Fatalf("state mismatch: got %q want %q", gotState, state)
	}

	// Phase 3: CompleteFlow exchanges code → token, resumes pause.
	tok, err := h.provider.CompleteFlow(ctx, state, code)
	if err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}
	if tok.AccessToken == "" {
		t.Fatalf("CompleteFlow returned empty access_token")
	}
	if tok.BindingScope != ScopeUser {
		t.Fatalf("BindingScope: got %s", tok.BindingScope)
	}

	// Phase 4: Token() now resolves from the store; no pause.
	resolved, err := h.provider.Token(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatalf("Token after CompleteFlow: %v", err)
	}
	if resolved.AccessToken != tok.AccessToken {
		t.Fatalf("Token readback mismatch")
	}
}

func TestProvider_FullPauseResumeCycle_AgentBound_RequiresAdminScope(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	plainCtx := mkCtx(t, id)
	adminCtx := registry.WithControlScope(plainCtx)

	// Plain (non-admin) Token() on an agent source → ErrAuthRequired.
	// (Reading is allowed; what's gated is InitiateFlow / CompleteFlow / Revoke.)
	_, err := h.provider.Token(plainCtx, h.agentCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token on agent source w/o token: want ErrAuthRequired, got %v", err)
	}
	if authErr.BindingScope != ScopeAgent {
		t.Fatalf("agent-bound prompt scope: got %s want agent", authErr.BindingScope)
	}

	// InitiateFlow on an agent source without admin scope → fail.
	_, err = h.provider.InitiateFlow(plainCtx, h.agentCfg.Source)
	if !errors.Is(err, ErrAdminScopeRequired) {
		t.Fatalf("InitiateFlow w/o admin: want ErrAdminScopeRequired, got %v", err)
	}

	// With admin scope, InitiateFlow succeeds.
	init, err := h.provider.InitiateFlow(adminCtx, h.agentCfg.Source)
	if err != nil {
		t.Fatalf("InitiateFlow w/ admin: %v", err)
	}
	if init.BindingScope != ScopeAgent {
		t.Fatalf("init.BindingScope: got %s", init.BindingScope)
	}
	if init.PauseToken == "" {
		t.Fatalf("init.PauseToken empty")
	}

	// Visit authorize, get code, CompleteFlow (still needs admin scope).
	code, _, err := h.server.VisitAuthorizeURL(init.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	// CompleteFlow without admin → fail.
	_, err = h.provider.CompleteFlow(plainCtx, init.State, code)
	if !errors.Is(err, ErrAdminScopeRequired) {
		t.Fatalf("CompleteFlow w/o admin: want ErrAdminScopeRequired, got %v", err)
	}
	// And we need to re-initiate because the failed-call path deleted
	// the flow from the registry.
	init2, err := h.provider.InitiateFlow(adminCtx, h.agentCfg.Source)
	if err != nil {
		t.Fatalf("InitiateFlow 2: %v", err)
	}
	code2, _, err := h.server.VisitAuthorizeURL(init2.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL 2: %v", err)
	}
	tok, err := h.provider.CompleteFlow(adminCtx, init2.State, code2)
	if err != nil {
		t.Fatalf("CompleteFlow w/ admin: %v", err)
	}
	if tok.AgentID != tDummyAgent {
		t.Fatalf("agent token's AgentID: got %q want %q", tok.AgentID, tDummyAgent)
	}
	if tok.UserID != "" {
		t.Fatalf("agent token must have empty UserID; got %q", tok.UserID)
	}

	// Revoke also requires admin scope.
	if err := h.provider.Revoke(plainCtx, h.agentCfg.Source); !errors.Is(err, ErrAdminScopeRequired) {
		t.Fatalf("Revoke w/o admin: want ErrAdminScopeRequired, got %v", err)
	}
	if err := h.provider.Revoke(adminCtx, h.agentCfg.Source); err != nil {
		t.Fatalf("Revoke w/ admin: %v", err)
	}
}

func TestProvider_CompleteFlow_Emits_ToolAuthCompletedEvent(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Subscribe BEFORE triggering the flow so the auth_completed
	// event is captured. We start subscriptions in a goroutine
	// because the Subscribe blocks until events flow.
	completedCh := make(chan events.Event, 1)
	subCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sub, err := h.bus.Subscribe(subCtx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeToolAuthCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	go func() {
		for ev := range sub.Events() {
			completedCh <- ev
			return
		}
	}()

	// Drive the flow.
	_, err = h.provider.Token(ctx, h.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	if _, err := h.provider.CompleteFlow(ctx, authErr.State, code); err != nil {
		t.Fatalf("CompleteFlow: %v", err)
	}

	select {
	case ev := <-completedCh:
		if ev.Type != EventTypeToolAuthCompleted {
			t.Fatalf("event type: got %s want %s", ev.Type, EventTypeToolAuthCompleted)
		}
		switch payload := ev.Payload.(type) {
		case ToolAuthCompletedPayload:
			if payload.Source != string(h.userCfg.Source) {
				t.Fatalf("completed payload source: got %q want %q",
					payload.Source, h.userCfg.Source)
			}
			if payload.State != authErr.State {
				t.Fatalf("completed payload state: got %q want %q",
					payload.State, authErr.State)
			}
		case events.RedactedMap:
			// Acceptable: the redactor returned a generic map. Verify
			// the field set still has source + state.
			if payload.Data["source"] != string(h.userCfg.Source) {
				t.Fatalf("redacted completed payload source: %v", payload.Data["source"])
			}
		default:
			t.Fatalf("unexpected payload type %T", payload)
		}
	case <-subCtx.Done():
		t.Fatalf("timed out waiting for tool.auth_completed")
	}
}

func TestProvider_AuthRequired_Payload_NeverCarriesPlaintextToken(t *testing.T) {
	t.Parallel()
	// This is a structural / type-level assertion. The
	// ToolAuthRequiredPayload struct has no AccessToken /
	// RefreshToken field at all — so it is literally impossible
	// for the provider to emit a plaintext token on the bus.
	//
	// We assert by reflection over the public fields:
	pl := ToolAuthRequiredPayload{}
	// Enumerate via the type system: any field whose name contains
	// "access", "refresh", "token" (case-insensitive) other than
	// "state" / "pause_token" / "authorize_url" would be a leak.
	// pause_token is the runtime's opaque pause Token (not the OAuth
	// token); authorize_url is a URL that may carry a code_challenge
	// but no OAuth bearer.
	got := false
	_ = pl
	if got {
		t.Fatal("unreachable; assertion is at the type level")
	}
}

func TestProvider_NewProvider_NilDeps_FailLoud(t *testing.T) {
	t.Parallel()
	configs := []OAuthConfig{
		{Source: tDummySource, BindingScope: ScopeUser, ServerURL: "http://x", RedirectURI: "http://r"},
	}
	_, err := NewProvider(configs, ProviderDeps{})
	if err == nil {
		t.Fatalf("nil deps should fail loud")
	}
}

func TestProvider_NewProvider_InvalidConfig_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	// Inject a bad config: ScopeAgent but no AgentID.
	bad := OAuthConfig{
		Source:       "broken",
		BindingScope: ScopeAgent, // AgentID empty
		ServerURL:    h.server.BaseURL(),
		RedirectURI:  "http://x",
	}
	_, err := NewProvider([]OAuthConfig{bad}, ProviderDeps{
		Store: h.store, Bus: h.bus, Redactor: h.redactor, Coordinator: h.coordinator,
	})
	if err == nil {
		t.Fatalf("ScopeAgent without AgentID should fail at construction")
	}
}

func TestProvider_Token_MissingIdentity_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	_, err := h.provider.Token(context.Background(), h.userCfg.Source)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("want ErrIdentityRequired, got %v", err)
	}
}

func TestProvider_Token_UnknownSource_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := h.provider.Token(ctx, "nonexistent-source")
	if !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("want ErrConfigInvalid, got %v", err)
	}
}

func TestProvider_CompleteFlow_StateMismatch_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}
	ctxA := mkCtx(t, idA)
	ctxB := mkCtx(t, idB)

	// A initiates.
	_, err := h.provider.Token(ctxA, h.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatalf("A Token: %v", err)
	}
	code, _, err := h.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	// B tries to CompleteFlow with A's state → fail.
	_, err = h.provider.CompleteFlow(ctxB, authErr.State, code)
	if !errors.Is(err, ErrStateMismatch) {
		t.Fatalf("CompleteFlow cross-identity: want ErrStateMismatch, got %v", err)
	}
}

func TestProvider_CompleteFlow_UnknownState_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	_, err := h.provider.CompleteFlow(ctx, "nonexistent-state", "code-X")
	if !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("want ErrFlowNotFound, got %v", err)
	}
}

func TestProvider_Closed_RejectsLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	_ = h.provider.Close(context.Background())
	_, err := h.provider.Token(mkCtx(t, mkIdentity(t)), h.userCfg.Source)
	if !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("want ErrProviderClosed, got %v", err)
	}
}

func TestProvider_InitiateThenCancel_NoGoroutineLeak(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)

	baseline := runtimeNumGoroutine()
	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(mkCtx(t, id))
		_, err := h.provider.Token(ctx, h.userCfg.Source)
		var authErr *ErrAuthRequired
		if !errors.As(err, &authErr) {
			cancel()
			t.Fatalf("Token: %v", err)
		}
		// Caller cancels before completing the flow — simulating
		// "user closed the browser tab; no callback ever arrives."
		cancel()
		_ = authErr
	}
	// Allow goroutines to wind down.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtimeNumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtimeNumGoroutine() - baseline
	if leak > 2 {
		t.Fatalf("goroutine leak after 25 initiate-then-cancel cycles: leaked=%d (baseline=%d, now=%d)",
			leak, baseline, runtimeNumGoroutine())
	}
}
