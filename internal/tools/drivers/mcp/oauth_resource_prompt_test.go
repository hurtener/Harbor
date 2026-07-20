package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// oauth_resource_prompt_test.go — the per-entry OAuth binding extended to the
// resource/prompt RPC paths. The per-tool `oauth_provider` binding HA-27b shipped
// for `callTool` now also resolves on `ReadResource`, `SubscribeResource`, the
// resource-read descriptor invoke, and the prompt-get descriptor invoke, keyed
// by the resource URI / prompt name (connection-level fallback when unbound).
//
// The fixture is the OFFICIAL go-sdk MCP server driven over the real streamable
// HTTP transport (§17.8: the conformance fixture derives from the official
// package's types + wire, never a hand-authored JSON-RPC shape).

// respPromptServer builds an httptest server hosting a go-sdk MCP server with
// two resources (`mem://bound`, `mem://unbound`), two prompts (`bound_prompt`,
// `unbound_prompt`), and subscribe support, fronted by the supplied inspector.
func respPromptServer(t *testing.T, front func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-respr-fixture", Version: "v0"}, &mcpsdk.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcpsdk.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcpsdk.UnsubscribeRequest) error { return nil },
	})
	for _, uri := range []string{"mem://bound", "mem://unbound"} {
		u := uri
		srv.AddResource(
			&mcpsdk.Resource{URI: u, Name: u, MIMEType: "text/plain"},
			func(_ context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
				return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
					{URI: req.Params.URI, MIMEType: "text/plain", Text: "content of " + req.Params.URI},
				}}, nil
			},
		)
	}
	for _, name := range []string{"bound_prompt", "unbound_prompt"} {
		srv.AddPrompt(
			&mcpsdk.Prompt{Name: name}, // no required args — invocable with {}
			func(_ context.Context, req *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
				return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{
					{Role: "assistant", Content: &mcpsdk.TextContent{Text: "prompt " + req.Params.Name}},
				}}, nil
			},
		)
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	hs := httptest.NewServer(front(handler))
	t.Cleanup(hs.Close)
	return hs
}

// rpcAddressing extracts the JSON-RPC method plus the addressing key an MCP
// request carries: the resource URI for resources/read + resources/subscribe,
// the name for prompts/get + tools/call. It also returns the `_meta` triple.
func rpcAddressing(body []byte) (method, key string, meta map[string]string) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string            `json:"name"`
			URI  string            `json:"uri"`
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", "", nil
	}
	switch msg.Method {
	case "resources/read", "resources/subscribe":
		return msg.Method, msg.Params.URI, msg.Params.Meta
	case "prompts/get", "tools/call":
		return msg.Method, msg.Params.Name, msg.Params.Meta
	default:
		return msg.Method, "", msg.Params.Meta
	}
}

// captureBearerFront records the Authorization header seen per `method|key`.
type captureBearerFront struct {
	inner http.Handler
	mu    sync.Mutex
	seen  map[string]string // "method|key" -> Authorization
}

func newCaptureBearerFront(inner http.Handler) *captureBearerFront {
	return &captureBearerFront{inner: inner, seen: map[string]string{}}
}

func (c *captureBearerFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			if method, key, _ := rpcAddressing(body); method != "" && key != "" {
				c.mu.Lock()
				c.seen[method+"|"+key] = r.Header.Get("Authorization")
				c.mu.Unlock()
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ContentLength = int64(len(body))
		}
	}
	c.inner.ServeHTTP(w, r)
}

func (c *captureBearerFront) bearer(method, key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen[method+"|"+key]
}

// TestResolveBearerCtx_ResourcePromptKey is the unit-level AC-1 gate: the
// per-entry map resolves by resource URI / prompt name, with connection-level
// fallback for an unbound key.
func TestResolveBearerCtx_ResourcePromptKey(t *testing.T) {
	connProv := &stubOAuthProvider{token: "CONN", allowedHosts: []string{"h"}}
	resProv := &stubOAuthProvider{token: "RES", allowedHosts: []string{"h"}}
	prProv := &stubOAuthProvider{token: "PROMPT", allowedHosts: []string{"h"}}
	p := &Provider{cfg: Config{
		Name:          "respr",
		OAuthProvider: connProv,
		ToolOAuthProviders: map[string]auth.OAuthProvider{
			"mem://bound":  resProv,
			"bound_prompt": prProv,
		},
	}, source: "respr"}
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})

	cases := []struct {
		key  string
		want string
	}{
		{"mem://bound", "RES"},     // bound resource URI
		{"bound_prompt", "PROMPT"}, // bound prompt name
		{"mem://unbound", "CONN"},  // unbound resource → connection fallback
		{"unbound_prompt", "CONN"}, // unbound prompt → connection fallback
	}
	for _, tc := range cases {
		got, err := p.resolveBearerCtx(ctx, tc.key)
		if err != nil {
			t.Fatalf("resolveBearerCtx(%q): %v", tc.key, err)
		}
		if bearerFrom(got) != tc.want {
			t.Fatalf("key %q resolved bearer %q, want %q", tc.key, bearerFrom(got), tc.want)
		}
	}
}

