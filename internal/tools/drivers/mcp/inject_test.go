package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// identityCredProvider is an auth.OAuthProvider whose Token derives the returned
// credential from the ctx identity — the acting principal — so the per-user
// isolation guarantee can be exercised. When ignoreIdentity is set it returns a
// constant credential regardless of ctx; the mutation-verification test flips
// this to prove the isolation assertion actually bites.
type identityCredProvider struct {
	ignoreIdentity bool
	tokErr         error
	allowedHosts   []string
}

func (p *identityCredProvider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if p.tokErr != nil {
		return auth.Token{}, p.tokErr
	}
	if p.ignoreIdentity {
		return auth.Token{AccessToken: "cred-CONSTANT", BindingScope: auth.ScopeUser}, nil
	}
	id, ok := identity.From(ctx)
	if !ok {
		return auth.Token{}, errors.New("no identity in ctx")
	}
	return auth.Token{AccessToken: "cred-" + id.TenantID + "-" + id.UserID, BindingScope: auth.ScopeUser}, nil
}
func (p *identityCredProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, errors.New("not implemented")
}
func (p *identityCredProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, errors.New("not implemented")
}
func (p *identityCredProvider) PendingFlow(context.Context, string) (auth.PendingFlowInfo, bool, error) {
	return auth.PendingFlowInfo{}, false, nil
}
func (p *identityCredProvider) DenyFlow(context.Context, string, string) error   { return nil }
func (p *identityCredProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (p *identityCredProvider) Close(context.Context) error                      { return nil }
func (p *identityCredProvider) AllowedDownstreamHosts() []string {
	return append([]string(nil), p.allowedHosts...)
}

var _ auth.OAuthProvider = (*identityCredProvider)(nil)

func injectProvider(inj *CredentialInjection) *Provider {
	return &Provider{cfg: Config{Name: "recv", Injection: inj}, source: "recv"}
}

// TestResolveInjection_HeaderForm_InjectsPulledValue proves the header form
// threads the pulled per-user credential onto the ctx for the injecting
// transport.
func TestResolveInjection_HeaderForm_InjectsPulledValue(t *testing.T) {
	p := injectProvider(&CredentialInjection{
		Provider: &identityCredProvider{},
		Form:     InjectionFormHeader,
		Header:   "x-vendor-api-key",
	})
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "alice", SessionID: "s1"})
	meta := mcpsdk.Meta{}
	out, err := p.resolveInjection(ctx, meta)
	if err != nil {
		t.Fatalf("resolveInjection: %v", err)
	}
	h := injectedHeadersFrom(out)
	if h["x-vendor-api-key"] != "cred-t1-alice" {
		t.Fatalf("header value = %q, want cred-t1-alice", h["x-vendor-api-key"])
	}
	if len(meta) != 0 {
		t.Fatalf("header form must not touch _meta: %+v", meta)
	}
}

// TestResolveInjection_BasicForm_InjectsBase64 proves the basic form injects
// Authorization: Basic base64(username:credential).
func TestResolveInjection_BasicForm_InjectsBase64(t *testing.T) {
	p := injectProvider(&CredentialInjection{
		Provider:      &identityCredProvider{},
		Form:          InjectionFormBasic,
		BasicUsername: "svc",
	})
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "bob", SessionID: "s1"})
	out, err := p.resolveInjection(ctx, mcpsdk.Meta{})
	if err != nil {
		t.Fatalf("resolveInjection: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("svc:cred-t1-bob"))
	if got := injectedHeadersFrom(out)["Authorization"]; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

// TestResolveInjection_MetaForm_InjectsNestedLeaf proves the _meta form writes
// the pulled value at the declared nested key path.
func TestResolveInjection_MetaForm_InjectsNestedLeaf(t *testing.T) {
	p := injectProvider(&CredentialInjection{
		Provider: &identityCredProvider{},
		Form:     InjectionFormMeta,
		MetaKey:  []string{"vendor", "api_key"},
	})
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "carol", SessionID: "s1"})
	meta := mcpsdk.Meta{}
	if _, err := p.resolveInjection(ctx, meta); err != nil {
		t.Fatalf("resolveInjection: %v", err)
	}
	vendor, ok := meta["vendor"].(map[string]any)
	if !ok {
		t.Fatalf("meta[vendor] not a nested map: %+v", meta)
	}
	if vendor["api_key"] != "cred-t1-carol" {
		t.Fatalf("meta leaf = %v, want cred-t1-carol", vendor["api_key"])
	}
}

