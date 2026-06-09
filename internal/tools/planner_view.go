package tools

// PlannerView is the planner-facing, schema-only projection of a
// [ToolCatalog] under one run's identity scope (Phase 110a — D-194;
// originally Phase 83i / D-152 as a `cmd/harbor` adapter).
//
// `planner.RunContext.Catalog` is the surface that renders into the
// `<available_tools>` prompt section. PlannerView wraps the production
// catalog + a per-run visibility filter (keyed on the run's identity
// triple + any GrantedScopes the operator declared). The planner sees
// the FILTERED set; the catalog's internal store is immutable from the
// planner's perspective (the planner only reads), and Resolve returns
// the schema-only [Tool] value — never the dispatch-side
// [ToolDescriptor].
//
// PlannerView satisfies `planner.ToolCatalogView` STRUCTURALLY
// (Resolve(name) (Tool, bool) + List() []Tool). This package cannot
// name that interface — `internal/planner` imports `internal/tools`
// (ToolCatalogView's methods return [Tool]) — so the compile-time
// assertion lives in `internal/planner`'s tests, where the import is
// legal.
//
// Per-run construction discipline (D-025): the view is a value type
// with two read-only fields. Each run constructs its own view via
// [NewPlannerView] — the filter depends on the run's identity. Sharing
// one view across runs would cross-contaminate visibility across
// tenants / users / sessions — DO NOT cache.
type PlannerView struct {
	cat    ToolCatalog
	filter CatalogFilter
}

// NewPlannerView constructs the per-run view over `cat` with
// `filter`'s visibility predicate. The filter's GrantedScopes is the
// operator-configured `tools.granted_scopes` list (Phase 83m / Item 6
// / D-156): tools whose declared AuthScopes are entirely contained in
// the granted set are visible; tools that require a missing scope are
// filtered out. An empty / nil GrantedScopes keeps the "no scopes
// granted" default — tools with AuthScopes are invisible to the
// planner; tools without AuthScopes are always visible (the standard
// [CatalogFilter] rule). The GrantedScopes slice is copied so the
// caller's backing array cannot mutate the view after construction.
func NewPlannerView(cat ToolCatalog, filter CatalogFilter) PlannerView {
	filter.GrantedScopes = append([]string(nil), filter.GrantedScopes...)
	return PlannerView{cat: cat, filter: filter}
}

// Resolve implements the planner's ToolCatalogView contract. Returns
// the schema-only Tool value the planner uses to build a CallTool
// decision — never the dispatch-side ToolDescriptor.
func (v PlannerView) Resolve(name string) (Tool, bool) {
	desc, ok := v.cat.Resolve(name)
	return desc.Tool, ok
}

// List implements the planner's ToolCatalogView contract. Returns the
// filtered slice of Tools visible to the run's identity.
func (v PlannerView) List() []Tool {
	return v.cat.List(v.filter)
}
