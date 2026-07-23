package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcp_reattach_test.go — the live-layer idempotent MCP re-attach (D-339)
// exercised through the PRODUCTION attacher (MCPConnectionAttacher): a
// same-name add against a still-live registration is an atomic upsert — the
// old server's catalog tools are deregistered and its transport closed, then
// the new connection's tools land — instead of failing on a duplicate tool
// name. Wave-level seam: the attach path (internal/tools/drivers/mcp) meeting
// the runtime add-connection glue (internal/runtime/serve).

// reattachEchoMCPServer builds an MCP server exposing one echo tool (discovered
// as `<name>_echo`). Callers wrap it in a streamable-HTTP httptest server and
// own its lifecycle — the goroutine-baseline test closes the fixture BEFORE
// asserting the baseline, so it cannot use a t.Cleanup-owned server.
func reattachEchoMCPServer() *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-reattach-fixture", Version: "v0"}, nil)
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
	return srv
}

// reattachFixtureServer wraps the echo MCP server in a streamable-HTTP httptest
// server whose Close is registered with t.Cleanup. Use it when the test does
// not need to tear the fixture down before an assertion.
func reattachFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := reattachEchoMCPServer()
	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs
}

// reattachFakeProvider is a serve-local stand-in for a still-live MCP provider
// (the deferred detach has not run). It satisfies the MCP registry's provider
// contract via its exported methods and records that its transport was closed.
type reattachFakeProvider struct {
	id     tools.ToolSourceID
	mu     sync.Mutex
	closed int
}

func (p *reattachFakeProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *reattachFakeProvider) DisplayModes() []string       { return nil }
func (p *reattachFakeProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (p *reattachFakeProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", nil
}
func (p *reattachFakeProvider) Close(context.Context) error {
	p.mu.Lock()
	p.closed++
	p.mu.Unlock()
	return nil
}
func (p *reattachFakeProvider) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

func reattachIDCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestMCPConnectionAttacher_ReAttach_ReplacesLiveRegistration drives the real
// production attacher: with a still-live same-name registration (a fake old
// provider plus its catalog tool under the exact name the fixture discovers),
// a runtime add of the SAME name succeeds as an atomic upsert — the old
// transport is closed and the new tool set is live — rather than colliding on
// the duplicate tool name.
func TestMCPConnectionAttacher_ReAttach_ReplacesLiveRegistration(t *testing.T) {
	const name = "reattach"

	// Goroutine baseline captured BEFORE any fixture / transport exists, so the
	// "no leak after a replace" assertion below is measured against a clean
	// floor once the whole thing — attacher, replaced fake, surviving real
	// transport, and the httptest fixture — is torn down.
	base := goruntime.NumGoroutine()

	mcpSrv := reattachEchoMCPServer()
	fixture := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return mcpSrv }, nil))
	fixtureClosed := false
	closeFixture := func() {
		if !fixtureClosed {
			fixtureClosed = true
			fixture.Close()
		}
	}
	t.Cleanup(closeFixture)

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	// Pre-seed a still-LIVE same-name registration + its colliding catalog tool,
	// OWNED BY the re-attaching caller (owner = {tenant "t", agent "agent-1"} —
	// the tuple the attacher stamps from the AttachRequest below), so the
	// re-attach is a SAME-OWNER supersede.
	callerOwner := toolauth.Owner{Tenant: "t", Agent: "agent-1"}
	oldProv := &reattachFakeProvider{id: tools.ToolSourceID(name)}
	if err := reg.Register(reattachIDCtx(t), mcpdrv.ServerRegistration{
		Provider: oldProv, Transport: "stdio", InitialState: mcpdrv.ServerStateOnline, Owner: callerOwner,
	}); err != nil {
		t.Fatalf("pre-seed registry: %v", err)
	}
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:      name + "_echo",
			Transport: tools.TransportInProcess,
			Source:    tools.ToolSourceID(name),
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("pre-seed catalog: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		AgentID:   "agent-1",
		Name:      name,
		Transport: agentcfg.MCPTransportHTTP,
		URL:       fixture.URL,
	}); err != nil {
		t.Fatalf("re-attach through the production attacher must succeed (upsert), got: %v", err)
	}

	// The old transport was closed exactly once by the replace's registry leg.
	if c := oldProv.closeCount(); c != 1 {
		t.Fatalf("old transport Close called %d times, want 1 (leak if 0)", c)
	}
	// The NEW tool set is live: reattach_echo now resolves to the MCP descriptor.
	d, ok := cat.Resolve(name + "_echo")
	if !ok {
		t.Fatal("catalog missing the re-attached tool reattach_echo")
	}
	if d.Tool.Transport != tools.TransportMCP {
		t.Fatalf("resolved tool is not the new MCP descriptor: transport=%q", d.Tool.Transport)
	}
	// The registry holds exactly the new provider, online.
	servers, _, lerr := reg.ListServers(reattachIDCtx(t), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	if len(servers) != 1 || servers[0].State != mcpdrv.ServerStateOnline || servers[0].ToolCount == 0 {
		t.Fatalf("registry not in the post-upsert state: %+v", servers)
	}

	// Goroutine baseline restored after teardown — the replaced transport (the
	// fake, no goroutines) plus the surviving real transport drain on Close, and
	// the fixture server winds down on its own Close, with no leak (the
	// acceptance criterion's "no leak after a replace").
	_ = a.Close(context.Background())
	closeFixture()
	deadline := time.Now().Add(5 * time.Second)
	for goruntime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		goruntime.GC()
	}
	if leaked := goruntime.NumGoroutine() - base; leaked > 2 {
		t.Errorf("goroutine leak after re-attach + Close: baseline=%d now=%d (leaked ~%d)", base, goruntime.NumGoroutine(), leaked)
	}
}

// TestMCPConnectionAttacher_ReAttach_Concurrent drives N interleaved same-name
// adds against ONE shared production attacher (D-025). The attacher serialises
// the whole attach under its own mutex; every re-attach is a clean upsert, so
// no add fails on a duplicate registration and exactly one registration
// survives. Under -race.
func TestMCPConnectionAttacher_ReAttach_Concurrent(t *testing.T) {
	const name = "reattach-concurrent"
	const n = 100
	fixture := reattachFixtureServer(t)

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
				Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
				AgentID:   "agent-1",
				Name:      name,
				Transport: agentcfg.MCPTransportHTTP,
				URL:       fixture.URL,
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent same-name add failed (duplicate registration?): %v", err)
	}

	// Exactly one registration survives — no cross-talk.
	servers, _, lerr := reg.ListServers(reattachIDCtx(t), mcpdrv.ListFilter{})
	if lerr != nil {
		t.Fatalf("ListServers: %v", lerr)
	}
	if len(servers) != 1 || servers[0].Name != name {
		t.Fatalf("want exactly one surviving registration %q, got %+v", name, servers)
	}
}