// TestProvider_ResourcePromptBinding_RoundTrip drives the real go-sdk wire and
// asserts the outbound bearer on EACH of the four extended sites: ReadResource
// (method), SubscribeResource, the resource-read descriptor invoke, and the
// prompt-get descriptor invoke — bound entries resolve their provider, unbound
// entries fall back to the connection binding, with identity propagation.
func TestProvider_ResourcePromptBinding_RoundTrip(t *testing.T) {
	var cap *captureBearerFront
	hs := respPromptServer(t, func(inner http.Handler) http.Handler {
		cap = newCaptureBearerFront(inner)
		return cap
	})
	host := strings.TrimPrefix(hs.URL, "http://")
	bus := mkScopeTestBus(t)

	connProv := prefixBearerProvider{tag: "CONN", hosts: []string{host}}
	resProv := prefixBearerProvider{tag: "RES", hosts: []string{host}}
	prProv := prefixBearerProvider{tag: "PROMPT", hosts: []string{host}}

	p, err := New(Config{
		Name:            "respr",
		TransportMode:   TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   connProv,
		ToolOAuthProviders: map[string]auth.OAuthProvider{
			"mem://bound":  resProv,
			"bound_prompt": prProv,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx := testIdentityCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})

	boundRes := discoverTool(t, p, "respr__resource.mem://bound")
	unboundRes := discoverTool(t, p, "respr__resource.mem://unbound")
	boundPrompt := discoverTool(t, p, "respr__prompt.bound_prompt")
	unboundPrompt := discoverTool(t, p, "respr__prompt.unbound_prompt")

	if _, err := boundRes.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("bound resource invoke: %v", err)
	}
	if _, err := unboundRes.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unbound resource invoke: %v", err)
	}
	if _, err := boundPrompt.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("bound prompt invoke: %v", err)
	}
	if _, err := unboundPrompt.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unbound prompt invoke: %v", err)
	}
	// ReadResource method site.
	if _, _, err := p.ReadResource(ctx, "mem://bound"); err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	// SubscribeResource site.
	if err := p.SubscribeResource(ctx, "mem://bound"); err != nil {
		t.Fatalf("SubscribeResource: %v", err)
	}

	// Assert the outbound bearer per site. Bound entries carry their provider
	// tag + the request identity; unbound entries fall back to CONN.
	checks := []struct {
		site, method, key, want string
	}{
		{"resource-read descriptor (bound)", "resources/read", "mem://bound", "Bearer RES-t1-u1"},
		{"resource-read descriptor (unbound fallback)", "resources/read", "mem://unbound", "Bearer CONN-t1-u1"},
		{"prompt-get descriptor (bound)", "prompts/get", "bound_prompt", "Bearer PROMPT-t1-u1"},
		{"prompt-get descriptor (unbound fallback)", "prompts/get", "unbound_prompt", "Bearer CONN-t1-u1"},
		{"SubscribeResource (bound)", "resources/subscribe", "mem://bound", "Bearer RES-t1-u1"},
	}
	for _, c := range checks {
		if got := cap.bearer(c.method, c.key); got != c.want {
			t.Fatalf("%s: bearer on %s|%s = %q, want %q", c.site, c.method, c.key, got, c.want)
		}
	}
}

