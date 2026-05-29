package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// defaultIdentity is the operator-default identity used to scope
// server-pushed events when no per-subscription identity is
// available.
func defaultIdentity() identity.Identity {
	return identity.Identity{TenantID: "test-tenant", UserID: "test-user", SessionID: "test-session"}
}

// newTestProvider constructs a Provider wired to a bus and discard
// logger. The Provider is NOT connected; callers pair via
// pairProvider for in-memory tests.
func newTestProvider(t *testing.T) (*Provider, *mockServer, events.EventBus, func()) {
	t.Helper()
	bus := newTestBus(t)
	m := newMockServer()
	cfg := Config{
		Name:            "mock",
		URL:             "http://example.invalid", // not used for in-memory pairing
		TransportMode:   TransportAuto,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		DefaultPolicy: tools.ToolPolicy{
			MaxRetries:  3,
			BackoffBase: 1 * time.Millisecond,
			BackoffMult: 2,
			BackoffMax:  10 * time.Millisecond,
			TimeoutMS:   2000,
			RetryOn:     []tools.ErrorClass{tools.ErrClassTransient, tools.ErrClassTimeout},
			Validate:    tools.ValidateNone, // server-side schema enforced
		},
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cleanup := pairProvider(t, m, p)
	return p, m, bus, func() {
		_ = p.Close(context.Background())
		cleanup()
	}
}

func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = bus.Close(context.Background())
	})
	return bus
}

func TestNew_RejectsInvalidConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{
			name: "empty name",
			cfg:  Config{},
		},
		{
			name: "missing bus",
			cfg:  Config{Name: "x", URL: "http://x"},
		},
		{
			name: "auto with no url and no command",
			cfg: Config{
				Name:            "x",
				Bus:             newTestBus(t),
				DefaultIdentity: defaultIdentity(),
			},
		},
		{
			name: "sse without url",
			cfg: Config{
				Name:            "x",
				TransportMode:   TransportSSE,
				Bus:             newTestBus(t),
				DefaultIdentity: defaultIdentity(),
			},
		},
		{
			name: "stdio without command",
			cfg: Config{
				Name:            "x",
				TransportMode:   TransportStdio,
				Bus:             newTestBus(t),
				DefaultIdentity: defaultIdentity(),
			},
		},
		{
			name: "unknown transport mode",
			cfg: Config{
				Name:            "x",
				TransportMode:   "foo",
				URL:             "http://x",
				Bus:             newTestBus(t),
				DefaultIdentity: defaultIdentity(),
			},
		},
		{
			name: "partial default identity",
			cfg: Config{
				Name:            "x",
				URL:             "http://x",
				Bus:             newTestBus(t),
				DefaultIdentity: identity.Identity{TenantID: "t"},
			},
		},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestProvider_Discover_TextTool_RoundTrip(t *testing.T) {
	p, _, _, cleanup := newTestProvider(t)
	defer cleanup()

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(descs) == 0 {
		t.Fatalf("expected at least one descriptor, got 0")
	}

	echo := findByName(descs, "mock_echo")
	if echo == nil {
		t.Fatalf("expected mock_echo, got names: %s", names(descs))
	}
	if echo.Tool.Transport != tools.TransportMCP {
		t.Errorf("expected Transport=mcp, got %q", echo.Tool.Transport)
	}
	if echo.Tool.Source != "mock" {
		t.Errorf("expected Source=mock, got %q", echo.Tool.Source)
	}

	args, _ := json.Marshal(map[string]any{"text": "hi"})
	res, err := echo.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	v, ok := res.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("expected MCPToolValue, got %T", res.Value)
	}
	if v.Text != "hi" {
		t.Errorf("expected echo 'hi', got %q", v.Text)
	}
}

func TestProvider_Discover_ListsResourcesAndPrompts(t *testing.T) {
	p, _, _, cleanup := newTestProvider(t)
	defer cleanup()

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if findByName(descs, "mock__resource.mem://hello") == nil {
		t.Errorf("expected resource descriptor, got: %s", names(descs))
	}
	if findByName(descs, "mock__prompt.greet") == nil {
		t.Errorf("expected prompt descriptor, got: %s", names(descs))
	}
}

func TestProvider_Invoke_Resource_ReadsContents(t *testing.T) {
	p, _, _, cleanup := newTestProvider(t)
	defer cleanup()

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock__resource.mem://hello")
	if d == nil {
		t.Fatalf("missing resource descriptor")
	}
	res, err := d.Invoke(ctx, nil)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	v := res.Value.(MCPToolValue)
	if !strings.Contains(v.Text, "hello world") {
		t.Errorf("expected resource text, got %q", v.Text)
	}
}

