package serve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestMCPConnectionAttacher_OAuthBindingError_FailsLoudNotParked is the
// regression for the silent-park bug: a runtime-added connection whose
// oauth_provider does not resolve (unknown provider, missing/mismatched
// downstream allow-list, stdio/no-url, static-Authorization conflict) is a
// fail-loud CONFIG error — it must NOT be misclassified as auth-required and
// parked. The binding-error message carries "oauth_provider", which the
// looksLikeAuthRequired string heuristic matches on the "oauth" marker; the
// typed-sentinel guard must take precedence so the operator sees the real
// diagnostic (e.g. "unknown provider") instead of a silent auth_required park.
func TestMCPConnectionAttacher_OAuthBindingError_FailsLoudNotParked(t *testing.T) {
	bus := mkDriverTestBus(t, auditpatterns.New())
	// No OAuth providers registered → the named provider is unknown, so
	// resolveOAuthBinding returns a loud mcpdrv.ErrOAuthBinding before any dial.
	a := NewMCPConnectionAttacher(tools.NewCatalog(), mcpdrv.NewRegistry(), bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, nil, nil, nil)
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		Identity:      identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, // full owner
		AgentID:       "agent-1",
		Name:          "mem",
		Transport:     agentcfg.MCPTransportHTTP,
		URL:           "https://mem.example.com/mcp",
		OAuthProvider: "nonexistent-provider",
	})
	if err == nil {
		t.Fatal("attach with an unresolvable oauth_provider must fail loud")
	}
	if !errors.Is(err, mcpdrv.ErrOAuthBinding) {
		t.Fatalf("want a loud ErrOAuthBinding surfacing the real diagnostic, got: %v", err)
	}
	if errors.Is(err, agentcfgprotocol.ErrAuthRequired) {
		t.Fatalf("a binding/config error must NOT be misclassified as auth-required (silent park): %v", err)
	}
}
