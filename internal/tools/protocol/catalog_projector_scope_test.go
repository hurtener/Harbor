package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// scopedPlannerView is a small read-only view used to prove that the
// protocol projector consumes the per-request effective catalog view rather
// than re-reading the process-global catalog directly. Production wiring
// supplies projection.ActivePlannerCatalogView through the same seam.
type scopedPlannerView struct {
	rows []tools.Tool
}

func (v scopedPlannerView) Resolve(name string) (tools.Tool, bool) {
	for _, row := range v.rows {
		if row.Name == name {
			return row, true
		}
	}
	return tools.Tool{}, false
}

func (v scopedPlannerView) List() []tools.Tool {
	return append([]tools.Tool(nil), v.rows...)
}

type scopedPlannerViewResolver func(context.Context, identity.Identity, string) (tools.PlannerCatalogView, error)

func (f scopedPlannerViewResolver) CatalogView(ctx context.Context, id identity.Identity, agentID string) (tools.PlannerCatalogView, error) {
	return f(ctx, id, agentID)
}

// TestCatalogProjector_UsesEffectiveViewPerUser pins the identity boundary at
// the tools Protocol projector. A shared catalog contains both users'
// physical MCP source names, but each request must be projected through the
// acting user's effective view. In particular, tools.get/describe/metrics
// must not resolve a foreign physical tool merely because it exists in the
// process-global catalog.
func TestCatalogProjector_UsesEffectiveViewPerUser(t *testing.T) {
	cat := newTestCatalog(t)
	physicalA := "shared~u-a_echo"
	physicalB := "shared~u-b_echo"
	for _, name := range []string{physicalA, physicalB} {
		if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{
			Name: name, Description: name + " schema", Source: tools.ToolSourceID(name[:len(name)-len("_echo")]),
			Transport: tools.TransportMCP, Loading: tools.LoadingAlways,
		}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		}}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	local := tools.Tool{Name: "alpha_search", Description: "local"}
	viewFor := func(user string) tools.PlannerCatalogView {
		own := physicalA
		if user == "user-b" {
			own = physicalB
		}
		return scopedPlannerView{rows: []tools.Tool{local, mustCatalogTool(t, cat, own)}}
	}
	resolver := scopedPlannerViewResolver(func(_ context.Context, id identity.Identity, _ string) (tools.PlannerCatalogView, error) {
		return viewFor(id.UserID), nil
	})
	projector, err := toolsprotocol.NewCatalogProjector(cat, toolsprotocol.WithCatalogViewResolver(resolver))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	service, err := toolsprotocol.NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	identityFor := func(user string) prototypes.IdentityScope {
		return prototypes.IdentityScope{Tenant: "tenant", User: user, Session: "session-" + user}
	}
	for _, tc := range []struct {
		user    string
		own     string
		foreign string
	}{
		{user: "user-a", own: physicalA, foreign: physicalB},
		{user: "user-b", own: physicalB, foreign: physicalA},
	} {
		t.Run(tc.user, func(t *testing.T) {
			id := identityFor(tc.user)
			list, err := service.List(context.Background(), prototypes.ToolListRequest{Identity: id})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list.Tools) != 2 {
				t.Fatalf("List returned %d rows, want local + own source: %+v", len(list.Tools), list.Tools)
			}
			if _, err := service.Get(context.Background(), prototypes.ToolGetRequest{Identity: id, ID: tc.own}); err != nil {
				t.Fatalf("Get own tool: %v", err)
			}
			if _, err := service.Get(context.Background(), prototypes.ToolGetRequest{Identity: id, ID: tc.foreign}); !errors.Is(err, toolsprotocol.ErrToolNotFound) {
				t.Fatalf("Get foreign tool error = %v, want ErrToolNotFound", err)
			}
			if _, err := service.Describe(context.Background(), prototypes.ToolDescribeRequest{Identity: id, ID: tc.foreign, AgentID: "agent-1"}); !errors.Is(err, toolsprotocol.ErrToolNotFound) {
				t.Fatalf("Describe foreign tool error = %v, want ErrToolNotFound", err)
			}
			if _, err := service.Metrics(context.Background(), prototypes.ToolMetricsRequest{Identity: id, ID: tc.foreign}); !errors.Is(err, toolsprotocol.ErrToolNotFound) {
				t.Fatalf("Metrics foreign tool error = %v, want ErrToolNotFound", err)
			}
		})
	}
}

func mustCatalogTool(t *testing.T, cat tools.ToolCatalog, name string) tools.Tool {
	t.Helper()
	desc, ok := cat.Resolve(name)
	if !ok {
		t.Fatalf("catalog missing %q", name)
	}
	return desc.Tool
}
