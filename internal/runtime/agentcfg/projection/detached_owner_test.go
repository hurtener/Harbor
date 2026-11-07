package projection_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// mutableOwners is a source-owner resolver whose registrations can be
// removed mid-test, standing in for a registry whose connection is detached
// after a view captured the catalog.
type mutableOwners struct {
	mu     sync.Mutex
	owners map[tools.ToolSourceID]toolauth.Owner
}

func (m *mutableOwners) OwnerOfSource(source tools.ToolSourceID) (toolauth.Owner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	owner, ok := m.owners[source]
	return owner, ok
}

func (m *mutableOwners) detach(source tools.ToolSourceID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.owners, source)
}

// TestUserScopedView_DetachedMCPSourceFailsClosed is the SECURITY regression
// for unknown MCP ownership: a foreign connection's tool still present in
// the catalog after its registry entry is gone (detach in progress) must
// not be admitted as "shared". The sequence is deterministic: build the
// view, detach the owner, then ask the view.
func TestUserScopedView_DetachedMCPSourceFailsClosed(t *testing.T) {
	ctx := context.Background()
	cat := tools.NewCatalog()
	foreign := tools.ToolSourceID("srv~u-foreign")
	mcpTool := foreign + "_secret_op"
	inproc := "local_helper"
	for _, tl := range []tools.Tool{
		{Name: string(mcpTool), Description: "foreign server op", Source: foreign, Transport: tools.TransportMCP, Loading: tools.LoadingDeferred},
		{Name: inproc, Description: "in-process helper", Transport: tools.TransportInProcess},
	} {
		if err := cat.Register(tools.ToolDescriptor{Tool: tl, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	owners := &mutableOwners{owners: map[tools.ToolSourceID]toolauth.Owner{
		foreign: {Tenant: projTenant, Agent: projAgent, User: "someone-else"},
	}}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: "me", SessionID: "s"}}
	view, err := projection.ActivePlannerCatalogView(ctx, nil, nil, projAgent, id, cat, tools.CatalogFilter{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
	}, owners)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	check := func(stage string) {
		t.Helper()
		if _, ok := view.Resolve(string(mcpTool)); ok {
			t.Fatalf("%s: foreign MCP tool resolves", stage)
		}
		discovered, _ := tools.DiscoveryToolsOf(view)
		for _, tl := range discovered {
			if tl.Name == string(mcpTool) {
				t.Fatalf("%s: foreign MCP tool in discovery set", stage)
			}
		}
		if names := tools.DiscoveryNames(view); len(names) != 1 || names[0] != inproc {
			t.Fatalf("%s: discovery names = %v, want only the unowned in-process tool", stage, names)
		}
	}
	check("owned by another user")
	owners.detach(foreign) // registry entry removed; catalog row not yet deregistered
	check("owner unknown after detach")
}
