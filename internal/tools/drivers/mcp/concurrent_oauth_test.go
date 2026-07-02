package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// identityBearerProvider mints a per-identity token derived from the ctx
// triple, mirroring a real broker's identity-bound minting. It is the fixture
// broker for the D-025 token-bleed test: the token embeds tenant+user so a
// server can assert the received bearer matches the request's `_meta` triple.
type identityBearerProvider struct{}

func (identityBearerProvider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return auth.Token{}, auth.ErrIdentityRequired
	}
	return auth.Token{AccessToken: "brokered-" + id.TenantID + "-" + id.UserID, BindingScope: auth.ScopeUser}, nil
}
func (identityBearerProvider) InitiateFlow(context.Context, tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, nil
}
func (identityBearerProvider) CompleteFlow(context.Context, string, string) (auth.Token, error) {
	return auth.Token{}, nil
}
func (identityBearerProvider) PendingFlow(string) (auth.PendingFlowInfo, bool) {
	return auth.PendingFlowInfo{}, false
}
func (identityBearerProvider) DenyFlow(context.Context, string, string) error   { return nil }
func (identityBearerProvider) Revoke(context.Context, tools.ToolSourceID) error { return nil }
func (identityBearerProvider) Close(context.Context) error                      { return nil }

// bearerMetaAsserter wraps an MCP streamable-HTTP handler and, for every
// tools/call POST, asserts the request's Authorization bearer matches the
// `_meta` triple carried on the SAME request — the isolation-critical
// no-token-bleed check. Mismatches are recorded atomically; the test asserts
// zero.
type bearerMetaAsserter struct {
	inner      http.Handler
	mismatches atomic.Int64
	callsSeen  atomic.Int64
	sampleMu   sync.Mutex
	sampleMsg  string
}

func (a *bearerMetaAsserter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err == nil {
			a.inspect(r.Header.Get("Authorization"), body)
		}
		r.Body = io.NopCloser(newReader(body))
		r.ContentLength = int64(len(body))
	}
	a.inner.ServeHTTP(w, r)
}

func (a *bearerMetaAsserter) inspect(authz string, body []byte) {
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string            `json:"name"`
			Meta map[string]string `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return
	}
	if msg.Method != "tools/call" {
		return
	}
	a.callsSeen.Add(1)
	tenant := msg.Params.Meta["tenant"]
	user := msg.Params.Meta["user"]
	want := "Bearer brokered-" + tenant + "-" + user
	if authz != want {
		a.mismatches.Add(1)
		a.sampleMu.Lock()
		if a.sampleMsg == "" {
			a.sampleMsg = fmt.Sprintf("meta(tenant=%s,user=%s) got Authorization=%q want %q", tenant, user, authz, want)
		}
		a.sampleMu.Unlock()
	}
}

func newReader(b []byte) io.Reader { return &byteSliceReader{b: b} }

type byteSliceReader struct {
	b   []byte
	pos int
}

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// newStreamableEchoServer builds an httptest server hosting a go-sdk MCP
// server with a single `echo` tool over the streamable-HTTP transport (the
// D-224 real-SDK-wire pattern), fronted by the bearer↔_meta asserter.
func newStreamableEchoServer(t *testing.T) (*httptest.Server, *bearerMetaAsserter) {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-oauth-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "echo",
			Description: "echo",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"text": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			Text string `json:"text"`
		}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: in.Text}}}, nil, nil
		},
	)
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
	asserter := &bearerMetaAsserter{inner: handler}
	hs := httptest.NewServer(asserter)
	t.Cleanup(hs.Close)
	return hs, asserter
}

func newOAuthBoundProvider(t *testing.T, url string) *Provider {
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
	p, err := New(Config{
		Name:            "oauthmcp",
		TransportMode:   TransportStreamableHTTP,
		URL:             url,
		Bus:             bus,
		DefaultIdentity: identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"},
		OAuthProvider:   identityBearerProvider{},
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

// TestConcurrentReuse_OAuthBearer_NoTokenBleed is the D-025 + isolation gate:
// N>=100 concurrent calls through ONE shared Provider with DISTINCT identity
// triples, each minting an identity-derived bearer. The fixture server
// asserts, per request, that the received Authorization bearer matches the
// `_meta` triple on the SAME request — a token bleed across identities is the
// failure this makes impossible. Runs under -race.
func TestConcurrentReuse_OAuthBearer_NoTokenBleed(t *testing.T) {
	hs, asserter := newStreamableEchoServer(t)
	p := newOAuthBoundProvider(t, hs.URL)
	if err := p.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var echo tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == "oauthmcp_echo" {
			echo = d
			break
		}
	}
	if echo.Invoke == nil {
		t.Fatalf("echo descriptor not discovered; got %d descriptors", len(descs))
	}

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
			_, errs[i] = echo.Invoke(ctx, json.RawMessage(`{"text":"hi"}`))
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d Invoke failed: %v", i, e)
		}
	}
	if got := asserter.callsSeen.Load(); got < n {
		t.Fatalf("server saw %d tools/call requests, want >= %d", got, n)
	}
	if got := asserter.mismatches.Load(); got != 0 {
		t.Fatalf("TOKEN BLEED: %d bearer<->_meta mismatches; sample: %s", got, asserter.sampleMsg)
	}

	// Cancellation cross-talk + goroutine baseline: close mid-life and assert
	// the goroutine count returns near baseline (the SDK session goroutines
	// are joined on Close).
	_ = p.Close(context.Background())
	settleGoroutines(t, baseline)
}

func settleGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+8 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Non-fatal slack: the httptest server + SDK may retain a few goroutines
	// briefly; a gross leak (many multiples of N) is the real failure.
	if got := runtime.NumGoroutine(); got > baseline+40 {
		t.Fatalf("goroutine leak: baseline %d, after close %d", baseline, got)
	}
}