// TestResolveInjection_PerUserIsolation proves two acting users on ONE shared
// connection produce two DISTINCT injected values (no cross-user bleed). The
// mutation-verification subtest flips the provider to ignore identity and
// asserts the isolation check then FAILS — proving the assertion bites.
func TestResolveInjection_PerUserIsolation(t *testing.T) {
	run := func(ignoreIdentity bool) (string, string) {
		p := injectProvider(&CredentialInjection{
			Provider: &identityCredProvider{ignoreIdentity: ignoreIdentity},
			Form:     InjectionFormHeader,
			Header:   "x-vendor-api-key",
		})
		ctxA := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "alice", SessionID: "s1"})
		ctxB := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "bob", SessionID: "s1"})
		outA, errA := p.resolveInjection(ctxA, mcpsdk.Meta{})
		outB, errB := p.resolveInjection(ctxB, mcpsdk.Meta{})
		if errA != nil || errB != nil {
			t.Fatalf("resolveInjection errs: %v / %v", errA, errB)
		}
		return injectedHeadersFrom(outA)["x-vendor-api-key"], injectedHeadersFrom(outB)["x-vendor-api-key"]
	}

	a, b := run(false)
	if a != "cred-t1-alice" || b != "cred-t1-bob" {
		t.Fatalf("per-user values wrong: %q / %q", a, b)
	}
	if a == b {
		t.Fatalf("cross-user bleed: both users got %q", a)
	}

	// Mutation verification: a provider that ignores identity yields the SAME
	// value for both users — the isolation invariant must then be violated.
	ma, mb := run(true)
	if ma != mb {
		t.Fatalf("mutation-verify precondition broken: expected identical values, got %q / %q", ma, mb)
	}
}

// TestResolveInjection_BrokerError_FailsLoud proves a broker error aborts the
// call (returns the typed error, nil ctx) — no unauthenticated fallback.
func TestResolveInjection_BrokerError_FailsLoud(t *testing.T) {
	authErr := &auth.ErrAuthRequired{}
	p := injectProvider(&CredentialInjection{
		Provider: &identityCredProvider{tokErr: authErr},
		Form:     InjectionFormHeader,
		Header:   "x-vendor-api-key",
	})
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "dave", SessionID: "s1"})
	out, err := p.resolveInjection(ctx, mcpsdk.Meta{})
	if err == nil {
		t.Fatal("want error on broker failure, got nil (an unauthenticated inject would leak)")
	}
	var typed *auth.ErrAuthRequired
	if !errors.As(err, &typed) {
		t.Fatalf("want errors.As *auth.ErrAuthRequired, got %v", err)
	}
	if out != nil {
		t.Fatalf("ctx must be nil on failure, got %v", out)
	}
}

// TestResolveInjection_EmptyToken_FailsLoud proves a ("", nil) broker return
// fails closed rather than injecting an empty credential.
func TestResolveInjection_EmptyToken_FailsLoud(t *testing.T) {
	// An identity provider returning a token whose AccessToken is empty: model
	// it with ignoreIdentity + a blank via a wrapper.
	p := injectProvider(&CredentialInjection{
		Provider: &emptyTokenProvider{},
		Form:     InjectionFormHeader,
		Header:   "x-vendor-api-key",
	})
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "erin", SessionID: "s1"})
	_, err := p.resolveInjection(ctx, mcpsdk.Meta{})
	if !errors.Is(err, ErrEmptyBearer) {
		t.Fatalf("want ErrEmptyBearer, got %v", err)
	}
}

