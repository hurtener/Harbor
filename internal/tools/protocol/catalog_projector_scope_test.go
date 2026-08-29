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

type logicalSourceResolver map[tools.ToolSourceID]string

func (r logicalSourceResolver) LogicalNameOfSource(source tools.ToolSourceID) (string, bool) {
	name, ok := r[source]
	return name, ok
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

// TestCatalogProjector_ProjectsLogicalOwnerPerUserView proves the catalog
// projector translates only the internal MCP source owner. Each user gets an
// identity-scoped view containing its own physical source; both rows project
// the same logical Owner, while their physical ID and Name remain the stable
// catalog keys needed by get/describe/dispatch. An unmapped source retains
// its raw owner as the honest fallback for partially assembled embedders.
func TestCatalogProjector_ProjectsLogicalOwnerPerUserView(t *testing.T) {
	const (
		physicalA = "shared~u-a_echo"
		physicalB = "shared~u-b_echo"
		raw       = "legacy-source_echo"
		nonMCP    = "local-source_collision"
		logical   = "shared"
	)
	physicalSourceA := tools.ToolSourceID("shared~u-a")
	physicalSourceB := tools.ToolSourceID("shared~u-b")
	rawSource := tools.ToolSourceID("legacy-source")
	row := func(name string, source tools.ToolSourceID) tools.Tool {
		return tools.Tool{
			Name:      name,
			Source:    source,
			Transport: tools.TransportMCP,
			Loading:   tools.LoadingAlways,
		}
	}
	nonMCPRow := tools.Tool{
		Name:      nonMCP,
		Source:    physicalSourceA,
		Transport: tools.TransportInProcess,
		Loading:   tools.LoadingAlways,
	}
	views := map[string]tools.PlannerCatalogView{
		"user-a": scopedPlannerView{rows: []tools.Tool{
			row(physicalA, physicalSourceA), row(raw, rawSource), nonMCPRow,
		}},
		"user-b": scopedPlannerView{rows: []tools.Tool{
			row(physicalB, physicalSourceB), row(raw, rawSource), nonMCPRow,
		}},
	}
	resolver := scopedPlannerViewResolver(func(_ context.Context, id identity.Identity, _ string) (tools.PlannerCatalogView, error) {
		return views[id.UserID], nil
	})
	logicalNames := logicalSourceResolver{
		physicalSourceA: logical,
		physicalSourceB: logical,
	}
	projector, err := toolsprotocol.NewCatalogProjector(
		newTestCatalog(t),
		toolsprotocol.WithCatalogViewResolver(resolver),
		toolsprotocol.WithLogicalSourceResolver(logicalNames),
	)
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	service, err := toolsprotocol.NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	for _, tc := range []struct {
		user         string
		physicalTool string
	}{
		{user: "user-a", physicalTool: physicalA},
		{user: "user-b", physicalTool: physicalB},
	} {
		t.Run(tc.user, func(t *testing.T) {
			resp, err := service.List(context.Background(), prototypes.ToolListRequest{
				Identity: prototypes.IdentityScope{Tenant: "tenant", User: tc.user, Session: "session-" + tc.user},
			})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			rows := make(map[string]prototypes.Tool, len(resp.Tools))
			for _, projected := range resp.Tools {
				rows[projected.ID] = projected
			}
			own, ok := rows[tc.physicalTool]
			if !ok {
				t.Fatalf("projected rows missing own physical tool %q: %v", tc.physicalTool, rows)
			}
			if own.ID != tc.physicalTool || own.Name != tc.physicalTool {
				t.Fatalf("physical catalog identity changed: ID=%q Name=%q want %q", own.ID, own.Name, tc.physicalTool)
			}
			if own.Owner != logical {
				t.Fatalf("own Owner = %q, want logical source %q", own.Owner, logical)
			}
			fallback, ok := rows[raw]
			if !ok {
				t.Fatalf("projected rows missing raw fallback tool %q: %v", raw, rows)
			}
			if fallback.Owner != string(rawSource) {
				t.Fatalf("unmapped Owner = %q, want raw source %q", fallback.Owner, rawSource)
			}
			collision, ok := rows[nonMCP]
			if !ok {
				t.Fatalf("projected rows missing non-MCP collision tool %q: %v", nonMCP, rows)
			}
			if collision.Owner != string(physicalSourceA) {
				t.Fatalf("non-MCP Owner = %q, want raw source %q despite resolver collision", collision.Owner, physicalSourceA)
			}
			for id := range rows {
				if tc.user == "user-a" && id == physicalB || tc.user == "user-b" && id == physicalA {
					t.Fatalf("foreign physical source %q escaped the scoped view", id)
				}
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
