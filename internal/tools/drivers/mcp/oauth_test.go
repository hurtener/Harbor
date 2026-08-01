package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// stubOAuthProvider is a minimal auth.OAuthProvider for the driver's binding
// + injection tests. Token returns a fixed token, or a configured error
// (including a typed *auth.ErrAuthRequired) to exercise the fail-closed path.
type stubOAuthProvider struct {
	token        string
	tokErr       error
	calls        int
	lastSrc      tools.ToolSourceID
	allowedHosts []string
}

func (s *stubOAuthProvider) Token(_ context.Context, source tools.ToolSourceID) (auth.Token, error) {
	s.calls++
	s.lastSrc = source
	if s.tokErr != nil {
		return auth.Token{}, s.tokErr
	}
	return auth.Token{AccessToken: s.token, BindingScope: auth.ScopeUser}, nil
}
func (s *stubOAuthProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, errors.New("not implemented")
}
func (s *stubOAuthProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, errors.New("not implemented")
}
func (s *stubOAuthProvider) PendingFlow(context.Context, string) (auth.PendingFlowInfo, bool, error) {
	return auth.PendingFlowInfo{}, false, nil
}
func (s *stubOAuthProvider) DenyFlow(context.Context, string, string) error { return nil }
func (s *stubOAuthProvider) Revoke(context.Context, tools.ToolSourceID) error {
	return nil
}
func (s *stubOAuthProvider) Close(context.Context) error { return nil }
func (s *stubOAuthProvider) AllowedDownstreamHosts() []string {
	return append([]string(nil), s.allowedHosts...)
}

var _ auth.OAuthProvider = (*stubOAuthProvider)(nil)

func testIdentityCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestBuildIdentityMeta_GoldenTripleBytes pins the exact `_meta` map for the
// no-agent, no-annotations case so the v1.9 wire bytes are unchanged.
func TestBuildIdentityMeta_GoldenTripleBytes(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	meta, err := buildIdentityMeta(ctx, nil)
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	got, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// json.Marshal sorts map keys — deterministic golden.
	const golden = `{"session":"s1","tenant":"t1","user":"u1"}`
	if string(got) != golden {
		t.Fatalf("golden triple bytes drifted:\n got  %s\n want %s", got, golden)
	}
}

func TestBuildIdentityMeta_MissingIdentityFailsClosed(t *testing.T) {
	if _, err := buildIdentityMeta(context.Background(), nil); !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("want ErrIdentityMissing, got %v", err)
	}
	// Partial triple also fails closed.
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	// Overwrite with an incomplete triple is not possible via identity.With
	// (it validates), so assert the empty-ctx path only above; the complete
	// ctx must succeed.
	if _, err := buildIdentityMeta(ctx, nil); err != nil {
		t.Fatalf("complete triple: %v", err)
	}
}

// TestBuildIdentityMeta_AgentAndAnnotations proves agent_id is stamped from
// ctx provenance and operator annotations are merged verbatim.
func TestBuildIdentityMeta_AgentAndAnnotations(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	ctx = tools.WithInvokingAgent(ctx, "agent-9")
	meta, err := buildIdentityMeta(ctx, map[string]string{"env": "prod", "team": "search"})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if meta["tenant"] != "t1" || meta["user"] != "u1" || meta["session"] != "s1" {
		t.Fatalf("triple drifted: %+v", meta)
	}
	if meta["agent_id"] != "agent-9" {
		t.Fatalf("agent_id = %v, want agent-9", meta["agent_id"])
	}
	if meta["env"] != "prod" || meta["team"] != "search" {
		t.Fatalf("annotations not merged: %+v", meta)
	}
}

// TestBuildIdentityMeta_ReservedAnnotationCannotShadow proves a reserved-key
// annotation that slipped past validation can never shadow the triple / agent
// (the merge-time invariant).
func TestBuildIdentityMeta_ReservedAnnotationCannotShadow(t *testing.T) {
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	ctx = tools.WithInvokingAgent(ctx, "real-agent")
	meta, err := buildIdentityMeta(ctx, map[string]string{
		"tenant":                            "EVIL",
		"agent_id":                          "EVIL",
		"io.modelcontextprotocol/something": "EVIL",
		"ok":                                "fine",
	})
	if err != nil {
		t.Fatalf("buildIdentityMeta: %v", err)
	}
	if meta["tenant"] != "t1" {
		t.Fatalf("reserved annotation shadowed tenant: %v", meta["tenant"])
	}
	if meta["agent_id"] != "real-agent" {
		t.Fatalf("reserved annotation shadowed agent_id: %v", meta["agent_id"])
	}
	if _, present := meta["io.modelcontextprotocol/something"]; present {
		t.Fatalf("spec-prefixed annotation leaked into _meta")
	}
	if meta["ok"] != "fine" {
		t.Fatalf("non-reserved annotation dropped")
	}
}

