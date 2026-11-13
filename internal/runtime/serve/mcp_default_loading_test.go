package serve

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestMCPConnectionAttacher_DefaultLoading_AddPathAndOverride drives the
// add-connection door (PrepareConnection → Activate) with the deployment
// default `deferred`, then proves the agent-config `tool_loading_modes`
// override pins the tool back to `always` in the run's planner projection.
func TestMCPConnectionAttacher_DefaultLoading_AddPathAndOverride(t *testing.T) {
	const name, agentID = "reports", "agent-1"
	fixture := appToolFixtureServer(t)
	cat := tools.NewCatalog()
	bus := mkDriverTestBus(t, auditpatterns.New())
	a := NewMCPConnectionAttacher(cat, mcpdrv.NewRegistry(), bus,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Identity{TenantID: "sys", UserID: "sys", SessionID: "sys"}, nil, nil, nil,
		WithDefaultLoading(tools.LoadingDeferred))
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	prepared, err := a.PrepareConnection(ctx, agentcfgprotocol.AttachRequest{
		Identity: id, AgentID: agentID, Name: name, Transport: agentcfg.MCPTransportHTTP, URL: fixture.URL,
	})
	if err != nil {
		t.Fatalf("PrepareConnection: %v", err)
	}
	if err := prepared.Activate(ctx); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	physical := mcpdrv.PhysicalServerName(name, toolauth.Owner{Tenant: "t", Agent: agentID}) + "_report"
	d, ok := cat.Resolve(physical)
	if !ok {
		t.Fatalf("catalog missing %s", physical)
	}
	if d.Tool.Loading != tools.LoadingDeferred {
		t.Fatalf("add path stamped loading=%q, want deferred", d.Tool.Loading)
	}

	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: id, RunID: "run-1"}
	plannerNames := func() map[string]tools.LoadingMode {
		view, err := projection.ActivePlannerCatalogView(ctx, reg, nil, agentID, q, cat, tools.CatalogFilter{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		}, a.registry)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]tools.LoadingMode{}
		for _, tool := range view.List() {
			out[tool.Name] = tool.Loading
		}
		return out
	}
	base := agentcfg.ConfigPayload{
		Connections:  &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: name, Transport: agentcfg.MCPTransportHTTP, URL: fixture.URL}}},
		ToolExposure: &agentcfg.ToolExposure{},
	}
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, base, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, listed := plannerNames()[physical]; listed {
		t.Fatal("deferred-by-default tool is declared on every call")
	}
	base.ToolExposure = &agentcfg.ToolExposure{ToolLoadingModes: map[string]string{name + "_report": "always"}}
	if _, err := reg.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, base, agentcfg.SetOptions{}); err != nil {
		t.Fatal(err)
	}
	if mode, listed := plannerNames()[physical]; !listed || mode != tools.LoadingAlways {
		t.Fatalf("tool_loading_modes always override not applied: listed=%v mode=%q", listed, mode)
	}
}