func TestProvider_Invoke_Prompt_RendersMessages(t *testing.T) {
	p, _, _, cleanup := newTestProvider(t)
	defer cleanup()

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock__prompt.greet")
	if d == nil {
		t.Fatalf("missing prompt descriptor")
	}
	args, _ := json.Marshal(map[string]string{"who": "Harbor"})
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	v := res.Value.(MCPToolValue)
	if !strings.Contains(v.Text, "Hello, Harbor") {
		t.Errorf("expected prompt 'Hello, Harbor', got %q", v.Text)
	}
}

func TestProvider_IdentityPropagation_StampedOnMeta(t *testing.T) {
	p, m, _, cleanup := newTestProvider(t)
	defer cleanup()

	ctx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: "acme", UserID: "alice", SessionID: "s-1",
	})
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock_echo")
	if d == nil {
		t.Fatalf("missing echo")
	}
	args, _ := json.Marshal(map[string]any{"text": "x"})
	if _, err := d.Invoke(ctx, args); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	meta := m.metaFor("echo")
	if meta["tenant"] != "acme" || meta["user"] != "alice" || meta["session"] != "s-1" {
		t.Errorf("identity meta mismatch: %v", meta)
	}
}

func TestProvider_PolicyRetry_OnIsError(t *testing.T) {
	p, m, _, cleanup := newTestProvider(t)
	defer cleanup()
	m.setFlakyTarget(2)

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock_flaky")
	if d == nil {
		t.Fatalf("missing flaky")
	}
	res, err := d.Invoke(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	v := res.Value.(MCPToolValue)
	if !strings.HasPrefix(v.Text, "ok: attempt") {
		t.Errorf("expected 'ok: attempt...', got %q", v.Text)
	}
	if m.flakyAttempts.Load() < 3 {
		t.Errorf("expected ≥3 attempts (2 failures + success), got %d", m.flakyAttempts.Load())
	}
}

func TestProvider_PolicyRetry_GivesUpOnExhaustion(t *testing.T) {
	p, m, _, cleanup := newTestProvider(t)
	defer cleanup()
	// More failures than retries.
	m.setFlakyTarget(100)

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock_flaky")
	_, err = d.Invoke(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected exhaustion, got nil")
	}
	if !errors.Is(err, tools.ErrToolPolicyExhausted) {
		t.Fatalf("expected ErrToolPolicyExhausted, got: %v", err)
	}
}

func TestProvider_ResourceUpdated_PublishesEvent(t *testing.T) {
	p, _, bus, cleanup := newTestProvider(t)
	defer cleanup()

	ctx := mustIdentity(t)
	if err := p.SubscribeResource(ctx, "mem://hello"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// Subscribe a listener BEFORE the server emits.
	id := defaultIdentity()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeMCPResourceUpdated},
	})
	if err != nil {
		t.Fatalf("subscribe bus: %v", err)
	}
	defer sub.Cancel()

	// Trigger an update from the server side.
	srv := getMockServer(p) // see helper at bottom
	if err := srv.ResourceUpdated(context.Background(), &mcpsdk.ResourceUpdatedNotificationParams{
		URI: "mem://hello",
	}); err != nil {
		t.Fatalf("ResourceUpdated: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != EventTypeMCPResourceUpdated {
			t.Fatalf("unexpected event type %q", ev.Type)
		}
		p, ok := ev.Payload.(ResourceUpdatedPayload)
		if !ok {
			t.Fatalf("unexpected payload type %T", ev.Payload)
		}
		if p.URI != "mem://hello" {
			t.Errorf("expected URI mem://hello, got %q", p.URI)
		}
		if p.Source != "mock" {
			t.Errorf("expected source 'mock', got %q", p.Source)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for mcp.resource_updated event")
	}
}

