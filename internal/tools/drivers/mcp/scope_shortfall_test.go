package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// wwwAuthenticateInsufficientScope loads the spec-derived RFC 6750 §3.1
// `WWW-Authenticate` challenge header value from the committed fixture — a
// wrong-field mutation of the fixture must fail the capture (§17.8).
func wwwAuthenticateInsufficientScope(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../auth/testdata/oauthdiscovery/www_authenticate_insufficient_scope.txt")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	return strings.TrimSpace(strings.TrimPrefix(line, "WWW-Authenticate:"))
}

// scope403Server fronts a real go-sdk MCP server: the handshake + tools/list
// pass through, but a tools/call for `shortfallTool` answers `403` +
// `WWW-Authenticate: insufficient_scope`, and a tools/call for `plain403Tool`
// answers a bare `403` (no challenge). Every other request forwards to the SDK
// server. This is the httptest MCP-shaped fixture the driver's shortfall
// capture is proven against.
type scope403Server struct {
	inner         http.Handler
	challenge     string
	shortfallTool string
	plain403Tool  string
}

func (s *scope403Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			if tool := callToolName(body); tool != "" {
				switch tool {
				case s.shortfallTool:
					w.Header().Set("WWW-Authenticate", s.challenge)
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":"insufficient_scope"}`))
					return
				case s.plain403Tool:
					// A bare 403 with no challenge — must stay opaque.
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`forbidden`))
					return
				}
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			r.ContentLength = int64(len(body))
		}
	}
	s.inner.ServeHTTP(w, r)
}

// callToolName returns the tools/call target tool name from a JSON-RPC body,
// or "" when the body is not a tools/call.
func callToolName(body []byte) string {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return ""
	}
	if msg.Method != "tools/call" {
		return ""
	}
	return msg.Params.Name
}

// newScope403Server builds an httptest server hosting a go-sdk MCP server with
// two tools (`needs_scope`, `plain_forbidden`), fronted by the 403 injector.
func newScope403Server(t *testing.T, challenge string) *httptest.Server {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-scope-fixture", Version: "v0"}, nil)
	okHandler := func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	}
	for _, name := range []string{"needs_scope", "plain_forbidden"} {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{
			Name:        name,
			Description: name,
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		}, okHandler)
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	front := &scope403Server{
		inner:         handler,
		challenge:     challenge,
		shortfallTool: "needs_scope",
		plain403Tool:  "plain_forbidden",
	}
	hs := httptest.NewServer(front)
	t.Cleanup(hs.Close)
	return hs
}