// TestDiscover_AmbiguousOAuthBinding_FailsLoud is the AC-3 gate: a per-entry key
// that addresses more than one MCP surface (a tool AND a prompt of the same
// name) is rejected at discovery rather than silently resolved by precedence.
func TestDiscover_AmbiguousOAuthBinding_FailsLoud(t *testing.T) {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-ambig-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(srv, &mcpsdk.Tool{
		Name: "dup", Description: "dup tool",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	})
	srv.AddPrompt(&mcpsdk.Prompt{Name: "dup"}, func(_ context.Context, _ *mcpsdk.GetPromptRequest) (*mcpsdk.GetPromptResult, error) {
		return &mcpsdk.GetPromptResult{Messages: []*mcpsdk.PromptMessage{{Role: "assistant", Content: &mcpsdk.TextContent{Text: "p"}}}}, nil
	})
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	hs := httptest.NewServer(handler)
	t.Cleanup(hs.Close)
	host := strings.TrimPrefix(hs.URL, "http://")

	p, err := New(Config{
		Name:            "ambig",
		TransportMode:   TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             mkScopeTestBus(t),
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		ToolOAuthProviders: map[string]auth.OAuthProvider{
			"dup": prefixBearerProvider{tag: "X", hosts: []string{host}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = p.Discover(context.Background())
	if err == nil || !errors.Is(err, ErrAmbiguousOAuthBinding) {
		t.Fatalf("Discover with a tool+prompt name collision must fail with ErrAmbiguousOAuthBinding, got %v", err)
	}
}

// perPathBearerAsserter fronts an MCP server and, per resources/read +
// prompts/get, asserts the Authorization bearer matches the EXPECTED per-key
// provider tag AND the `_meta` triple — a token bleed across RPCs, keys, or
// identities is the failure.
type perPathBearerAsserter struct {
	inner      http.Handler
	keyTag     map[string]string // addressing key -> expected provider tag
	mismatches atomic.Int64
	callsSeen  atomic.Int64
	sampleMu   sync.Mutex
	sample     string
}

func (a *perPathBearerAsserter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			a.inspect(r.Header.Get("Authorization"), body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ContentLength = int64(len(body))
		}
	}
	a.inner.ServeHTTP(w, r)
}

func (a *perPathBearerAsserter) inspect(authz string, body []byte) {
	method, key, meta := rpcAddressing(body)
	if method != "resources/read" && method != "prompts/get" {
		return
	}
	tag, ok := a.keyTag[key]
	if !ok {
		return
	}
	a.callsSeen.Add(1)
	want := "Bearer " + tag + "-" + meta["tenant"] + "-" + meta["user"]
	if authz != want {
		a.mismatches.Add(1)
		a.sampleMu.Lock()
		if a.sample == "" {
			a.sample = fmt.Sprintf("method=%s key=%s meta(%s,%s) got %q want %q", method, key, meta["tenant"], meta["user"], authz, want)
		}
		a.sampleMu.Unlock()
	}
}

// TestConcurrentReuse_ResourcePromptOAuthProviders_NoTokenBleed extends the
// 191 per-tool no-bleed gate to the resource/prompt paths: N>=128 concurrent
// calls through ONE shared Provider, split across bound-resource,
// unbound-resource (connection fallback), bound-prompt, and unbound-prompt
// (connection fallback), each with DISTINCT identities. A bleed across RPC,
// key, or identity is impossible to pass. Runs under -race with a goroutine
// leak baseline.
func TestConcurrentReuse_ResourcePromptOAuthProviders_NoTokenBleed(t *testing.T) {
	var asserter *perPathBearerAsserter
	hs := respPromptServer(t, func(inner http.Handler) http.Handler {
		asserter = &perPathBearerAsserter{
			inner: inner,
			keyTag: map[string]string{
				"mem://bound":    "RES",
				"mem://unbound":  "CONN",
				"bound_prompt":   "PROMPT",
				"unbound_prompt": "CONN",
			},
		}
		return asserter
	})
	host := strings.TrimPrefix(hs.URL, "http://")
	bus := mkScopeTestBus(t)

	p, err := New(Config{
		Name:            "respr",
		TransportMode:   TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   prefixBearerProvider{tag: "CONN", hosts: []string{host}},
		ToolOAuthProviders: map[string]auth.OAuthProvider{
			"mem://bound":  prefixBearerProvider{tag: "RES", hosts: []string{host}},
			"bound_prompt": prefixBearerProvider{tag: "PROMPT", hosts: []string{host}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	boundRes := discoverTool(t, p, "respr__resource.mem://bound")
	unboundRes := discoverTool(t, p, "respr__resource.mem://unbound")
	boundPrompt := discoverTool(t, p, "respr__prompt.bound_prompt")
	unboundPrompt := discoverTool(t, p, "respr__prompt.unbound_prompt")
	targets := []tools.ToolDescriptor{boundRes, unboundRes, boundPrompt, unboundPrompt}

	baseline := runtime.NumGoroutine()
	const n = 128
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("sess-%d", i),
			}
			ctx, iErr := identity.With(context.Background(), id)
			if iErr != nil {
				errs[i] = iErr
				return
			}
			_, errs[i] = targets[i%len(targets)].Invoke(ctx, json.RawMessage(`{}`))
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d Invoke failed: %v", i, e)
		}
	}
	if got := asserter.callsSeen.Load(); got < n {
		t.Fatalf("server saw %d resource/prompt calls, want >= %d", got, n)
	}
	if got := asserter.mismatches.Load(); got != 0 {
		t.Fatalf("TOKEN BLEED: %d mismatches; sample: %s", got, asserter.sample)
	}
	_ = p.Close(context.Background())
	settleGoroutines(t, baseline)
}
