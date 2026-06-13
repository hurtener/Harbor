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
// declares `_meta.ui.resourceUri` (an MCP App) on its tool DEFINITION and
// returns an EMPTY result `_meta` — exactly matching the canonical
// `io.modelcontextprotocol/ui` (ext-apps) dialect (McpUiToolMetaSchema:
// "UI-related metadata for tools"; resourceUri = "URI of the UI resource to
// display for this tool") AND a real ext-apps server (go-study-mcp's
// tools/list binds `ui://…/index.html` per tool, while a successful call
// returns a null result `_meta`). This is the fixture that the spec-
// conformant discovery path is tested against — NOT the earlier
// hand-authored fixture that put `_meta.ui` on the RESULT (a self-consistent
// shape that matched the buggy code but not the spec, hiding the bug).
//
// The `plain` tool carries NO ui binding and returns no app, so the negative
// branch (no discovery event for an ordinary tool) is testable. The
// `weather_pip` tool declares the binding on its definition AND emits a
// per-result display-mode hint on its result `_meta.ui`, so the secondary
// merge (result hint wins for display mode over the binding default) is
// testable.
func newAppToolProvider(t *testing.T, bus events.EventBus, resourceURI string) *Provider {
	t.Helper()
	srv := mcpsdk.NewServer(
		&mcpsdk.Implementation{Name: "harbor-app-tool-test-server", Version: "v0"},
		&mcpsdk.ServerOptions{},
	)
	// CANONICAL PLACEMENT: the `ui://` resource URI rides the tool DEFINITION's
	// `_meta.ui` slot (Tool.Meta), and the call result carries an EMPTY `_meta`.
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "weather",
			Description: "Returns weather and declares a ui:// app on its definition.",
			Meta:        mcpsdk.Meta{"ui": map[string]any{"resourceUri": resourceURI}},
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			// EMPTY result _meta — the conformant golden path.
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"temp":21}`}},
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
	mcpsdk.AddTool(srv,
		&mcpsdk.Tool{
			Name:        "weather_pip",
			Description: "Declares a ui:// app on its definition AND a per-result display-mode hint.",
			Meta:        mcpsdk.Meta{"ui": map[string]any{"resourceUri": resourceURI}},
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		func(_ context.Context, _ *mcpsdk.CallToolRequest, _ any) (*mcpsdk.CallToolResult, any, error) {
			// A server MAY steer THIS result's presentation with a per-result
			// `_meta.ui` hint; the resource URI still comes from the binding.
			return &mcpsdk.CallToolResult{
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"temp":21}`}},
				Meta:    mcpsdk.Meta{"ui": map[string]any{"preferredFrame": "pip"}},
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
// descriptor's Invoke closure, the same path a planner drives) to a tool that
// declares a `ui://` app on its DEFINITION emits `mcp.app_available` on the
// real bus — even though the call result carries an EMPTY `_meta` (the
// conformant golden path). The event carries the server source id, the
// resource URI captured from the tool definition, an empty display-mode hint
// (the renderer defaults to inline), and the run/identity correlation. The
// reconciled app reference is ALSO surfaced on the returned ToolResult.Value,
// the source the app-tool-call proxy projects from.
//
// If the parse reverted to result-only (the pre-fix bug), this test FAILS:
// the conformant server's empty result `_meta` carries no `ui` slot, so
// discovery never fires and the select times out. Runs with the real inmem
// bus and a fake MCP server; -race is the gate.
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
	res, err := desc.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// The reconciled app reference is surfaced on the result value — the
	// source the app-tool-call proxy (mcpconsole) projects onto its response.
	val, ok := res.Value.(MCPToolValue)
	if !ok {
		t.Fatalf("result Value type = %T, want MCPToolValue", res.Value)
	}
	if val.AppRef == nil || val.AppRef.ResourceURI != resourceURI {
		t.Fatalf("result AppRef = %+v, want ResourceURI %q (from the tool definition)", val.AppRef, resourceURI)
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
		// The canonical tool-definition `_meta.ui` carries no display mode, and
		// the conformant result `_meta` is empty — so the hint is empty and the
		// Console renderer defaults to inline (mcp-app.svelte: `app?.displayMode
		// || 'inline'`).
		if payload.DisplayMode != "" {
			t.Errorf("payload DisplayMode = %q, want empty (renderer defaults to inline)", payload.DisplayMode)
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

// TestMCPAppAvailable_ResultHintMergesOverBinding proves the secondary merge:
// a tool that declares the `ui://` binding on its DEFINITION and emits a
// per-result display-mode hint on its result `_meta.ui` surfaces the binding's
// resource URI with the per-result hint winning for display mode.
func TestMCPAppAvailable_ResultHintMergesOverBinding(t *testing.T) {
	bus := newTestBus(t)
	const resourceURI = "ui://weather/main.html"
	p := newAppToolProvider(t, bus, resourceURI)

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

	ctx, err := identity.WithRun(mustIdentity(t), id, "run-pip-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	desc := resolveTool(t, p, "weather-server_weather_pip")
	if _, err := desc.Invoke(ctx, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(AppAvailablePayload)
		if !ok {
			t.Fatalf("unexpected payload type %T", ev.Payload)
		}
		if payload.ResourceURI != resourceURI {
			t.Errorf("payload ResourceURI = %q, want %q (the tool-definition binding)", payload.ResourceURI, resourceURI)
		}
		if payload.DisplayMode != "pip" {
			t.Errorf("payload DisplayMode = %q, want pip (per-result hint wins over the binding)", payload.DisplayMode)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for mcp.app_available event")
	}
}

// TestMCPAppAvailable_PlainResultEmitsNoEvent proves the negative branch: a
// tool with NO `_meta.ui` binding (and no result hint) emits no discovery
// event, so an ordinary tool — or one referencing a file:// / https://
// resource — is never mistaken for an app.
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
		t.Fatalf("unexpected discovery event for a plain tool: %+v", ev)
	case <-time.After(300 * time.Millisecond):
		// No event — the expected outcome.
	}
}
