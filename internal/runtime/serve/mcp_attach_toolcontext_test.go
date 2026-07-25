package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// mcp_attach_toolcontext_test.go — the RUNTIME-ATTACHED half of MCP Apps data
// delivery.
//
// The boot-config attach path (internal/runtime/assemble) has always wired the
// MCP Apps tool-context store, so a `ui://` app declared by a tool on a
// boot-configured server renders with its real data. The runtime-add path
// (agent_config.add_mcp_connection → MCPConnectionAttacher) did NOT: it built
// AttachDeps without a ToolContext, so an app declared by a tool on a server an
// operator attached at runtime captured nothing, and a host had no context to
// deliver into the rendered app. These tests pin the two halves of the fix —
// the attacher threads the capturer, and an unwired attacher advertises no
// tool-call id rather than promising a context that was never written.

// appToolMCPServer builds a fixture MCP server whose `report` tool declares a
// `ui://` app on its DEFINITION's `_meta.ui` slot — the canonical placement
// (the call result's `_meta` stays empty), so this exercises the same shape a
// real ext-apps server produces rather than an implementer's interpretation
// (CLAUDE.md §17.8).
func appToolMCPServer() *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "harbor-app-attach-fixture", Version: "v0"}, nil)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "report",
			Description: "Renders a report and declares a ui:// app on its definition.",
			Meta:        mcpsdk.Meta{"ui": map[string]any{"resourceUri": "ui://reports/dashboard.html"}},
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"region": map[string]any{"type": "string"}},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, in struct {
			Region string `json:"region"`
		}) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"region":"` + in.Region + `","revenue":42}`}},
			}, nil, nil
		},
	)
	return srv
}

// appToolFixtureServer wraps the app-declaring MCP server in a
// streamable-HTTP httptest server whose Close is registered with t.Cleanup.
func appToolFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := appToolMCPServer()
	hs := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil))
	t.Cleanup(hs.Close)
	return hs
}

// attachCapturer is a thread-safe in-memory mcpdrv.ToolContextCapturer. The
// runtime's real store is exercised by its own package's tests; here the seam
// under test is the WIRING — whether the attacher hands a capturer to the
// driver at all — so a recording double is the right instrument.
type attachCapturer struct {
	mu    sync.Mutex
	calls []mcpdrv.CapturedToolContext
	ids   []identity.Identity
}

func (c *attachCapturer) Capture(ctx context.Context, in mcpdrv.CapturedToolContext) error {
	id, _ := identity.From(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, in)
	c.ids = append(c.ids, id)
	return nil
}

func (c *attachCapturer) snapshot() ([]mcpdrv.CapturedToolContext, []identity.Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mcpdrv.CapturedToolContext(nil), c.calls...),
		append([]identity.Identity(nil), c.ids...)
}

// attachAppServer attaches the app-declaring fixture through the production
// attacher and returns the resolved catalog descriptor for its `report` tool.
func attachAppServer(t *testing.T, a *MCPConnectionAttacher, cat tools.ToolCatalog, name, url string) tools.ToolDescriptor {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		AgentID:   "agent-1",
		Name:      name,
		Transport: agentcfg.MCPTransportHTTP,
		URL:       url,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	d, ok := cat.Resolve(name + "_report")
	if !ok {
		t.Fatalf("catalog missing the attached app tool %s_report", name)
	}
	return d
}

// runScopedCtx returns a ctx carrying the caller identity plus a run id (the
// tool-call id hashes the run, and the discovery event correlates by it).
func runScopedCtx(t *testing.T, runID string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// TestMCPConnectionAttacher_ToolContextCaptured_RuntimeAddedServer drives the
// REAL attach lifecycle against a REAL streamable-HTTP MCP server whose tool
// declares a `ui://` app, and proves the runtime-added connection captures the
// tool context exactly as a boot-config one does: the capturer records the
// invocation's input + lowered result under the identity the call ran with,
// and the discovery event advertises the SAME id the record is keyed by — so a
// host can fetch it and the rendered app shows real data.
//
// Before the fix, the attacher built AttachDeps with no ToolContext: the
// capturer recorded nothing while the event still advertised an id, and a host
// fetching that id got a miss for a record that never existed.
func TestMCPConnectionAttacher_ToolContextCaptured_RuntimeAddedServer(t *testing.T) {
	const name = "reports"
	fixture := appToolFixtureServer(t)

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	cap := &attachCapturer{}
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil, cap)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t", User: "u", Session: "s",
		Types: []events.EventType{mcpdrv.EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	desc := attachAppServer(t, a, cat, name, fixture.URL)

	const runID = "run-attach-ctx-1"
	args := json.RawMessage(`{"region":"emea"}`)
	if _, ierr := desc.Invoke(runScopedCtx(t, runID), args); ierr != nil {
		t.Fatalf("invoke the attached app tool: %v", ierr)
	}

	calls, ids := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("capturer recorded %d contexts, want 1 — the runtime-added connection did not capture", len(calls))
	}
	got := calls[0]
	if string(got.ServerID) != name || got.Tool != "report" {
		t.Errorf("captured metadata = server %q tool %q, want %q / report", got.ServerID, got.Tool, name)
	}
	if len(got.Result) == 0 {
		t.Error("captured result is empty — the delivered app would render no data")
	}
	// Identity rides the call's ctx, never the attacher's default (§6).
	if ids[0].TenantID != "t" || ids[0].UserID != "u" || ids[0].SessionID != "s" {
		t.Errorf("captured under identity %+v, want the invoking caller's (t,u,s)", ids[0])
	}

	// The discovery event advertises the id the record is keyed by — the
	// correlation a host follows to fetch the context.
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(mcpdrv.AppAvailablePayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		if payload.ToolCallID == "" {
			t.Fatal("event ToolCallID empty despite a successful capture")
		}
		if payload.ToolCallID != got.ToolCallID {
			t.Errorf("event ToolCallID = %q, captured under %q — a host would fetch a miss",
				payload.ToolCallID, got.ToolCallID)
		}
		if payload.ResourceURI != "ui://reports/dashboard.html" {
			t.Errorf("event ResourceURI = %q, want the tool definition's ui:// URI", payload.ResourceURI)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.app_available event from the runtime-added server")
	}
}

// TestMCPConnectionAttacher_NoCapturer_AdvertisesNoToolCallID is the
// defence-in-depth half: an attacher built with NO capturer (a legitimate
// embedder shape) still attaches and still discovers the app, but the
// discovery advertises NO tool-call id — so a host mounts the app with no data
// delivery instead of reporting the view as lost for a context that was never
// written.
func TestMCPConnectionAttacher_NoCapturer_AdvertisesNoToolCallID(t *testing.T) {
	const name = "reports-nocap"
	fixture := appToolFixtureServer(t)

	cat := tools.NewCatalog()
	reg := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, reg, bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil, nil)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t", User: "u", Session: "s",
		Types: []events.EventType{mcpdrv.EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	desc := attachAppServer(t, a, cat, name, fixture.URL)
	if _, ierr := desc.Invoke(runScopedCtx(t, "run-attach-nocap-1"), json.RawMessage(`{"region":"emea"}`)); ierr != nil {
		t.Fatalf("invoke the attached app tool: %v", ierr)
	}

	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(mcpdrv.AppAvailablePayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		if payload.ToolCallID != "" {
			t.Errorf("event ToolCallID = %q, want empty (no capturer wired — the id would promise a record that does not exist)", payload.ToolCallID)
		}
		if payload.ResourceURI == "" {
			t.Error("event ResourceURI empty — the app must still be discovered without a capturer")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no mcp.app_available event from the runtime-added server")
	}
}
