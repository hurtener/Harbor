package serve

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// defaultIdentityRejection is the stable message the MCP provider raises when
// its DefaultIdentity is not fully populated — the failure a runtime-added
// connection hit on every SDK-facade embedder before v1.17.1.
const defaultIdentityRejection = "DefaultIdentity must be fully populated"

// TestResolveMCPAttachIdentity_FillsEmptyWithDefault: the SDK facade sets no
// MCPDefaultIdentity, so serve.Boot must fill the empty value with the
// fully-populated assemble default before wiring the runtime-add attacher.
func TestResolveMCPAttachIdentity_FillsEmptyWithDefault(t *testing.T) {
	got := resolveMCPAttachIdentity(identity.Identity{})
	if got != assemble.DefaultMCPIdentity {
		t.Fatalf("empty identity: got %+v, want the assemble default %+v", got, assemble.DefaultMCPIdentity)
	}
	if got.TenantID == "" || got.UserID == "" || got.SessionID == "" {
		t.Fatalf("the fallback must be fully populated (tenant/user/session), got %+v", got)
	}
}

// TestResolveMCPAttachIdentity_PassesThroughConfigured: a caller that DOES set
// a service identity (harbor serve / dev compose / devstack) keeps it verbatim.
func TestResolveMCPAttachIdentity_PassesThroughConfigured(t *testing.T) {
	want := identity.Identity{TenantID: "acme", UserID: "svc", SessionID: "sess-1"}
	if got := resolveMCPAttachIdentity(want); got != want {
		t.Fatalf("configured identity must pass through unchanged: got %+v, want %+v", got, want)
	}
}

// TestMCPConnectionAttacher_EmptyDefaultIdentity_RejectedAtProviderBuild is the
// regression itself: an attacher built with an EMPTY default identity (the
// pre-v1.17.1 SDK-facade state) rejects a runtime-added connection at provider
// construction — it never reaches the dial, so a compiled SDK server could
// accept no runtime-added MCP connection.
func TestMCPConnectionAttacher_EmptyDefaultIdentity_RejectedAtProviderBuild(t *testing.T) {
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(tools.NewCatalog(), mcpdrv.NewRegistry(), bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{}, nil, nil, nil) // empty default identity — the bug
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, // full owner
		AgentID:   "agent-1",
		Name:      "mem",
		Transport: agentcfg.MCPTransportHTTP,
		URL:       "http://127.0.0.1:1/mcp", // nothing listens on port 1
	})
	if err == nil {
		t.Fatal("attach with an empty default identity must fail (provider rejects it)")
	}
	if !strings.Contains(err.Error(), defaultIdentityRejection) {
		t.Fatalf("empty default identity must be rejected with %q, got: %v", defaultIdentityRejection, err)
	}
}

// TestMCPConnectionAttacher_ResolvedDefaultIdentity_ReachesDial proves the fix:
// building the attacher with the resolved fallback (exactly what serve.Boot now
// passes for an unset MCPDefaultIdentity) gets PAST provider construction to the
// real dial — the connection is only refused because nothing is listening.
func TestMCPConnectionAttacher_ResolvedDefaultIdentity_ReachesDial(t *testing.T) {
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(tools.NewCatalog(), mcpdrv.NewRegistry(), bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		resolveMCPAttachIdentity(identity.Identity{}), nil, nil, nil) // the fix
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Attach(ctx, agentcfgprotocol.AttachRequest{
		Identity:  identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
		AgentID:   "agent-1",
		Name:      "mem",
		Transport: agentcfg.MCPTransportHTTP,
		URL:       "http://127.0.0.1:1/mcp",
	})
	if err == nil {
		t.Fatal("attach to an unreachable endpoint must still fail (at the dial)")
	}
	if strings.Contains(err.Error(), defaultIdentityRejection) {
		t.Fatalf("with the resolved fallback the provider must build (reach the dial), not be rejected for identity: %v", err)
	}
}
