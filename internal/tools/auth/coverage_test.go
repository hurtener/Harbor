package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/tools"
)

// Supplemental tests covering helper paths the happy-path / failure
// tests don't naturally exercise. Coverage hygiene per
// CLAUDE.md §11; master-plan Phase 30 coverage target is 85%.

func TestErrAuthRequired_Error_NilReceiver(t *testing.T) {
	t.Parallel()
	var e *ErrAuthRequired
	got := e.Error()
	if got == "" {
		t.Fatal("nil receiver Error() must return non-empty")
	}
}

func TestErrAuthRequired_Error_DefaultMessage(t *testing.T) {
	t.Parallel()
	e := &ErrAuthRequired{Source: tools.ToolSourceID("src")}
	if e.Error() == "" {
		t.Fatal("non-nil receiver Error() must return non-empty")
	}
	e2 := &ErrAuthRequired{Source: "x", Message: "custom"}
	if e2.Error() != "auth: custom" {
		t.Fatalf("with Message: got %q want \"auth: custom\"", e2.Error())
	}
}

func TestOAuthConfig_Validate_AllFailureModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  OAuthConfig
		want error
	}{
		{"empty source", OAuthConfig{BindingScope: ScopeUser, RedirectURI: "x", ServerURL: "y"}, ErrConfigInvalid},
		{"invalid binding scope", OAuthConfig{Source: "s", BindingScope: "weird"}, ErrInvalidBindingScope},
		{"agent scope no agent id", OAuthConfig{Source: "s", BindingScope: ScopeAgent}, ErrAgentIDRequired},
		{"no redirect uri", OAuthConfig{Source: "s", BindingScope: ScopeUser, ServerURL: "x"}, ErrConfigInvalid},
		{"no server url no token url", OAuthConfig{Source: "s", BindingScope: ScopeUser, RedirectURI: "x"}, ErrConfigInvalid},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if !errors.Is(err, c.want) {
				t.Fatalf("Validate: want %v, got %v", c.want, err)
			}
		})
	}
}

func TestOAuthConfig_Validate_ValidShape(t *testing.T) {
	t.Parallel()
	cfg := OAuthConfig{
		Source: "s", BindingScope: ScopeUser,
		RedirectURI: "http://x", ServerURL: "http://y",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid cfg should pass; got %v", err)
	}
	agentCfg := OAuthConfig{
		Source: "s", BindingScope: ScopeAgent, AgentID: "a",
		RedirectURI: "http://x", AuthorizeURL: "http://az", TokenURL: "http://tk",
	}
	if err := agentCfg.Validate(); err != nil {
		t.Fatalf("valid agent cfg should pass; got %v", err)
	}
}

func TestOAuthConfig_SubjectID(t *testing.T) {
	t.Parallel()
	id := mkIdentity(t)
	user := OAuthConfig{BindingScope: ScopeUser}
	if user.SubjectID(id) != id.UserID {
		t.Fatal("user-scope SubjectID mismatch")
	}
	agent := OAuthConfig{BindingScope: ScopeAgent, AgentID: "ag"}
	if agent.SubjectID(id) != "ag" {
		t.Fatal("agent-scope SubjectID mismatch")
	}
	bad := OAuthConfig{BindingScope: "weird"}
	if bad.SubjectID(id) != "" {
		t.Fatal("invalid-scope SubjectID should be empty")
	}
}

func TestToken_SubjectID(t *testing.T) {
	t.Parallel()
	u := Token{BindingScope: ScopeUser, UserID: "alice"}
	if u.SubjectID() != "alice" {
		t.Fatal("user-bound Token SubjectID mismatch")
	}
	a := Token{BindingScope: ScopeAgent, AgentID: "ag"}
	if a.SubjectID() != "ag" {
		t.Fatal("agent-bound Token SubjectID mismatch")
	}
	bad := Token{BindingScope: "weird"}
	if bad.SubjectID() != "" {
		t.Fatal("invalid-scope Token SubjectID should be empty")
	}
}

func TestProvider_ConfigFor(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	cfg, ok := h.provider.ConfigFor(h.userCfg.Source)
	if !ok {
		t.Fatal("ConfigFor: known source not found")
	}
	if cfg.Source != h.userCfg.Source {
		t.Fatalf("ConfigFor: returned wrong cfg")
	}
	_, ok = h.provider.ConfigFor("unknown")
	if ok {
		t.Fatal("ConfigFor: unknown source returned true")
	}
}

func TestProvider_DuplicateConfig_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	dup := h.userCfg // same Source as already configured
	_, err := NewProvider([]OAuthConfig{h.userCfg, dup}, ProviderDeps{
		Store: h.store, Bus: h.bus, Redactor: h.redactor, Coordinator: h.coordinator,
	})
	if err == nil {
		t.Fatal("duplicate OAuthConfig should fail loud at construction")
	}
}

func TestProvider_Revoke_UnknownSource_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	ctx := mkCtx(t, mkIdentity(t))
	if err := h.provider.Revoke(ctx, "unknown"); !errors.Is(err, ErrConfigInvalid) {
		t.Fatalf("Revoke unknown source: want ErrConfigInvalid, got %v", err)
	}
}

