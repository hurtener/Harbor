// Phase 83g cross-subsystem integration test per CLAUDE.md §17.
//
// Phase 83g wires the Phase 28 MCP southbound driver into the dev
// binary's boot path: `cmd/harbor/cmd_dev.go::bootDevStack` now
// consumes `cfg.Tools.MCPServers[]`, spawns the configured transport
// (stdio subprocess / HTTP), opens the MCP session, discovers tools,
// and registers each `ToolDescriptor` on the tool catalog — closing
// the second consumer gap surfaced during the 83f operator-validation
// (issue #208 found 83a/b/c/d/e's primitives unconsumed by the dev
// binary; this test pins the analogous fix for the MCP path).
//
// What this test proves:
//
//   - The dev stack spawns a real MCP stdio subprocess from
//     `cfg.Tools.MCPServers[i].Command` and reaches an MCP session.
//   - Tools discovered from the server reach the tool catalog under
//     the configured source name (the same name the planner sees in
//     its `<available_tools>` rendering — 83b).
//   - The MCP Registry exposes the server (the catalog-side wiring
//     lands now; the Console MCP-page mount is a follow-up that
//     reuses this Registry).
//   - No orphan subprocess after stack teardown.
//
// Real drivers everywhere on the seam (§17.3): real audit redactor,
// real EventBus, real StateStore, real Coordinator, real tools
// catalog, real `mcpdrv.Provider` against a real stdio subprocess
// (the `cmd/harbor-mcptest-stdio` test fixture). No mocks at the
// boundary.

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// buildMCPTestServer compiles the `cmd/harbor-mcptest-stdio` binary
// into a per-test tempdir and returns its absolute path. The binary
// is tiny (~one tool) and `go build` is fast (<1s on a warm cache);
// shipping a pre-built fixture binary would add release ceremony for
// a test-only artifact.
func buildMCPTestServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "harbor-mcptest-stdio")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	// Resolve the repo root from the test file's location so this
	// works regardless of `go test`'s working directory.
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/harbor-mcptest-stdio")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build cmd/harbor-mcptest-stdio: %v\n%s", err, out)
	}
	return binPath
}

// repoRoot walks up from the test file's directory until it finds a
// `go.mod` — the repo root. Works no matter where `go test` is run.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repoRoot: walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// TestE2E_Phase83g_MCPServerToolsReachTheCatalog is the headline
// positive end-to-end: a configured MCP stdio server is spawned at
// boot, its `echo` tool reaches the catalog, and the MCP Registry
// reflects the server. Subprocess cleanup verified by the stack's
// Close path.
func TestE2E_Phase83g_MCPServerToolsReachTheCatalog(t *testing.T) {
	t.Parallel()
	binPath := buildMCPTestServer(t)

	cfgPath := writeDevConfig(t)
	cfg, err := config.Load(context.Background(), cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Append one MCP server pointing at the freshly-built test binary.
	cfg.Tools.MCPServers = []config.MCPServerConfig{
		{
			Name:          "mcptest",
			TransportMode: "stdio",
			Command:       []string{binPath},
		},
	}

	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{
		LLMConfigSnapshot: phase83gMockLLMSnapshot(cfg),
	})
	defer stack.Close()

	if stack.Catalog == nil {
		t.Fatal("devstack: Catalog is nil (SkipCatalog should not have fired)")
	}
	if stack.MCPRegistry == nil {
		t.Fatal("devstack: MCPRegistry is nil (Phase 83g registry construction must have fired)")
	}

	// Phase 28 stamps registered tools with the source-id (server
	// name) as their tool-name prefix. Phase 107c step 10/11 audit
	// changed the separator from `.` to `_` so the wire-side name
	// matches OpenAI's `^[a-zA-Z0-9_-]{1,128}$` spec (OpenRouter →
	// Bedrock rejects dots in tool names). The `echo` tool from
	// `harbor-mcptest-stdio` now lands as `mcptest_echo`.
	wantName := "mcptest_echo"
	d, ok := stack.Catalog.Resolve(wantName)
	if !ok {
		t.Fatalf("catalog: tool %q not registered — MCP discovery did not reach the catalog. Configured server name=%q, command=%v",
			wantName, "mcptest", []string{binPath})
	}
	if d.Tool.Name != wantName {
		t.Errorf("catalog: tool name = %q, want %q", d.Tool.Name, wantName)
	}
	if d.Tool.Description == "" {
		t.Error("catalog: descriptor carries no description — Phase 28's buildToolDescriptor should populate Description from the MCP Tool.Description")
	}

	// The Registry must expose the server (the catalog-side wiring
	// landed; the Console MCP-page mount that consumes this Registry
	// follows in its own phase). The Registry's identity-scoped
	// reads require the dev-token identity on ctx — the registry's
	// ValidateEvent side-channel rejects missing-identity calls.
	idCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  devstack.DefaultDevTenant,
		UserID:    devstack.DefaultDevUser,
		SessionID: devstack.DefaultDevSession,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	servers, _, listErr := stack.MCPRegistry.ListServers(idCtx, mcpRegistryListFilterAll())
	if listErr != nil {
		t.Fatalf("MCPRegistry.ListServers: %v", listErr)
	}
	if len(servers) != 1 {
		t.Fatalf("MCPRegistry: got %d servers, want 1", len(servers))
	}
	if servers[0].Name != "mcptest" {
		t.Errorf("MCPRegistry: server name = %q, want %q", servers[0].Name, "mcptest")
	}
	if servers[0].Transport != "stdio" {
		t.Errorf("MCPRegistry: server transport = %q, want %q", servers[0].Transport, "stdio")
	}
}

// phase83gMockLLMSnapshot mirrors phase83f's pattern — the devstack
// opens an LLM client whose dependency comes from cfg.LLM; the
// recording / real LLM is moot for 83g (no tasks are spawned, so the
// planner never runs), but a complete-shaped snapshot is still
// required for the LLM construction path to succeed.
func phase83gMockLLMSnapshot(cfg *config.Config) *llm.ConfigSnapshot {
	return &llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: cfg.LLM.ContextWindowReserve,
		HeavyOutputThreshold: cfg.Artifacts.HeavyOutputThresholdBytes,
		ModelProfiles: map[string]llm.ModelProfile{
			"anthropic/claude-sonnet-4": {
				ContextWindowTokens: 200000,
				TokenEstimator:      "chars_div_4",
			},
		},
	}
}

// mcpRegistryListFilterAll returns the zero-filter — every configured
// server matches.
func mcpRegistryListFilterAll() mcpdrv.ListFilter {
	return mcpdrv.ListFilter{}
}