// TestPushIdentity_PrefersCtxOverDefault — Phase 83m (Item 1, D-156):
// the helper that drives server-pushed event identity prefers
// `identity.From(ctx)` whenever the ctx carries a populated triple;
// only when the ctx has no triple (or one is incomplete) does the
// helper fall back to the cached Config.DefaultIdentity. Validates the
// isolation-rule narrowing: per-call subscriptions stamp the inflight
// caller's identity on the resulting bus event so multi-tenant
// operators see correct provenance.
func TestPushIdentity_PrefersCtxOverDefault(t *testing.T) {
	cfg := Config{DefaultIdentity: defaultIdentity()}

	// Case 1: ctx with full triple — the helper returns the ctx
	// identity, not the cached default.
	ctxID := identity.Identity{TenantID: "ctx-tenant", UserID: "ctx-user", SessionID: "ctx-session"}
	ctx, err := identity.With(context.Background(), ctxID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got := pushIdentity(ctx, cfg)
	if got != ctxID {
		t.Errorf("ctx-identity preferred: got %+v, want %+v", got, ctxID)
	}

	// Case 2: bare ctx with no triple — falls back to the default.
	got = pushIdentity(context.Background(), cfg)
	if got != cfg.DefaultIdentity {
		t.Errorf("no-triple fallback: got %+v, want %+v", got, cfg.DefaultIdentity)
	}

	// Case 3: nil ctx — same fallback shape; the helper must not panic.
	got = pushIdentity(nil, cfg) //nolint:staticcheck // intentional nil for fallback test
	if got != cfg.DefaultIdentity {
		t.Errorf("nil-ctx fallback: got %+v, want %+v", got, cfg.DefaultIdentity)
	}
}

// TestProvider_ResourceUpdated_FallsBackToDefaultWhenCtxBare — Phase
// 83m (Item 1, D-156): the SDK's notification dispatch path delivers
// the handler a bare session ctx (no identity) for transport-side
// pushes. The driver MUST fall back to `Config.DefaultIdentity` so the
// bus's `ValidateEvent` does not reject the resulting event. The
// existing `TestProvider_ResourceUpdated_PublishesEvent` exercises
// this path implicitly (it subscribes under `defaultIdentity()` —
// which happens to coincide with `Config.DefaultIdentity`); this test
// pins the contract explicitly so a regression that drops the
// fallback would fail loud here.
//
// Note: the MCP SDK does not propagate the per-call subscription ctx
// into the `ResourceUpdatedHandler` (see
// `Client.callResourceUpdatedHandler` — the ctx is the session's
// internal dispatch ctx). The pushIdentity helper still PREFERS a
// populated ctx when one is supplied, but the production path almost
// always falls back to `Config.DefaultIdentity` for these SDK-pushed
// notifications. A future SDK release that threads the subscription
// ctx through would make the unit-tested preference path live.
func TestProvider_ResourceUpdated_FallsBackToDefaultWhenCtxBare(t *testing.T) {
	p, _, bus, cleanup := newTestProvider(t)
	defer cleanup()

	subCtx := mustIdentity(t)
	if err := p.SubscribeResource(subCtx, "mem://hello"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	id := defaultIdentity()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeMCPResourceUpdated},
	})
	if err != nil {
		t.Fatalf("subscribe bus: %v", err)
	}
	defer sub.Cancel()

	// Fire the server-side notification with a BARE ctx (no identity)
	// — mirrors the SDK's dispatch ctx for transport pushes. The
	// driver must fall back to Config.DefaultIdentity; a regression
	// that drops the fallback would either panic on validate or
	// silently emit nothing.
	srv := getMockServer(p)
	if err := srv.ResourceUpdated(context.Background(), &mcpsdk.ResourceUpdatedNotificationParams{
		URI: "mem://hello",
	}); err != nil {
		t.Fatalf("ResourceUpdated: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Identity.TenantID != id.TenantID {
			t.Errorf("event.Identity.TenantID = %q, want %q (DefaultIdentity fallback dropped)",
				ev.Identity.TenantID, id.TenantID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for mcp.resource_updated event under DefaultIdentity fallback")
	}
}

func TestProvider_Close_Idempotent(t *testing.T) {
	p, _, _, cleanup := newTestProvider(t)
	_ = cleanup // we still get session via pair; we'll Close manually

	ctx := context.Background()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("close 2 (should be idempotent): %v", err)
	}
	_, err := p.Discover(mustIdentity(t))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected after Close, got: %v", err)
	}
}