func mkScopeTestBus(t *testing.T) events.EventBus {
	t.Helper()
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func discoverTool(t *testing.T, p *Provider, harborName string) tools.ToolDescriptor {
	t.Helper()
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range descs {
		if d.Tool.Name == harborName {
			return d
		}
	}
	t.Fatalf("tool %q not discovered among %d descriptors", harborName, len(descs))
	return tools.ToolDescriptor{}
}

// TestCallTool_InsufficientScope_TypedError proves a `403` +
// `WWW-Authenticate: insufficient_scope` on a CallTool RPC becomes a typed
// *tools.ErrInsufficientScope with every field populated, while a bare `403`
// stays an opaque transport error.
func TestCallTool_InsufficientScope_TypedError(t *testing.T) {
	challenge := wwwAuthenticateInsufficientScope(t)
	hs := newScope403Server(t, challenge)
	bus := mkScopeTestBus(t)

	var recorded atomic.Pointer[ScopeShortfall]
	p, err := New(Config{
		Name:            "scopemcp",
		TransportMode:   TransportStreamableHTTP,
		URL:             hs.URL,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OnScopeShortfall: func(sf ScopeShortfall) {
			captured := sf
			recorded.Store(&captured)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// Discover BOTH tools up front — a 403 closes the streamable session, so
	// discovery must precede the first shortfall call.
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byName := map[string]tools.ToolDescriptor{}
	for _, d := range descs {
		byName[d.Tool.Name] = d
	}

	// The insufficient_scope tool → typed structured error.
	needsScope := byName["scopemcp_needs_scope"]
	if needsScope.Invoke == nil {
		t.Fatalf("needs_scope not discovered")
	}
	_, callErr := needsScope.Invoke(ctx, json.RawMessage(`{}`))
	var scopeErr *tools.ErrInsufficientScope
	if callErr == nil || !asErrInsufficientScope(callErr, &scopeErr) {
		t.Fatalf("Invoke err = %v, want *tools.ErrInsufficientScope", callErr)
	}
	if scopeErr.ToolName != "needs_scope" {
		t.Errorf("ToolName = %q, want needs_scope", scopeErr.ToolName)
	}
	if len(scopeErr.RequiredScopes) != 2 || scopeErr.RequiredScopes[0] != "read:calendar" {
		t.Errorf("RequiredScopes = %v, want [read:calendar write:calendar]", scopeErr.RequiredScopes)
	}
	if scopeErr.WWWAuthenticate != challenge {
		t.Errorf("WWWAuthenticate = %q, want verbatim %q", scopeErr.WWWAuthenticate, challenge)
	}
	if !strings.HasPrefix(scopeErr.Origin, "http") || scopeErr.DownstreamResource == "" {
		t.Errorf("origin/resource not populated: %+v", scopeErr)
	}
	// The registry-side capture fired too (parallel async record).
	if rec := recorded.Load(); rec == nil {
		t.Errorf("OnScopeShortfall never fired")
	} else if rec.ToolName != "needs_scope" {
		t.Errorf("recorded ToolName = %q", rec.ToolName)
	}

	// The bare-403 tool → opaque transport error, NOT a structured shortfall.
	// A fresh provider (the prior 403 closed the first session).
	p2, err := New(Config{
		Name:             "scopemcp2",
		TransportMode:    TransportStreamableHTTP,
		URL:              hs.URL,
		Bus:              bus,
		DefaultIdentity:  identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OnScopeShortfall: func(ScopeShortfall) {},
	})
	if err != nil {
		t.Fatalf("New p2: %v", err)
	}
	t.Cleanup(func() { _ = p2.Close(context.Background()) })
	if err := p2.Connect(context.Background()); err != nil {
		t.Fatalf("Connect p2: %v", err)
	}
	plain := discoverTool(t, p2, "scopemcp2_plain_forbidden")
	_, plainErr := plain.Invoke(ctx, json.RawMessage(`{}`))
	if plainErr == nil {
		t.Fatalf("bare 403 unexpectedly succeeded")
	}
	var stray *tools.ErrInsufficientScope
	if asErrInsufficientScope(plainErr, &stray) {
		t.Fatalf("bare 403 produced a structured shortfall (false positive): %v", plainErr)
	}
}

// asErrInsufficientScope wraps errors.As for the test's target type.
func asErrInsufficientScope(err error, target **tools.ErrInsufficientScope) bool {
	return errors.As(err, target)
}

// TestRegistry_LastScopeShortfall_Surfaced proves the connection view's
// GetServer detail read carries the last observed shortfall after an
// insufficient-scope call, even for a reader that never made the call.
func TestRegistry_LastScopeShortfall_Surfaced(t *testing.T) {
	challenge := wwwAuthenticateInsufficientScope(t)
	hs := newScope403Server(t, challenge)
	bus := mkScopeTestBus(t)
	reg := NewRegistry()

	p, err := New(Config{
		Name:             "scopemcp",
		TransportMode:    TransportStreamableHTTP,
		URL:              hs.URL,
		Bus:              bus,
		DefaultIdentity:  identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OnScopeShortfall: func(sf ScopeShortfall) { reg.RecordScopeShortfall("scopemcp", sf) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := reg.Register(context.Background(), ServerRegistration{Provider: p, Transport: "streamable_http", URLOrCommand: hs.URL, InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	needsScope := discoverTool(t, p, "scopemcp_needs_scope")
	_, _ = needsScope.Invoke(ctx, json.RawMessage(`{}`))

	// A DIFFERENT reader identity sees the recorded shortfall on the detail read.
	readerCtx, _ := identity.With(context.Background(), identity.Identity{TenantID: "admin", UserID: "op", SessionID: "sx"})
	view, err := reg.GetServer(readerCtx, "scopemcp")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if view.LastScopeShortfall == nil {
		t.Fatalf("LastScopeShortfall nil on detail read")
	}
	if view.LastScopeShortfall.ToolName != "needs_scope" {
		t.Errorf("ToolName = %q", view.LastScopeShortfall.ToolName)
	}
	if len(view.LastScopeShortfall.RequiredScopes) != 2 {
		t.Errorf("RequiredScopes = %v", view.LastScopeShortfall.RequiredScopes)
	}
}

// prefixBearerProvider mints a per-identity token prefixed with a provider tag
// so a per-tool binding test can assert which provider authenticated each
// tools/call.
type prefixBearerProvider struct {
	tag   string
	hosts []string
}

func (p prefixBearerProvider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return auth.Token{}, auth.ErrIdentityRequired
	}
	return auth.Token{AccessToken: p.tag + "-" + id.TenantID + "-" + id.UserID, BindingScope: auth.ScopeUser}, nil
}
func (prefixBearerProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}
func (prefixBearerProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, nil
}
func (prefixBearerProvider) PendingFlow(string) (auth.PendingFlowInfo, bool) {
	return auth.PendingFlowInfo{}, false
}
func (prefixBearerProvider) DenyFlow(context.Context, string, string) error   { return nil }
func (prefixBearerProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (prefixBearerProvider) Close(context.Context) error                      { return nil }
func (p prefixBearerProvider) AllowedDownstreamHosts() []string               { return p.hosts }

// perToolBearerAsserter fronts an MCP server and, per tools/call, asserts the
// Authorization bearer matches the EXPECTED per-tool provider tag AND the
// `_meta` triple — a token bleed across tools OR identities is the failure.
type perToolBearerAsserter struct {
	inner      http.Handler
	toolTag    map[string]string // server tool name → expected provider tag
	mismatches atomic.Int64
	callsSeen  atomic.Int64
	sampleMu   sync.Mutex
	sample     string
}

func (a *perToolBearerAsserter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

func (a *perToolBearerAsserter) inspect(authz string, body []byte) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string            `json:"name"`
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.Method != "tools/call" {
		return
	}
	a.callsSeen.Add(1)
	tag, ok := a.toolTag[msg.Params.Name]
	if !ok {
		return
	}
	want := "Bearer " + tag + "-" + msg.Params.Meta["tenant"] + "-" + msg.Params.Meta["user"]
	if authz != want {
		a.mismatches.Add(1)
		a.sampleMu.Lock()
		if a.sample == "" {
			a.sample = fmt.Sprintf("tool=%s meta(%s,%s) got %q want %q", msg.Params.Name, msg.Params.Meta["tenant"], msg.Params.Meta["user"], authz, want)
		}
		a.sampleMu.Unlock()
	}
}

// TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed is the D-025 + isolation
// gate for the per-tool OAuth binding: N>=128 concurrent calls through ONE
// shared Provider, split across a connection-level binding and a per-tool
// override, each with DISTINCT identities. The fixture asserts, per request,
// that the bearer matches BOTH the tool's bound provider AND the request
// identity — a bleed across tools or identities is impossible to pass. Runs
// under -race.
func TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed(t *testing.T) {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-pertool-fixture", Version: "v0"}, nil)
	okHandler := func(_ context.Context, _ *mcpsdk.CallToolRequest, _ struct{}) (*mcpsdk.CallToolResult, any, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil, nil
	}
	for _, name := range []string{"conn_tool", "tool_bound"} {
		mcpsdk.AddTool(srv, &mcpsdk.Tool{
			Name: name, Description: name,
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		}, okHandler)
	}
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	asserter := &perToolBearerAsserter{
		inner:   handler,
		toolTag: map[string]string{"conn_tool": "CONN", "tool_bound": "TOOL"},
	}
	hs := httptest.NewServer(asserter)
	t.Cleanup(hs.Close)

	host := strings.TrimPrefix(hs.URL, "http://")
	bus := mkScopeTestBus(t)
	connProv := prefixBearerProvider{tag: "CONN", hosts: []string{host}}
	toolProv := prefixBearerProvider{tag: "TOOL", hosts: []string{host}}

	p, err := New(Config{
		Name:               "pertool",
		TransportMode:      TransportStreamableHTTP,
		URL:                hs.URL,
		Bus:                bus,
		DefaultIdentity:    identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:      connProv,
		ToolOAuthProviders: map[string]auth.OAuthProvider{"tool_bound": toolProv},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	connTool := discoverTool(t, p, "pertool_conn_tool")
	toolBound := discoverTool(t, p, "pertool_tool_bound")

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
			// Half hit the connection-level binding, half the per-tool override.
			if i%2 == 0 {
				_, errs[i] = connTool.Invoke(ctx, json.RawMessage(`{}`))
			} else {
				_, errs[i] = toolBound.Invoke(ctx, json.RawMessage(`{}`))
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d Invoke failed: %v", i, e)
		}
	}
	if got := asserter.callsSeen.Load(); got < n {
		t.Fatalf("server saw %d tools/call, want >= %d", got, n)
	}
	if got := asserter.mismatches.Load(); got != 0 {
		t.Fatalf("TOKEN BLEED: %d mismatches; sample: %s", got, asserter.sample)
	}
	_ = p.Close(context.Background())
	settleGoroutines(t, baseline)
}