// captureRoundTripper records the last request it saw and returns a canned
// 200 without a real network call.
type captureRoundTripper struct {
	lastAuth      string
	sawAuthorized bool
	staticHeader  string
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.lastAuth = req.Header.Get("Authorization")
	c.staticHeader = req.Header.Get("X-Static")
	if c.lastAuth != "" {
		c.sawAuthorized = true
	}
	return &http.Response{
		StatusCode: 200,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestBearerInjectingTransport_CtxTokenToHeader(t *testing.T) {
	cap := &captureRoundTripper{}
	bt := &bearerInjectingTransport{base: cap}
	req, _ := http.NewRequestWithContext(withBearer(context.Background(), "tok-123"), http.MethodPost, "http://example.test", nil)
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if cap.lastAuth != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want %q", cap.lastAuth, "Bearer tok-123")
	}
}

func TestBearerInjectingTransport_NoTokenPassThrough(t *testing.T) {
	cap := &captureRoundTripper{}
	bt := &bearerInjectingTransport{base: cap}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test", nil)
	resp, err := bt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	if cap.sawAuthorized {
		t.Fatalf("Authorization set on a no-token request: %q", cap.lastAuth)
	}
}

// TestBuildHTTPClient_BearerWinsOverStaticAuthorization proves the layering
// order: the per-identity bearer is set LAST (innermost) so a stray static
// Authorization can never shadow it.
func TestBuildHTTPClient_BearerWinsOverStaticAuthorization(t *testing.T) {
	cap := &captureRoundTripper{}
	// Build the layered transport the way buildHTTPClient does, but inject our
	// capture base by constructing the wrappers directly.
	inner := &bearerInjectingTransport{base: cap}
	outer := &headerInjectingTransport{
		base:    inner,
		headers: map[string]string{"Authorization": "Bearer STATIC-should-lose", "X-Static": "yes"},
	}
	client := &http.Client{Transport: outer}
	req, _ := http.NewRequestWithContext(withBearer(context.Background(), "identity-tok"), http.MethodPost, "http://example.test", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	_ = resp.Body.Close()
	if cap.lastAuth != "Bearer identity-tok" {
		t.Fatalf("static Authorization shadowed the per-identity bearer: got %q", cap.lastAuth)
	}
	if cap.staticHeader != "yes" {
		t.Fatalf("static non-auth header dropped: %q", cap.staticHeader)
	}
}

func TestResolveBearerCtx_UnboundIsNoOp(t *testing.T) {
	p := &Provider{cfg: Config{}}
	ctx := context.Background()
	got, err := p.resolveBearerCtx(ctx, "")
	if err != nil {
		t.Fatalf("resolveBearerCtx: %v", err)
	}
	if bearerFrom(got) != "" {
		t.Fatalf("unbound connection stashed a bearer")
	}
}

func TestResolveBearerCtx_FailClosedOnTokenError(t *testing.T) {
	authErr := &auth.ErrAuthRequired{Source: "m365", Message: "consent required"}
	p := &Provider{cfg: Config{OAuthProvider: &stubOAuthProvider{tokErr: authErr}}}
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	_, err := p.resolveBearerCtx(ctx, "")
	if err == nil {
		t.Fatal("want error on Token failure, got nil")
	}
	var typed *auth.ErrAuthRequired
	if !errors.As(err, &typed) {
		t.Fatalf("want errors.As *auth.ErrAuthRequired, got %v", err)
	}
}

// TestResolveBearerCtx_FailClosedOnEmptyToken — defence-in-depth: a bound
// provider returning ("", nil) must abort the RPC (ErrEmptyBearer) rather
// than proceed unauthenticated on a per-identity-authorized connection.
func TestResolveBearerCtx_FailClosedOnEmptyToken(t *testing.T) {
	p := &Provider{cfg: Config{Name: "graph", OAuthProvider: &stubOAuthProvider{token: ""}}, source: "graph"}
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	_, err := p.resolveBearerCtx(ctx, "")
	if err == nil {
		t.Fatal("want error on empty bearer, got nil (an unauthenticated call would leak)")
	}
	if !errors.Is(err, ErrEmptyBearer) {
		t.Fatalf("want ErrEmptyBearer, got %v", err)
	}
	if !contains(err.Error(), "graph") {
		t.Fatalf("error must name the connection/source, got %q", err.Error())
	}
}

func TestResolveBearerCtx_StashesBearer(t *testing.T) {
	p := &Provider{cfg: Config{OAuthProvider: &stubOAuthProvider{token: "brokered-xyz"}}, source: "m365"}
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	got, err := p.resolveBearerCtx(ctx, "")
	if err != nil {
		t.Fatalf("resolveBearerCtx: %v", err)
	}
	if bearerFrom(got) != "brokered-xyz" {
		t.Fatalf("bearer not stashed: %q", bearerFrom(got))
	}
}

// TestResolveOAuthBinding_Table covers the attach-time binding validation.
func TestResolveOAuthBinding_Table(t *testing.T) {
	prov := &stubOAuthProvider{token: "t", allowedHosts: []string{"mcp.example.test"}}
	providers := map[string]auth.OAuthProvider{"m365": prov}

	t.Run("unbound", func(t *testing.T) {
		got, err := resolveOAuthBinding(config.MCPServerConfig{Name: "x"}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err != nil || got != nil {
			t.Fatalf("unbound: got (%v, %v)", got, err)
		}
	})
	t.Run("resolves", func(t *testing.T) {
		got, err := resolveOAuthBinding(config.MCPServerConfig{Name: "x", URL: "https://mcp.example.test", OAuthProvider: "m365"}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err != nil || got == nil {
			t.Fatalf("resolves: got (%v, %v)", got, err)
		}
	})
	t.Run("unknown provider lists registered", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{Name: "x", URL: "https://mcp.example.test", OAuthProvider: "nope"}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding, got %v", err)
		}
		if got := err.Error(); !contains(got, "m365") {
			t.Fatalf("error must list registered providers, got %q", got)
		}
	})
	t.Run("stdio binding rejected", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{Name: "x", OAuthProvider: "m365"}, TransportStdio, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for stdio, got %v", err)
		}
	})
	t.Run("per-tool binding resolves + falls back", func(t *testing.T) {
		ms := config.MCPServerConfig{
			Name: "x", URL: "https://mcp.example.test", OAuthProvider: "m365",
			ToolOAuthProviders: map[string]string{"send_mail": "m365"},
		}
		toolProviders, err := resolveToolOAuthBindings(ms, TransportStreamableHTTP, mapProviderResolver(providers))
		if err != nil {
			t.Fatalf("resolveToolOAuthBindings: %v", err)
		}
		if toolProviders["send_mail"] == nil {
			t.Fatalf("send_mail not resolved")
		}
	})
	t.Run("per-tool binding re-enforces rules (unknown provider)", func(t *testing.T) {
		ms := config.MCPServerConfig{
			Name: "x", URL: "https://mcp.example.test",
			ToolOAuthProviders: map[string]string{"send_mail": "nope"},
		}
		_, err := resolveToolOAuthBindings(ms, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for unknown per-tool provider, got %v", err)
		}
	})
	t.Run("per-tool binding re-enforces rules (stdio)", func(t *testing.T) {
		ms := config.MCPServerConfig{
			Name: "x", Command: []string{"srv"},
			ToolOAuthProviders: map[string]string{"send_mail": "m365"},
		}
		_, err := resolveToolOAuthBindings(ms, TransportStdio, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for stdio per-tool binding, got %v", err)
		}
	})
	t.Run("auto transport without url (command-only, resolves to stdio) rejected", func(t *testing.T) {
		// The silent-degradation trap: an auto/omitted transport with only a
		// command auto-selects stdio at connect — the bearer would never
		// reach any wire. Must fail exactly like explicit stdio.
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", Command: []string{"/bin/echo"}, OAuthProvider: "m365",
		}, TransportAuto, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for auto+command-only binding, got %v", err)
		}
	})
	t.Run("static authorization conflict rejected", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", URL: "https://mcp.example.test", OAuthProvider: "m365",
			Headers: map[string]string{"AUTHORIZATION": "Bearer static"},
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for static Authorization, got %v", err)
		}
	})
	t.Run("reserved annotation key rejected", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", MetaAnnotations: map[string]string{"tenant": "x"},
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for reserved annotation, got %v", err)
		}
	})
	t.Run("spec-prefixed annotation key rejected", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", MetaAnnotations: map[string]string{"io.modelcontextprotocol/x": "y"},
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for spec-prefixed annotation, got %v", err)
		}
	})
	t.Run("empty annotation key rejected", func(t *testing.T) {
		_, err := resolveOAuthBinding(config.MCPServerConfig{
			Name: "x", MetaAnnotations: map[string]string{"  ": "y"},
		}, TransportStreamableHTTP, mapProviderResolver(providers))
		if err == nil || !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("want ErrOAuthBinding for empty annotation key, got %v", err)
		}
	})
}

// TestAttach_AutoCommandBindingFailsLoud drives the FULL Attach entry with a
// command-only, omitted-transport config binding an oauth_provider — the
// exact shape that would auto-select stdio at connect and silently skip
// bearer injection. Attach must fail loud BEFORE any connection attempt.
func TestAttach_AutoCommandBindingFailsLoud(t *testing.T) {
	prov := &stubOAuthProvider{token: "t"}
	var closers []func(context.Context) error
	err := Attach(context.Background(), config.MCPServerConfig{
		Name:          "local",
		Command:       []string{"/bin/echo"},
		OAuthProvider: "m365",
	}, AttachDeps{
		Catalog:        tools.NewCatalog(),
		Registry:       NewRegistry(),
		Closers:        &closers,
		OAuthProviders: map[string]auth.OAuthProvider{"m365": prov},
	})
	if err == nil {
		t.Fatal("Attach accepted an oauth_provider binding on an auto+command-only (stdio-resolving) connection")
	}
	if !errors.Is(err, ErrOAuthBinding) {
		t.Fatalf("want ErrOAuthBinding, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