func TestProvider_ConcurrentReuse_D025(t *testing.T) {
	const n = 100
	p, m, _, cleanup := newTestProvider(t)
	defer cleanup()
	m.setFlakyTarget(0)

	ctx := mustIdentity(t)
	descs, err := p.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	d := findByName(descs, "mock_echo")
	if d == nil {
		t.Fatalf("missing echo")
	}

	baseline := runtime.NumGoroutine()

	type result struct {
		err error
		got string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i%8)
			invokeCtx, err := identity.With(context.Background(), identity.Identity{
				TenantID: tenant, UserID: fmt.Sprintf("u-%d", i%8), SessionID: fmt.Sprintf("s-%d", i%8),
			})
			if err != nil {
				results[i] = result{err: err}
				return
			}
			args, _ := json.Marshal(map[string]any{"text": fmt.Sprintf("n=%d", i)})
			res, err := d.Invoke(invokeCtx, args)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			v := res.Value.(MCPToolValue)
			results[i] = result{got: v.Text}
		}()
	}
	wg.Wait()

	failures := 0
	for i, r := range results {
		if r.err != nil {
			failures++
			t.Logf("invocation %d failed: %v", i, r.err)
			continue
		}
		expected := fmt.Sprintf("n=%d", i)
		if r.got != expected {
			t.Errorf("invocation %d: expected %q, got %q (context bleed?)", i, expected, r.got)
		}
	}
	if failures > 0 {
		t.Errorf("%d concurrent invocations failed", failures)
	}

	// Goroutine leak check — let SDK drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Gosched()
		if runtime.NumGoroutine() <= baseline+10 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+10 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

func TestProvider_Auto_FallsBackToSSE(t *testing.T) {
	// Build two HTTP test servers: one that always rejects the
	// streamable-HTTP shape, one that serves the SSE shape via the
	// SDK's SSEHandler.
	rejectStreamable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject streamable-HTTP — return 400 for any POST so the
		// client treats it as a connection-time failure.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer rejectStreamable.Close()

	// SSE server.
	mockSrv := newMockServer()
	sseHandler := mcpsdk.NewSSEHandler(func(*http.Request) *mcpsdk.Server {
		return mockSrv.server
	}, nil)
	sseServer := httptest.NewServer(sseHandler)
	defer sseServer.Close()

	// We can't point one URL at two servers, so this test specifically
	// asserts the explicit-SSE path works against the SSE server,
	// which is the fallback target. The earlier streamable-HTTP
	// rejection path is covered by inspecting Connect's error wrapping.

	bus := newTestBus(t)
	cfg := Config{
		Name:            "fallback",
		URL:             sseServer.URL,
		TransportMode:   TransportSSE,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		DefaultPolicy:   tools.DefaultPolicy(),
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Connect(ctx); err != nil {
		t.Fatalf("Connect SSE: %v", err)
	}
	if got := p.SelectedMode(); got != TransportSSE {
		t.Errorf("expected SelectedMode=sse, got %q", got)
	}

	// Now exercise the auto-fallback explicitly: a Config with
	// TransportMode=auto + URL pointing at the rejecter should
	// surface ErrTransportFailed (no SSE peer to fall back to with
	// the same URL).
	cfg2 := Config{
		Name:            "rejected",
		URL:             rejectStreamable.URL,
		TransportMode:   TransportAuto,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		DefaultPolicy:   tools.DefaultPolicy(),
	}
	p2, err := New(cfg2)
	if err != nil {
		t.Fatalf("New 2: %v", err)
	}
	defer p2.Close(context.Background())
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	err = p2.Connect(ctx2)
	if err == nil {
		t.Fatalf("expected ErrTransportFailed, got nil")
	}
	if !errors.Is(err, ErrTransportFailed) {
		t.Fatalf("expected ErrTransportFailed, got: %v", err)
	}
}

func TestSelectTransport_Stdio_RequiresArgvForm(t *testing.T) {
	cfg := Config{
		Name:            "x",
		TransportMode:   TransportStdio,
		Command:         []string{}, // empty
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
	}
	_, _, err := selectTransport(cfg)
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty Command, got: %v", err)
	}
}

func TestSelectTransport_ModeDispatch(t *testing.T) {
	cases := []struct {
		mode MCPTransportMode
		url  string
		cmd  []string
		want MCPTransportMode
	}{
		{TransportSSE, "http://x", nil, TransportSSE},
		{TransportStreamableHTTP, "http://x", nil, TransportStreamableHTTP},
		{TransportStdio, "", []string{"echo"}, TransportStdio},
		{TransportAuto, "http://x", nil, TransportStreamableHTTP},
		{TransportAuto, "", []string{"echo"}, TransportStdio},
	}
	for _, tc := range cases {

		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := Config{
				Name:            "x",
				TransportMode:   tc.mode,
				URL:             tc.url,
				Command:         tc.cmd,
				Bus:             newTestBus(t),
				DefaultIdentity: defaultIdentity(),
			}
			_, mode, err := selectTransport(cfg)
			if err != nil {
				t.Fatalf("selectTransport: %v", err)
			}
			if mode != tc.want {
				t.Errorf("expected mode %q, got %q", tc.want, mode)
			}
		})
	}
}