func TestProvider_Revoke_HappyPath_UserBound(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Seed a token directly via the TokenStore.
	tok := Token{
		Source: h.userCfg.Source, BindingScope: ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID, AccessToken: "x",
	}
	if err := h.store.Put(ctx, tok); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	if err := h.provider.Revoke(ctx, h.userCfg.Source); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	_, ok, err := h.store.Get(ctx, ScopeUser, id.UserID, h.userCfg.Source)
	if err != nil {
		t.Fatalf("Get post-revoke: %v", err)
	}
	if ok {
		t.Fatal("Revoke did not remove the token")
	}
}

func TestProvider_InitiateFlow_MissingIdentity_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	_, err := h.provider.InitiateFlow(context.Background(), h.userCfg.Source)
	if !errors.Is(err, ErrIdentityRequired) {
		t.Fatalf("InitiateFlow no-identity: want ErrIdentityRequired, got %v", err)
	}
}

func TestProvider_CompleteFlow_MissingIdentity_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	_, err := h.provider.CompleteFlow(context.Background(), "state", "code")
	if !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("CompleteFlow no-identity: want ErrFlowNotFound (state lookup happens first), got %v", err)
	}
}

func TestProvider_CompleteFlow_ExpiredFlow_FailLoud(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)
	_, err := h.provider.Token(ctx, h.userCfg.Source)
	var authErr *ErrAuthRequired
	if !errors.As(err, &authErr) {
		t.Fatal("Token did not return ErrAuthRequired")
	}
	// Force-expire the flow by mutating the flow record.
	h.provider.flowsMu.Lock()
	rec, ok := h.provider.flows[authErr.State]
	if !ok {
		h.provider.flowsMu.Unlock()
		t.Fatal("flow not registered")
	}
	rec.ExpiresAt = time.Now().Add(-time.Hour)
	h.provider.flowsMu.Unlock()

	code, _, err := h.server.VisitAuthorizeURL(authErr.AuthorizeURL)
	if err != nil {
		t.Fatalf("VisitAuthorizeURL: %v", err)
	}
	_, err = h.provider.CompleteFlow(ctx, authErr.State, code)
	if !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired flow: want ErrFlowExpired, got %v", err)
	}
}

func TestProvider_Token_IsExpired_FreshToken(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Seed a fresh, non-expired token directly via the TokenStore.
	tok := Token{
		Source: h.userCfg.Source, BindingScope: ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID,
		AccessToken: "fresh-access", TokenType: "Bearer",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := h.store.Put(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := h.provider.Token(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got.AccessToken != "fresh-access" {
		t.Fatalf("fresh token expected; got %q", got.AccessToken)
	}
}

func TestSplitScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"repo", []string{"repo"}},
		{"repo read:user", []string{"repo", "read:user"}},
		{"  repo   read:user  ", []string{"repo", "read:user"}},
	}
	for _, c := range cases {
		got := splitScopes(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitScopes(%q) len: got %d want %d", c.in, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitScopes(%q)[%d]: got %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestNonEmpty(t *testing.T) {
	t.Parallel()
	if nonEmpty("a", "b") != "a" {
		t.Fatal("a wins when set")
	}
	if nonEmpty("", "b") != "b" {
		t.Fatal("b wins when a empty")
	}
	if nonEmpty("", "") != "" {
		t.Fatal("both empty stays empty")
	}
}

func TestSummary(t *testing.T) {
	t.Parallel()
	short := []byte("ok")
	if summary(short) != "ok" {
		t.Fatal("short summary mismatch")
	}
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'a'
	}
	got := summary(long)
	if len(got) != 200+len("…") {
		t.Fatalf("long summary length: got %d", len(got))
	}
}

func TestExpiresAt_EdgeCases(t *testing.T) {
	t.Parallel()
	now := time.Now()
	zero := tokenExchangeResponse{}
	if !zero.expiresAt(now).IsZero() {
		t.Fatal("zero ExpiresIn should produce zero time")
	}
	negative := tokenExchangeResponse{ExpiresIn: -1}
	if !negative.expiresAt(now).IsZero() {
		t.Fatal("negative ExpiresIn should produce zero time")
	}
	positive := tokenExchangeResponse{ExpiresIn: 3600}
	got := positive.expiresAt(now)
	if got.Sub(now) != time.Hour {
		t.Fatalf("expiresAt: got delta %v want 1h", got.Sub(now))
	}
}

func TestProvider_Token_FreshNonExpired_NoStorage(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Seed a fresh, ZERO-ExpiresAt (long-lived) token.
	tok := Token{
		Source: h.userCfg.Source, BindingScope: ScopeUser,
		TenantID: id.TenantID, UserID: id.UserID,
		AccessToken: "long-lived",
	}
	if err := h.store.Put(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := h.provider.Token(ctx, h.userCfg.Source)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got.AccessToken != "long-lived" {
		t.Fatalf("long-lived token: got %q", got.AccessToken)
	}
}

func TestProvider_IsExpired_ZeroExpiry_NeverExpired(t *testing.T) {
	t.Parallel()
	h := newProviderHarness(t)
	if h.provider.isExpired(Token{}) {
		t.Fatal("zero ExpiresAt should be treated as not-expired")
	}
}

func TestNewPKCEVerifier(t *testing.T) {
	t.Parallel()
	a, err := newPKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newPKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatal("empty verifier")
	}
	if a == b {
		t.Fatal("two verifiers should differ")
	}
}

func TestNewState(t *testing.T) {
	t.Parallel()
	a, err := newState()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newState()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two state tokens should differ")
	}
}