type emptyTokenProvider struct{ identityCredProvider }

func (e *emptyTokenProvider) Token(context.Context, tools.ToolSourceID) (auth.Token, error) {
	return auth.Token{AccessToken: "", BindingScope: auth.ScopeUser}, nil
}

// TestInjection_OneAuthModeGuard proves the attach-time binding resolver rejects
// an injection mapping declared alongside a bearer provider or a static
// Authorization header (one auth mode per connection).
func TestInjection_OneAuthModeGuard(t *testing.T) {
	// Static Authorization header conflict, isolated at resolveInjectionBinding.
	prov := &identityCredProvider{allowedHosts: []string{"mcp.example.test"}}
	providers := mapProviderResolver(map[string]auth.OAuthProvider{"broker": prov})
	_, err := resolveInjectionBinding(config.MCPServerConfig{
		Name:      "recv",
		URL:       "https://mcp.example.test",
		Headers:   map[string]string{"Authorization": "Basic xxx"},
		Injection: &config.MCPCredentialInjectionConfig{Provider: "broker", Form: config.MCPInjectionFormBasic},
	}, TransportStreamableHTTP, providers)
	if !errors.Is(err, ErrOAuthBinding) {
		t.Fatalf("static Authorization + injection: want ErrOAuthBinding, got %v", err)
	}
	// Bearer + injection conflict.
	_, err = resolveInjectionBinding(config.MCPServerConfig{
		Name:          "recv",
		URL:           "https://mcp.example.test",
		OAuthProvider: "broker",
		Injection:     &config.MCPCredentialInjectionConfig{Provider: "broker", Form: config.MCPInjectionFormBasic},
	}, TransportStreamableHTTP, providers)
	if !errors.Is(err, ErrOAuthBinding) {
		t.Fatalf("bearer + injection: want ErrOAuthBinding, got %v", err)
	}
}

func TestSignedPrivateProviderOverride_ResolvesReceiverInjection(t *testing.T) {
	prov := &identityCredProvider{allowedHosts: []string{"bamboo.example.test"}}
	server := config.MCPServerConfig{
		Name: "bamboo", URL: "https://bamboo.example.test/t/cleartech/mcp",
		Injection: &config.MCPCredentialInjectionConfig{
			Provider: "capability-bamboo", Form: config.MCPInjectionFormHeader, Header: "x-bamboohr-api-key",
		},
	}
	name := privateOverrideProviderName(server)
	resolver := overrideProviderResolver{base: mapProviderResolver(nil), name: name, provider: prov}
	got, err := resolveInjectionBinding(server, TransportStreamableHTTP, resolver)
	if err != nil {
		t.Fatalf("resolve signed private injection provider: %v", err)
	}
	if name != "capability-bamboo" || got == nil || got.Provider != prov || got.Form != InjectionFormHeader || got.Header != "x-bamboohr-api-key" {
		t.Fatalf("signed private injection binding = name %q, binding %+v", name, got)
	}

	server.URL = "https://other.example.test/mcp"
	if _, err := resolveInjectionBinding(server, TransportStreamableHTTP, resolver); !errors.Is(err, ErrOAuthBinding) {
		t.Fatalf("private provider sink widening error = %v, want ErrOAuthBinding", err)
	}
}

