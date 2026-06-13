package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestLive_MCPAppAvailable_RealExtAppsServer is the real-server regression
// guard for the spec-conformant MCP App discovery path. It drives a real
// `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) over stdio and
// asserts a UI-bound tool call fires `mcp.app_available` from the tool's
// DEFINITION `_meta.ui.resourceUri` — the placement a real server uses
// (go-study-mcp's tools/list binds `ui://go-study-mcp/studio/index.html` per
// tool, and a tool call returns an empty/null result `_meta`). This is the
// gate the in-memory fixture stands in for: a self-consistent hand fixture
// could match the code while diverging from the wire; the real binary cannot.
//
// CI skips it (no external binary / API key). Run in dev with:
//
//	HARBOR_LIVE_MCP=1 OPENROUTER_API_KEY=sk-or-... \
//	  go test -race -count=1 -timeout 180s \
//	  -run TestLive_MCPAppAvailable_RealExtAppsServer ./internal/tools/drivers/mcp/
//
// The server binary defaults to ~/Repos/go-study-mcp/go-study-mcp; override
// with HARBOR_GO_STUDY_MCP_BIN. The subprocess inherits the test process env,
// so OPENROUTER_API_KEY flows to the server.
func TestLive_MCPAppAvailable_RealExtAppsServer(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_MCP") != "1" {
		t.Skip("set HARBOR_LIVE_MCP=1 to run the live ext-apps server probe (spawns a real MCP binary, may burn API credits)")
	}
	bin := os.Getenv("HARBOR_GO_STUDY_MCP_BIN")
	if bin == "" {
		home, _ := os.UserHomeDir()
		bin = filepath.Join(home, "Repos", "go-study-mcp", "go-study-mcp")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("go-study-mcp binary not found at %q (set HARBOR_GO_STUDY_MCP_BIN): %v", bin, err)
	}
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set — the go-study-mcp tools call OpenRouter")
	}

	bus := newTestBus(t)
	id := defaultIdentity()
	p, err := New(Config{
		Name:            "go-study-mcp",
		TransportMode:   TransportStdio,
		Command:         []string{bin},
		Bus:             bus,
		DefaultIdentity: id,
		DefaultPolicy:   tools.DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// The real server advertises {logging, resources, tools} — NOT the
	// io.modelcontextprotocol/ui capability — so DisplayModes negotiation is
	// empty and the tool-definition `_meta.ui` carries only a resourceUri.
	if modes := p.DisplayModes(); len(modes) != 0 {
		t.Logf("server advertised display modes %v (test still asserts the inline default)", modes)
	}

	const harborTool = "go-study-mcp_synthesize_speech"
	var desc tools.ToolDescriptor
	for _, d := range descs {
		if d.Tool.Name == harborTool {
			desc = d
			break
		}
	}
	if desc.Invoke == nil {
		t.Fatalf("tool %q not discovered (have %d descriptors)", harborTool, len(descs))
	}

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

	ctx, err := identity.WithRun(mustIdentity(t), id, "run-live-mcp-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "speech.mp3")
	args, _ := json.Marshal(map[string]any{
		"text":       "Harbor live ext-apps probe.",
		"outputPath": outPath,
	})
	// The call fires the discovery event before returning — even an IsError
	// result (e.g. a TTS failure) still surfaces the app, because the binding
	// is captured at discovery, not parsed from the result. A transport-level
	// error (no CallToolResult) is the only path that skips the event; surface
	// it loudly.
	if _, callErr := desc.Invoke(ctx, args); callErr != nil {
		t.Logf("tool call returned %v (an IsError result still fires discovery)", callErr)
	}

	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(AppAvailablePayload)
		if !ok {
			t.Fatalf("unexpected payload type %T", ev.Payload)
		}
		if payload.ServerID != p.source {
			t.Errorf("payload ServerID = %q, want %q", payload.ServerID, p.source)
		}
		if !IsUIResourceURI(payload.ResourceURI) {
			t.Errorf("payload ResourceURI = %q, want a ui:// URI from the tool definition", payload.ResourceURI)
		}
		t.Logf("real server fired mcp.app_available: server=%q resource=%q displayMode=%q",
			payload.ServerID, payload.ResourceURI, payload.DisplayMode)
	case <-time.After(120 * time.Second):
		t.Fatalf("timeout waiting for mcp.app_available from the real ext-apps server — discovery did not fire from the tool definition")
	}
}
