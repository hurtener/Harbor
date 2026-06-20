package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func plannerViewTestCatalog(t *testing.T) ToolCatalog {
	t.Helper()
	cat := NewCatalog()
	reg := func(tool Tool) {
		t.Helper()
		if err := cat.Register(ToolDescriptor{
			Tool: tool,
			Invoke: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
				return ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", tool.Name, err)
		}
	}
	reg(Tool{Name: "open_tool"})
	reg(Tool{Name: "scoped_tool", AuthScopes: []string{"scope:a"}})
	reg(Tool{Name: "two_scope_tool", AuthScopes: []string{"scope:a", "scope:b"}})
	return cat
}

// exclusionViewCatalog registers two MCP tools sharing one source plus a
// standalone tool, for the ExclusionView projection tests.
func exclusionViewCatalog(t *testing.T) ToolCatalog {
	t.Helper()
	cat := NewCatalog()
	reg := func(tool Tool) {
		t.Helper()
		if err := cat.Register(ToolDescriptor{
			Tool: tool,
			Invoke: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
				return ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", tool.Name, err)
		}
	}
	reg(Tool{Name: "srvA_alpha", Source: "srvA", Transport: TransportMCP})
	reg(Tool{Name: "srvA_beta", Source: "srvA", Transport: TransportMCP})
	reg(Tool{Name: "srvB_gamma", Source: "srvB", Transport: TransportMCP})
	reg(Tool{Name: "local_tool"})
	return cat
}

func exclusionViewNames(v PlannerCatalogView) map[string]bool {
	out := map[string]bool{}
	for _, tool := range v.List() {
		out[tool.Name] = true
	}
	return out
}

// TestExclusionView_PausedServerHidesAllItsTools — every tool sharing a
// paused server's source id is hidden from BOTH List and Resolve; other
// servers' tools and standalone tools stay visible.
func TestExclusionView_PausedServerHidesAllItsTools(t *testing.T) {
	t.Parallel()
	cat := exclusionViewCatalog(t)
	base := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	v := NewExclusionView(base, []string{"srvA"}, nil)
	names := exclusionViewNames(v)
	if names["srvA_alpha"] || names["srvA_beta"] {
		t.Errorf("paused server srvA tools still visible: %v", names)
	}
	if !names["srvB_gamma"] || !names["local_tool"] {
		t.Errorf("non-paused tools missing: %v", names)
	}
	if _, ok := v.Resolve("srvA_alpha"); ok {
		t.Error("Resolve(srvA_alpha) succeeded against a paused server, want found=false")
	}
	if _, ok := v.Resolve("srvB_gamma"); !ok {
		t.Error("Resolve(srvB_gamma) failed for a non-paused tool")
	}
}

// TestExclusionView_DisabledToolHiddenByName — an individually-disabled
// tool is hidden while its siblings on the same server stay visible.
func TestExclusionView_DisabledToolHiddenByName(t *testing.T) {
	t.Parallel()
	cat := exclusionViewCatalog(t)
	base := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	v := NewExclusionView(base, nil, []string{"srvA_alpha"})
	names := exclusionViewNames(v)
	if names["srvA_alpha"] {
		t.Errorf("disabled tool srvA_alpha still visible: %v", names)
	}
	if !names["srvA_beta"] {
		t.Errorf("sibling srvA_beta wrongly hidden: %v", names)
	}
	if _, ok := v.Resolve("srvA_alpha"); ok {
		t.Error("Resolve(srvA_alpha) succeeded for a disabled tool, want found=false")
	}
}

// TestExclusionView_EmptySets_PassThrough — a view with empty exclusion
// sets wraps the inner view faithfully (hides nothing).
func TestExclusionView_EmptySets_PassThrough(t *testing.T) {
	t.Parallel()
	cat := exclusionViewCatalog(t)
	base := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	v := NewExclusionView(base, nil, nil)
	if got, want := len(v.List()), len(base.List()); got != want {
		t.Errorf("empty-set ExclusionView hid tools: got %d want %d", got, want)
	}
}

// TestPlannerView_EmptyGranted_FiltersScopedTools — the empty-granted
// rule (Phase 83m / D-156, preserved by the 110a promotion): with no
// granted scopes, tools WITHOUT AuthScopes are visible and tools WITH
// AuthScopes are invisible.
func TestPlannerView_EmptyGranted_FiltersScopedTools(t *testing.T) {
	t.Parallel()
	cat := plannerViewTestCatalog(t)
	v := NewPlannerView(cat, CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
	})
	listed := v.List()
	if len(listed) != 1 || listed[0].Name != "open_tool" {
		t.Errorf("empty-granted List = %+v, want only open_tool", listed)
	}
}

// TestPlannerView_GrantedSubsetRule — tools whose AuthScopes are
// entirely contained in the granted set are visible; tools requiring a
// missing scope are filtered out.
func TestPlannerView_GrantedSubsetRule(t *testing.T) {
	t.Parallel()
	cat := plannerViewTestCatalog(t)
	v := NewPlannerView(cat, CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
		GrantedScopes: []string{"scope:a"},
	})
	names := map[string]bool{}
	for _, tool := range v.List() {
		names[tool.Name] = true
	}
	if !names["open_tool"] || !names["scoped_tool"] {
		t.Errorf("List = %v, want open_tool + scoped_tool visible", names)
	}
	if names["two_scope_tool"] {
		t.Errorf("two_scope_tool visible with only scope:a granted, want filtered")
	}
}

// TestPlannerView_Resolve_ReturnsSchemaOnlyTool — Resolve returns the
// schema-only Tool value (the descriptor's Tool), with found=false for
// unknown names.
func TestPlannerView_Resolve_ReturnsSchemaOnlyTool(t *testing.T) {
	t.Parallel()
	cat := plannerViewTestCatalog(t)
	v := NewPlannerView(cat, CatalogFilter{TenantID: "t", UserID: "u", SessionID: "s"})
	tool, ok := v.Resolve("open_tool")
	if !ok || tool.Name != "open_tool" {
		t.Errorf("Resolve(open_tool) = (%+v, %v), want the schema Tool", tool, ok)
	}
	if _, ok := v.Resolve("missing"); ok {
		t.Error("Resolve(missing) = ok, want found=false")
	}
}

// TestPlannerView_CopiesGrantedScopes — the constructor copies the
// GrantedScopes slice so a caller mutating its backing array after
// construction cannot change the view's visibility (D-025 immutability).
func TestPlannerView_CopiesGrantedScopes(t *testing.T) {
	t.Parallel()
	cat := plannerViewTestCatalog(t)
	granted := []string{"scope:a"}
	v := NewPlannerView(cat, CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
		GrantedScopes: granted,
	})
	granted[0] = "scope:mutated"
	names := map[string]bool{}
	for _, tool := range v.List() {
		names[tool.Name] = true
	}
	if !names["scoped_tool"] {
		t.Errorf("caller mutation leaked into the view: List = %v", names)
	}
}