// TestResolveInjectionBinding_MetaReservedKey_Rejected proves an injection
// mapping targeting a reserved _meta key is rejected at attach time (fail-loud).
func TestResolveInjectionBinding_MetaReservedKey_Rejected(t *testing.T) {
	prov := &identityCredProvider{allowedHosts: []string{"mcp.example.test"}}
	providers := mapProviderResolver(map[string]auth.OAuthProvider{"broker": prov})
	for _, reserved := range []string{"tenant", "agent_id", "traceparent"} {
		_, err := resolveInjectionBinding(config.MCPServerConfig{
			Name:      "recv",
			URL:       "https://mcp.example.test",
			Injection: &config.MCPCredentialInjectionConfig{Provider: "broker", Form: config.MCPInjectionFormMeta, MetaKey: reserved + ".api_key"},
		}, TransportStreamableHTTP, providers)
		if !errors.Is(err, ErrOAuthBinding) {
			t.Fatalf("reserved segment %q: want ErrOAuthBinding, got %v", reserved, err)
		}
	}
	// A non-covered leaf is also rejected (redaction guarantee).
	_, err := resolveInjectionBinding(config.MCPServerConfig{
		Name:      "recv",
		URL:       "https://mcp.example.test",
		Injection: &config.MCPCredentialInjectionConfig{Provider: "broker", Form: config.MCPInjectionFormMeta, MetaKey: "vendor.blob"},
	}, TransportStreamableHTTP, providers)
	if !errors.Is(err, ErrOAuthBinding) {
		t.Fatalf("non-covered leaf: want ErrOAuthBinding, got %v", err)
	}
}

// TestInjectedHeaderTransport_ConcurrentReuse_NoBleed drives N≥100 interleaved
// per-user injection calls through ONE shared Provider + ONE shared injecting
// transport under -race, asserting each identity's request carried its OWN
// injected credential (no cross-user bleed) and no goroutine leak after.
func TestInjectedHeaderTransport_ConcurrentReuse_NoBleed(t *testing.T) {
	base := runtime.NumGoroutine()
	shared := injectProvider(&CredentialInjection{
		Provider: &identityCredProvider{},
		Form:     InjectionFormHeader,
		Header:   "x-vendor-api-key",
	})
	cap := &capturingRoundTripper{}
	transport := &injectedHeaderTransport{base: cap}

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			user := fmt.Sprintf("u%d", i%7)
			ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: user, SessionID: "s1"})
			injCtx, err := shared.resolveInjection(ctx, mcpsdk.Meta{})
			if err != nil {
				t.Errorf("resolveInjection: %v", err)
				return
			}
			req := httptest.NewRequest(http.MethodPost, "https://mcp.example.test/rpc", nil).WithContext(injCtx)
			// Carry the REQUESTING user on a non-injected marker header so the
			// capturing base can detect a bleed (an injected value belonging to a
			// DIFFERENT user than the marker).
			req.Header.Set("x-test-user", user)
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Errorf("RoundTrip: %v", err)
				return
			}
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	if bleeds := cap.bleeds(); len(bleeds) > 0 {
		t.Fatalf("cross-user credential bleed detected: %v", bleeds)
	}
	if cap.count() != n {
		t.Fatalf("observed %d requests, want %d", cap.count(), n)
	}

	// Bounded drain-poll (§17.4): the per-request goroutines can still be
	// unwinding at this instant, so re-sample after GC until the count settles
	// within tolerance or a deadline elapses — a genuine leak still fails.
	settleDeadline := time.Now().Add(5 * time.Second)
	var got int
	for {
		runtime.GC()
		got = runtime.NumGoroutine()
		if got <= base+5 || !time.Now().Before(settleDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got > base+5 {
		t.Errorf("goroutine leak: base=%d after=%d", base, got)
	}
}

// capturingRoundTripper compares the injected credential header against the
// requesting user (carried on the non-injected x-test-user marker), recording a
// bleed whenever an injected value belongs to a DIFFERENT user than the marker.
type capturingRoundTripper struct {
	mu     sync.Mutex
	total  int
	bleedL []string
}

func (c *capturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	marker := req.Header.Get("x-test-user")
	val := req.Header.Get("x-vendor-api-key")
	want := "cred-t1-" + marker
	c.mu.Lock()
	c.total++
	if val != want {
		c.bleedL = append(c.bleedL, fmt.Sprintf("user %s got %q want %q", marker, val, want))
	}
	c.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

func (c *capturingRoundTripper) bleeds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.bleedL...)
}

func (c *capturingRoundTripper) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