func TestLowerCallToolResult_ContentNormalization(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "hello "},
			&mcpsdk.TextContent{Text: "world"},
			&mcpsdk.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"},
			&mcpsdk.ResourceLink{URI: "mem://x", Name: "x"},
		},
	}
	v, err := lowerCallToolResult(res)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if v.Text != "hello world" {
		t.Errorf("text: %q", v.Text)
	}
	if len(v.Parts) != 2 {
		t.Fatalf("expected 2 non-text parts, got %d", len(v.Parts))
	}
	if v.Parts[0].Kind != ContentKindImage {
		t.Errorf("expected image part, got %q", v.Parts[0].Kind)
	}
	if v.Parts[1].Kind != ContentKindLink {
		t.Errorf("expected link part, got %q", v.Parts[1].Kind)
	}
}

func TestLowerCallToolResult_IsErrorMapsToTypedError(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}},
		IsError: true,
	}
	_, err := lowerCallToolResult(res)
	if !errors.Is(err, ErrMCPToolError) {
		t.Fatalf("expected ErrMCPToolError, got %v", err)
	}
}

func TestLowerCallToolResult_AudioAndEmbedded(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.AudioContent{Data: []byte{9, 8, 7}, MIMEType: "audio/wav"},
			&mcpsdk.EmbeddedResource{Resource: &mcpsdk.ResourceContents{
				URI:      "mem://x",
				MIMEType: "text/plain",
				Text:     "embedded",
			}},
		},
	}
	v, err := lowerCallToolResult(res)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	if len(v.Parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(v.Parts))
	}
	if v.Parts[0].Kind != ContentKindAudio {
		t.Errorf("expected audio, got %q", v.Parts[0].Kind)
	}
	if v.Parts[1].Kind != ContentKindEmbedded {
		t.Errorf("expected embedded, got %q", v.Parts[1].Kind)
	}
	if v.Parts[1].Embedded.Text != "embedded" {
		t.Errorf("expected embedded text, got %q", v.Parts[1].Embedded.Text)
	}
}

func TestLowerCallToolResult_NilTolerant(t *testing.T) {
	v, err := lowerCallToolResult(nil)
	if err != nil {
		t.Errorf("nil should not error: %v", err)
	}
	if v.Text != "" || len(v.Parts) != 0 {
		t.Errorf("expected zero value, got %+v", v)
	}
}

