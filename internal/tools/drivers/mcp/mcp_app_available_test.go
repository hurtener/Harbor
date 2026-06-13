package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// newAppToolProvider builds an in-memory MCP server whose `weather` tool
// returns a result carrying `_meta.ui.resourceUri` (an MCP App), paired with
// a connected Provider. The `plain` tool returns no app, so the negative
// branch (no discovery event for an ordinary result) is testable.
func newAppToolProvider(t *testing.T, bus events.EventBus, resourceURI string) *Provider {
	t.Helper()
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-app-tool-test-server", Version: "v0"},
		&mcpsdk.ServerOptions{},
	)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "weather",
			Description: "Returns weather and declares a ui:// app.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"temp":21}`}},
				Meta:    mcpsdk.Meta{"ui": map[string]any{"resourceUri": resourceURI, "preferredFrame": "inline"}},
			}, nil, nil
		},
	)
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "plain",
			Description: "Returns a plain result with no app.",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"ok":true}`}},
			}, nil, nil
		},
	)

	p, err := New(Config{
		Name:            "weather-server",
		URL:             "http://example.invalid",
		TransportMode:   TransportAuto,
		Bus:             bus,
		DefaultIdentity: defaultIdentity(),
		DefaultPolicy:   tools.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	clientSession, err := p.client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	p.mu.Lock()
	p.session = clientSession
	p.selectedMode = MCPTransportMode("inmemory")
	p.mu.Unlock()
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
		_ = p.Close(context.Background())
	})
	return p
}

// resolveTool discovers the provider's tools and returns the descriptor for
// the Harbor-side `<source>_<name>` tool.
func resolveTool(t *testing.T, p *Provider, harborName string) tools.ToolDescriptor {
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
	t.Fatalf("tool %q not discovered (have %d tools)", harborName, len(descs))
	return tools.ToolDescriptor{}
}

// TestE2E_MCPAppAvailable_PlannerPathEmitsEvent is the load-bearing
// integration test: a PLANNER-initiated MCP tool call (through the
// descriptor's Invoke closure, the same path a planner drives) whose result
// declares a `ui://` app emits `mcp.app_available` on the real bus, carrying
// the server source id, the resource URI, the per-result display-mode hint,
// and the run/identity correlation. This closes the gap where the
// planner-path app reference reached no surface. Runs with the real inmem bus
// and a fake MCP server; -race is the gate.
func TestE2E_MCPAppAvailable_PlannerPathEmitsEvent(t *testing.T) {
	bus := newTestBus(t)
	const resourceURI = "ui://weather/main.html"
	p := newAppToolProvider(t, bus, resourceURI)

	id := defaultIdentity()
	const runID = "run-app-1"
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe bus: %v", err)
	}
	defer sub.Cancel()

	// The planner-invocation ctx carries BOTH the identity (the edge sets it)
	// AND the run quadruple (the run loop adds it) — exactly the production
	// shape, so the emitted event correlates to the run.
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	desc := resolveTool(t, p, "weather-server_weather")
	if _, err := desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != EventTypeMCPAppAvailable {
			t.Fatalf("unexpected event type %q", ev.Type)
		}
		if ev.Identity.RunID != runID {
			t.Errorf("event RunID = %q, want %q", ev.Identity.RunID, runID)
		}
		payload, ok := ev.Payload.(AppAvailablePayload)
		if !ok {
			t.Fatalf("unexpected payload type %T", ev.Payload)
		}
		if payload.ServerID != p.source {
			t.Errorf("payload ServerID = %q, want %q", payload.ServerID, p.source)
		}
		if payload.ResourceURI != resourceURI {
			t.Errorf("payload ResourceURI = %q, want %q", payload.ResourceURI, resourceURI)
		}
		if payload.DisplayMode != "inline" {
			t.Errorf("payload DisplayMode = %q, want inline", payload.DisplayMode)
		}
		if payload.Identity.Identity != id {
			t.Errorf("payload identity = %+v, want %+v", payload.Identity.Identity, id)
		}
		if payload.RawHTMLTrusted {
			t.Errorf("payload RawHTMLTrusted = true, want default-deny false")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for mcp.app_available event")
	}
}

// TestMCPAppAvailable_PlainResultEmitsNoEvent proves the negative branch: an
// ordinary tool result (no `_meta.ui`) emits no discovery event, so an
// ordinary file:// / https:// resource is never mistaken for an app.
func TestMCPAppAvailable_PlainResultEmitsNoEvent(t *testing.T) {
	bus := newTestBus(t)
	p := newAppToolProvider(t, bus, "ui://weather/main.html")

	id := defaultIdentity()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{EventTypeMCPAppAvailable},
	})
	if err != nil {
		t.Fatalf("subscribe bus: %v", err)
	}
	defer sub.Cancel()

	ctx, err := identity.WithRun(mustIdentity(t), id, "run-plain-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	desc := resolveTool(t, p, "weather-server_plain")
	if _, err := desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	select {
	case ev := <-sub.Events():
		t.Fatalf("unexpected discovery event for a plain result: %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// No event — the expected outcome.
	}
}
