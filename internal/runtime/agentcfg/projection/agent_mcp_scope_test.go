package projection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func TestAgentMCPProjection_ConcurrentOwnersAndLegacyResume(t *testing.T) {
	reg := newRegistry(t)
	cat := tools.NewCatalog()
	resolver := testPhysicalSourceResolver{owners: map[tools.ToolSourceID]auth.Owner{}, logical: map[tools.ToolSourceID]string{}}
	owners := []auth.Owner{{Tenant: "tenant-a", Agent: "base"}, {Tenant: "tenant-b", Agent: "base"}, {Tenant: "tenant-a", Agent: "derived"}}
	for _, owner := range owners {
		q := identity.Quadruple{Identity: identity.Identity{TenantID: owner.Tenant, UserID: "setup", SessionID: "setup"}}
		descriptors := &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "same", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.test/mcp"}}}
		if _, err := reg.SetRevision(context.Background(), q, owner.Agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Connections: descriptors}, agentcfg.SetOptions{}); err != nil {
			t.Fatal(err)
		}
		source := tools.ToolSourceID(mcp.PhysicalServerName("same", owner))
		resolver.owners[source] = owner
		resolver.logical[source] = "same"
		if err := cat.Register(tools.ToolDescriptor{Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }, Tool: tools.Tool{Name: string(source) + "_echo", Source: source, Loading: tools.LoadingAlways}}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := owners[i%len(owners)]
			q := identity.Quadruple{Identity: identity.Identity{TenantID: owner.Tenant, UserID: fmt.Sprintf("u-%d", i), SessionID: fmt.Sprintf("s-%d", i)}}
			view, err := projection.ActivePlannerCatalogView(context.Background(), reg, nil, owner.Agent, q, cat, tools.CatalogFilter{}, resolver)
			if err != nil {
				t.Error(err)
				return
			}
			want := mcp.PhysicalServerName("same", owner) + "_echo"
			if list := view.List(); len(list) != 1 || list[0].Name != want {
				t.Errorf("catalog=%v want %s", list, want)
			}
			if got, ok := view.Resolve("same_echo"); !ok || got.Name != want {
				t.Errorf("legacy resumed alias=%v %v", got, ok)
			}
			for _, foreign := range owners {
				if foreign != owner {
					if _, ok := view.Resolve(mcp.PhysicalServerName("same", foreign) + "_echo"); ok {
						t.Error("foreign descriptor visible")
					}
				}
			}
		}(i)
	}
	wg.Wait()

	// Multiple physical rows for a legacy logical source do not select one
	// by iteration order. An exact boot id retains its original meaning.
	owner := owners[0]
	duplicate := tools.ToolSourceID("duplicate-physical")
	resolver.owners[duplicate] = owner
	resolver.logical[duplicate] = "same"
	add := func(name string, source tools.ToolSourceID) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: name, Source: source, Loading: tools.LoadingAlways}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) { return tools.ToolResult{}, nil }}); err != nil {
			t.Fatal(err)
		}
	}
	add("duplicate-physical_echo", duplicate)
	q := identity.Quadruple{Identity: identity.Identity{TenantID: owner.Tenant, UserID: "u", SessionID: "s"}}
	view, err := projection.ActivePlannerCatalogView(context.Background(), reg, nil, owner.Agent, q, cat, tools.CatalogFilter{}, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view.Resolve("same_echo"); ok {
		t.Fatal("ambiguous legacy alias admitted")
	}
	resolver.owners["same"] = auth.Owner{}
	add("same_echo", "same")
	if got, ok := view.Resolve("same_echo"); !ok || got.Source != "same" {
		t.Fatalf("exact boot precedence: %v %v", got, ok)
	}
}
