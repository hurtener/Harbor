package mcpconsole_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestLive_AppsAccessor_ReadResource_StudioDocInline is the real-server
// guard for the App-document inline fix. It drives a real
// `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) over stdio
// and reads its studio App document — ~86 KB, above the 32 KiB LLM-context
// heavy threshold — through the SAME AppsAccessor.ReadResource path the
// `mcp.servers.read_resource` Protocol method uses. It asserts the document
// comes back INLINE (not an artifactRef), so the Console renders it on the
// in-memory ArtifactStore with no presigned URL — the live failure mode
// this phase closes (a by-reference App fails loud on every non-S3 driver).
//
// CI skips it (no external binary). It needs NO API key — read_resource
// reads a static resource, it does not call a tool. Run in dev with:
//
//	HARBOR_LIVE_MCP=1 go test -race -count=1 -timeout 120s \
//	  -run TestLive_AppsAccessor_ReadResource_StudioDocInline \
//	  ./internal/mcpconsole/
//
// The server binary defaults to ~/Repos/go-study-mcp/go-study-mcp; override
// with HARBOR_GO_STUDY_MCP_BIN.
func TestLive_AppsAccessor_ReadResource_StudioDocInline(t *testing.T) {
	if os.Getenv("HARBOR_LIVE_MCP") != "1" {
		t.Skip("set HARBOR_LIVE_MCP=1 to run the live ext-apps server read_resource probe (spawns a real MCP binary)")
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
	id := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	p, err := mcp.New(mcp.Config{
		Name:            "go-study-mcp",
		TransportMode:   mcp.TransportStdio,
		Command:         []string{bin},
		Bus:             bus,
		DefaultIdentity: id,
		DefaultPolicy:   tools.DefaultPolicy(),
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
	if err := reg.Register(mcp.ServerRegistration{
		Provider:     p,
		Transport:    "stdio",
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    reg,
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         bus,
		ToolContext: newToolCtxStore(t, 0),
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}

	const studioDoc = "ui://go-study-mcp/studio/index.html"
	got, err := acc.ReadResource(idCtx(t), "go-study-mcp", studioDoc)
	if err != nil {
		t.Fatalf("ReadResource(%q): %v", studioDoc, err)
	}
	if got.Artifact != nil {
		t.Fatalf("studio App document was offloaded to an artifactRef (%+v) — it must ride inline so it renders on the in-memory store", got.Artifact)
	}
	if len(got.Inline) <= 32*1024 {
		t.Fatalf("studio App document inline size = %d bytes — expected a real (>32 KiB) document above the heavy threshold", len(got.Inline))
	}
	t.Logf("real server returned the studio App document inline: %d bytes (above the 32 KiB heavy threshold, below the 2 MiB App cap)", len(got.Inline))
}
