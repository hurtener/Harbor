package tools

import (
	"context"
	"encoding/json"
	"sync"
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

// loadingOverrideTestCatalog registers a mix of forms/sources/boot-loading
// modes for the LoadingOverrideView tests: two TOOL-form MCP descriptors on
// srvA (one boot-always, one boot-deferred), one RESOURCE-form MCP
// descriptor on srvA (boot-deferred, per the driver default), and one
// in-process tool (Source empty, Form zero-value, boot-always).
func loadingOverrideTestCatalog(t *testing.T) ToolCatalog {
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
	reg(Tool{Name: "srvA_alpha", Source: "srvA", Transport: TransportMCP, Loading: LoadingAlways})
	reg(Tool{Name: "srvA_beta", Source: "srvA", Transport: TransportMCP, Loading: LoadingDeferred})
	reg(Tool{Name: "srvA_res", Source: "srvA", Transport: TransportMCP, Loading: LoadingDeferred, Form: ToolFormResource})
	reg(Tool{Name: "local_tool", Loading: LoadingAlways})
	return cat
}

func loadingOverrideBothModesView(t *testing.T, cat ToolCatalog) PlannerView {
	t.Helper()
	return NewPlannerView(cat, CatalogFilter{
		TenantID: "t", UserID: "u", SessionID: "s",
		LoadingModes: []LoadingMode{LoadingAlways, LoadingDeferred},
	})
}

// TestLoadingOverrideView_RewritesAndFiltersList proves List() filters on the
// EFFECTIVE mode (not the boot mode) and rewrites Tool.Loading on every
// returned entry.
func TestLoadingOverrideView_RewritesAndFiltersList(t *testing.T) {
	t.Parallel()
	cat := loadingOverrideTestCatalog(t)
	inner := loadingOverrideBothModesView(t, cat)
	effective := map[string]LoadingMode{
		"srvA_alpha": LoadingDeferred, // boot-always -> effective deferred: must vanish from List()
		"srvA_beta":  LoadingAlways,   // boot-deferred -> effective always: must appear in List()
	}
	v := NewLoadingOverrideView(inner, effective, nil) // nil visibleModes -> default [Always]
	names := map[string]LoadingMode{}
	for _, tool := range v.List() {
		names[tool.Name] = tool.Loading
	}
	if _, present := names["srvA_alpha"]; present {
		t.Errorf("srvA_alpha overridden to deferred must be absent from List(), got %v", names)
	}
	if mode, present := names["srvA_beta"]; !present || mode != LoadingAlways {
		t.Errorf("srvA_beta overridden to always must appear in List() with Loading=always, got present=%v mode=%v", present, mode)
	}
	if mode, present := names["local_tool"]; !present || mode != LoadingAlways {
		t.Errorf("local_tool has no override and boots always; want present with Loading=always, got present=%v mode=%v", present, mode)
	}
	if _, present := names["srvA_res"]; present {
		t.Errorf("srvA_res has no override and boots deferred; must stay absent from the default Always-only List()")
	}
}

// TestLoadingOverrideView_ResolveNeverFiltersButRewrites proves Resolve()
// returns a deferred-effective tool (the D-167 two-turn discovery cycle
// depends on this) while still rewriting Loading to the effective mode.
func TestLoadingOverrideView_ResolveNeverFiltersButRewrites(t *testing.T) {
	t.Parallel()
	cat := loadingOverrideTestCatalog(t)
	inner := loadingOverrideBothModesView(t, cat)
	effective := map[string]LoadingMode{"srvA_alpha": LoadingDeferred}
	v := NewLoadingOverrideView(inner, effective, nil)

	tool, ok := v.Resolve("srvA_alpha")
	if !ok {
		t.Fatal("Resolve must never filter on loading — srvA_alpha should resolve despite the deferred override")
	}
	if tool.Loading != LoadingDeferred {
		t.Errorf("Resolve must rewrite Loading to the effective mode, got %v", tool.Loading)
	}

	// A tool absent from `effective` resolves with its boot-effective mode
	// unchanged.
	tool2, ok := v.Resolve("srvA_res")
	if !ok || tool2.Loading != LoadingDeferred {
		t.Errorf("Resolve(srvA_res) = (%+v, %v), want boot-effective deferred, found", tool2, ok)
	}

	if _, ok := v.Resolve("missing"); ok {
		t.Error("Resolve(missing) = ok, want found=false")
	}
}

// TestLoadingOverrideView_DefaultVisibleModes proves an empty visibleModes
// slice defaults to [LoadingAlways], mirroring CatalogFilter's default.
func TestLoadingOverrideView_DefaultVisibleModes(t *testing.T) {
	t.Parallel()
	cat := loadingOverrideTestCatalog(t)
	inner := loadingOverrideBothModesView(t, cat)
	v := NewLoadingOverrideView(inner, nil, nil)
	names := exclusionViewNames(v)
	if names["srvA_beta"] || names["srvA_res"] {
		t.Errorf("default visible modes must exclude boot-deferred tools, got %v", names)
	}
	if !names["srvA_alpha"] || !names["local_tool"] {
		t.Errorf("default visible modes must include boot-always tools, got %v", names)
	}
}

// TestLoadingOverrideView_CopiesMaps proves the constructor copies both the
// effective map and visibleModes so a caller mutation after construction
// cannot change the view's behaviour (D-025 immutability, matching the
// PlannerView GrantedScopes precedent).
func TestLoadingOverrideView_CopiesMaps(t *testing.T) {
	t.Parallel()
	cat := loadingOverrideTestCatalog(t)
	inner := loadingOverrideBothModesView(t, cat)
	effective := map[string]LoadingMode{"srvA_beta": LoadingAlways}
	visible := []LoadingMode{LoadingAlways}
	v := NewLoadingOverrideView(inner, effective, visible)
	effective["srvA_beta"] = LoadingDeferred
	visible[0] = LoadingDeferred

	names := exclusionViewNames(v)
	if !names["srvA_beta"] {
		t.Errorf("caller mutation of the effective map leaked into the view: %v", names)
	}
}

// TestLoadingOverrideView_ConcurrentReuse runs N=100 concurrent List/Resolve
// invocations against ONE shared LoadingOverrideView instance, asserting no
// data race (the -race gate) and no torn/inconsistent read — the D-025
// concurrent-reuse contract for this per-run value-type view.
func TestLoadingOverrideView_ConcurrentReuse(t *testing.T) {
	cat := loadingOverrideTestCatalog(t)
	inner := loadingOverrideBothModesView(t, cat)
	effective := map[string]LoadingMode{"srvA_alpha": LoadingDeferred, "srvA_beta": LoadingAlways}
	v := NewLoadingOverrideView(inner, effective, nil)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			names := exclusionViewNames(v)
			if names["srvA_alpha"] {
				t.Errorf("srvA_alpha must stay deferred-excluded under concurrent reads")
			}
			if !names["srvA_beta"] {
				t.Errorf("srvA_beta must stay always-included under concurrent reads")
			}
			tool, ok := v.Resolve("srvA_alpha")
			if !ok || tool.Loading != LoadingDeferred {
				t.Errorf("Resolve(srvA_alpha) = (%+v, %v), want deferred, found", tool, ok)
			}
		}()
	}
	wg.Wait()
}
