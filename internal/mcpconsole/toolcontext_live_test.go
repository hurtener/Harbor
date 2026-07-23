package mcpconsole_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestLive_ToolContext_RealServerRoundTrip is the §17.8 real-server probe for
// the MCP Apps "Data Delivery" lifecycle. It drives a real
// `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) over stdio with
// the tool-context capturer wired into the Provider (exactly as the runtime
// wires it at boot), invokes a tool that declares a `ui://` app — capturing
// the input + lowered result at the invocation site — and reads the captured
// context back through AppsAccessor.ToolContext (the `mcp.apps.tool_context`
// Protocol read path), asserting the round-trip.
//
// CI skips it (no external binary). Run in dev with:
//
//	HARBOR_LIVE_MCP=1 go test -race -count=1 -timeout 120s \
//	  -run TestLive_ToolContext_RealServerRoundTrip ./internal/mcpconsole/
//
// The server binary defaults to ~/Repos/go-study-mcp/go-study-mcp; override
// with HARBOR_GO_STUDY_MCP_BIN.
func TestLive_ToolContext_RealServerRoundTrip(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_MCP") != "1" {
		t.Skip("set HARBOR_LIVE_MCP=1 to run the live ext-apps tool-context round-trip probe (spawns a real MCP binary)")
	}
	bin := os.Getenv("HARBOR_GO_STUDY_MCP_BIN")
	if bin == "" {
		home, _ := os.UserHomeDir()
		bin = filepath.Join(home, "Repos", "go-study-mcp", "go-study-mcp")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("go-study-mcp binary not found at %q (set HARBOR_GO_STUDY_MCP_BIN): %v", bin, err)
	}

	bus := newAppsBus(t)
	tc := newToolCtxStore(t, 0)
	id := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	p, err := mcp.New(mcp.Config{
		Name:            "go-study-mcp",
		TransportMode:   mcp.TransportStdio,
		Command:         []string{bin},
		Bus:             bus,
		DefaultIdentity: id,
		DefaultPolicy:   tools.DefaultPolicy(),
		ToolContext:     tc,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	reg := mcp.NewRegistry()
	if err := reg.Register(context.Background(), mcp.ServerRegistration{
		Provider: p, Transport: "stdio", InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry: reg, Catalog: tools.NewCatalog(), Store: newAppsStore(t),
		Bus: bus, ToolContext: tc,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	descs, err := p.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	runCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	runCtx, err = identity.WithRun(runCtx, id, "live-run-1")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Invoke each discovered tool with empty args; the first that declares a
	// `ui://` app exercises the capture + read round-trip.
	for _, d := range descs {
		if d.Invoke == nil {
			continue
		}
		res, invErr := d.Invoke(runCtx, json.RawMessage(`{}`))
		if invErr != nil {
			continue
		}
		val, ok := res.Value.(mcp.MCPToolValue)
		if !ok || val.AppRef == nil || val.AppRef.ToolCallID == "" {
			continue
		}
		row, readErr := acc.ToolContext(runCtx, "go-study-mcp", val.AppRef.ToolCallID)
		if readErr != nil {
			t.Fatalf("ToolContext(%q): %v", val.AppRef.ToolCallID, readErr)
		}
		if row.Result.Inline == nil && row.Result.Artifact == nil {
			t.Fatalf("captured tool context has no result for tool %q", d.Tool.Name)
		}
		t.Logf("real server round-trip: tool %q declared app %q, tool-context read returned (input set=%v, result set=%v)",
			d.Tool.Name, val.AppRef.ResourceURI, row.Input.Inline != nil || row.Input.Artifact != nil,
			row.Result.Inline != nil || row.Result.Artifact != nil)
		return
	}
	t.Skip("no discovered go-study-mcp tool declared a ui:// app on invocation with empty args — cannot exercise the tool-context round-trip")
}