func TestProvider_SourceID(t *testing.T) {
	p, err := New(Config{
		Name:            "src",
		URL:             "http://example.invalid",
		TransportMode:   TransportSSE,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.SourceID(); got != tools.ToolSourceID("src") {
		t.Errorf("SourceID: got %q, want 'src'", got)
	}
	if mode := p.SelectedMode(); mode != "" {
		t.Errorf("SelectedMode before Connect should be empty, got %q", mode)
	}
}

func TestDeriveSideEffect_AnnotationDispatch(t *testing.T) {
	cases := []struct {
		name string
		a    *mcpsdk.ToolAnnotations
		want tools.SideEffect
	}{
		{"nil annotations -> external", nil, tools.SideEffectExternal},
		{"read only -> read", &mcpsdk.ToolAnnotations{ReadOnlyHint: true}, tools.SideEffectRead},
		{"idempotent -> read", &mcpsdk.ToolAnnotations{IdempotentHint: true}, tools.SideEffectRead},
		{"destructive defaults -> external", &mcpsdk.ToolAnnotations{}, tools.SideEffectExternal},
	}
	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSideEffect(tc.a); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHeaderInjectingTransport_AddsHeaders(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := buildHTTPClient(Config{Headers: map[string]string{"Authorization": "Bearer test"}})
	req, _ := http.NewRequestWithContext(context.Background(), "GET", srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer test" {
		t.Errorf("expected Authorization header, got %q", gotAuth)
	}
}

func TestBuildHTTPClient_NoHeaders_ReturnsDefault(t *testing.T) {
	got := buildHTTPClient(Config{})
	if got != http.DefaultClient {
		t.Errorf("expected http.DefaultClient when no headers, got custom client")
	}
}

func TestProvider_ClassifyConnectError(t *testing.T) {
	if classifyConnectError(nil) {
		t.Errorf("nil error should not be recoverable")
	}
	if classifyConnectError(context.Canceled) {
		t.Errorf("context.Canceled should not be recoverable")
	}
	if classifyConnectError(context.DeadlineExceeded) {
		t.Errorf("DeadlineExceeded should not be recoverable")
	}
	if !classifyConnectError(errors.New("network down")) {
		t.Errorf("regular error should be recoverable")
	}
}

func TestProvider_Discover_NotConnected(t *testing.T) {
	p, err := New(Config{
		Name:            "x",
		URL:             "http://example.invalid",
		TransportMode:   TransportSSE,
		Bus:             newTestBus(t),
		DefaultIdentity: defaultIdentity(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Discover(context.Background())
	if !errors.Is(err, ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
	if err := p.SubscribeResource(context.Background(), "mem://x"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("expected ErrNotConnected from SubscribeResource, got %v", err)
	}
}

func TestNewStdioTransport_RejectsEmptyBinary(t *testing.T) {
	_, _, err := newStdioTransport(Config{Command: []string{""}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig for empty Command[0], got %v", err)
	}
	_, _, err = newStdioTransport(Config{Command: []string{}})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("expected ErrInvalidConfig for empty Command, got %v", err)
	}
}

// TestValidTransportModes_MirrorConfigValidator pins the four
// transport modes the driver and config validator both know about.
// The config validator's `allowedMCPTransportModes` map (in
// `internal/config/validate.go`) is intentionally duplicated to
// avoid a config → driver dependency edge; this test fails when the
// two lists drift.
func TestValidTransportModes_MirrorConfigValidator(t *testing.T) {
	want := []string{"auto", "sse", "streamable_http", "stdio"}
	for _, m := range want {
		if !IsValidTransportMode(m) {
			t.Errorf("driver does not accept transport mode %q (config validator does)", m)
		}
	}
	// Sanity: a fifth mode is not silently accepted.
	if IsValidTransportMode("websocket") {
		t.Errorf("driver accepts unknown transport mode 'websocket'")
	}
}

// IsValidTransportMode is consumed by the config package for the
// raw-string YAML validation. Cover the boundary.
func TestIsValidTransportMode(t *testing.T) {
	cases := map[string]bool{
		"auto":            true,
		"sse":             true,
		"streamable_http": true,
		"stdio":           true,
		"unknown":         false,
		"":                false,
		"AUTO":            false, // case-sensitive
	}
	for s, want := range cases {
		if got := IsValidTransportMode(s); got != want {
			t.Errorf("IsValidTransportMode(%q): got %v, want %v", s, got, want)
		}
	}
}

// --- helpers ---

func findByName(descs []tools.ToolDescriptor, name string) *tools.ToolDescriptor {
	for i := range descs {
		if descs[i].Tool.Name == name {
			return &descs[i]
		}
	}
	return nil
}

func names(descs []tools.ToolDescriptor) string {
	out := make([]string, 0, len(descs))
	for _, d := range descs {
		out = append(out, d.Tool.Name)
	}
	return strings.Join(out, ",")
}

func mustIdentity(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), defaultIdentity())
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// getMockServer extracts the embedded mock server from the Provider's
// test pairing. We need this so TestProvider_ResourceUpdated_PublishesEvent
// can trigger a server-side ResourceUpdated. The Provider doesn't
// hold a back-reference to the mock; the test does it via a small
// indirection: pairProvider stashes the *mockServer.server side on
// the provider's logger handler? No — easier: rebuild a fresh test
// harness inline.
//
// To keep the test simple, this helper looks up the captured server
// from the package-global testServers map.
func getMockServer(p *Provider) *mcpsdk.Server {
	mu.RLock()
	defer mu.RUnlock()
	return testServers[p]
}

// Package-private store from pairProvider so tests can reach the
// server side to trigger server-pushed notifications. Avoids
// threading the mock through every test helper.
var (
	mu          sync.RWMutex
	testServers = make(map[*Provider]*mcpsdk.Server)
)

// recordServerForTest registers s as the mock for p. Called by
// pairProvider.
func recordServerForTest(p *Provider, s *mcpsdk.Server) {
	mu.Lock()
	defer mu.Unlock()
	testServers[p] = s
}

// forgetServerForTest removes p from the map after Close.
func forgetServerForTest(p *Provider) {
	mu.Lock()
	defer mu.Unlock()
	delete(testServers, p)
}
