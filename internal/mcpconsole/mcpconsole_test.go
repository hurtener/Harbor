package mcpconsole_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// stubProvider is a deterministic mcp provider — no MCP wire.
type stubProvider struct{ id tools.ToolSourceID }

func (p *stubProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *stubProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return []tools.ToolDescriptor{
		{Tool: tools.Tool{Name: string(p.id) + ".tool-a"}},
		{Tool: tools.Tool{Name: string(p.id) + "__resource.repo://x"}},
	}, nil
}

func idCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t-1", UserID: "u-1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestRegistryAccessor_NilRejected(t *testing.T) {
	if _, err := mcpconsole.NewRegistryAccessor(nil); err == nil {
		t.Fatal("want error for nil registry")
	}
}

func TestRegistryAccessor_ListAndGet(t *testing.T) {
	reg := mcp.NewRegistry(mcp.WithRegistryClock(func() time.Time {
		return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	}))
	if err := reg.Register(mcp.ServerRegistration{
		Provider:     &stubProvider{id: "srv-a"},
		Transport:    "http+sse",
		InitialState: mcp.ServerStateOnline,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acc, err := mcpconsole.NewRegistryAccessor(reg)
	if err != nil {
		t.Fatalf("NewRegistryAccessor: %v", err)
	}

	rows, next, err := acc.ListServers(idCtx(t), protocol.MCPListFilter{})
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "srv-a" || next != "" {
		t.Fatalf("ListServers shape wrong: %v next=%q", rows, next)
	}

	row, err := acc.GetServer(idCtx(t), "srv-a")
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if row.Transport != "http+sse" {
		t.Fatalf("GetServer shape wrong: %+v", row)
	}

	res, err := acc.ListResources(idCtx(t), "srv-a")
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(res) != 1 || res[0].URI != "repo://x" {
		t.Fatalf("ListResources shape wrong: %v", res)
	}

	disc, err := acc.RefreshDiscovery(idCtx(t), "srv-a")
	if err != nil {
		t.Fatalf("RefreshDiscovery: %v", err)
	}
	if disc.DiscoveryID == "" {
		t.Fatalf("want a discovery id")
	}

	prev, err := acc.SetRawHTMLTrust(idCtx(t), "srv-a", true)
	if err != nil {
		t.Fatalf("SetRawHTMLTrust: %v", err)
	}
	if prev {
		t.Fatalf("want prior trust false")
	}
}

func TestOAuthAccessor_NilRejected(t *testing.T) {
	if _, err := mcpconsole.NewOAuthAccessor(nil); err == nil {
		t.Fatal("want error for nil provider")
	}
}

// Phase 83w F6 (D-164) — NoOAuthAccessor returns empty bindings and
// fails loudly on flow-initiation / revocation. The V1 dev posture
// uses this when no OAuth provider is wired so the read-only
// `mcp.servers.*` methods still serve real data while the OAuth verbs
// fail closed (CLAUDE.md §13 no silent degradation).
func TestNoOAuthAccessor_EmptyBindingsAndLoudFailure(t *testing.T) {
	ctx := context.Background()
	a := mcpconsole.NewNoOAuthAccessor()

	bindings, err := a.ListBindings(ctx, "any-server")
	if err != nil {
		t.Fatalf("ListBindings: unexpected error: %v", err)
	}
	if len(bindings) != 0 {
		t.Errorf("ListBindings: got %d bindings, want 0", len(bindings))
	}
	if bindings == nil {
		t.Error("ListBindings: returned nil slice; want empty (non-nil) slice for JSON [] rendering")
	}

	if _, _, err := a.InitiateBinding(ctx, "any-server", "subject"); err == nil {
		t.Error("InitiateBinding: expected ErrNoOAuthConfigured, got nil")
	} else if !errors.Is(err, mcpconsole.ErrNoOAuthConfigured) {
		t.Errorf("InitiateBinding: err=%v, want ErrNoOAuthConfigured", err)
	}

	if _, err := a.RevokeBinding(ctx, "any-server", "subject"); err == nil {
		t.Error("RevokeBinding: expected ErrNoOAuthConfigured, got nil")
	} else if !errors.Is(err, mcpconsole.ErrNoOAuthConfigured) {
		t.Errorf("RevokeBinding: err=%v, want ErrNoOAuthConfigured", err)
	}
}
