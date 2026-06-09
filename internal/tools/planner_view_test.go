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
