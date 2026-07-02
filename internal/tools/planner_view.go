package tools

// PlannerView is the planner-facing, schema-only projection of a
// [ToolCatalog] under one run's identity scope (
// originally as a `cmd/harbor` adapter).
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
// Per-run construction discipline: the view is a value type
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
// operator-configured `tools.granted_scopes` list (a scoped
// ): tools whose declared AuthScopes are entirely contained in
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

// PlannerCatalogView is the planner-facing, schema-only catalog view
// contract (Resolve + List over [Tool]). Both [PlannerView] and
// [ExclusionView] satisfy it; the run-loop driver assigns the value it
// builds to the planner's structurally-identical `ToolCatalogView`. The
// interface lives here (not in `internal/planner`) so the agent-config
// run-start projection can compose views without importing `planner`.
type PlannerCatalogView interface {
	// Resolve returns the schema-only Tool by name and a presence bool.
	Resolve(name string) (Tool, bool)
	// List returns every Tool visible to the run.
	List() []Tool
}

// compile-time assertion: PlannerView satisfies PlannerCatalogView.
var _ PlannerCatalogView = PlannerView{}

// ExclusionView wraps a [PlannerCatalogView], hiding any tool whose Source
// is a paused MCP server or whose Name is individually disabled — the
// run-start projection of the agent-config tool-exposure desired state.
//
// Exclusion is projection-time only and applies NEXT-turn: the live MCP
// transport stays WARM while a server is paused (resume is a flag flip, not
// a re-dial). A tool hidden here is absent from BOTH List (the
// `<available_tools>` prompt section) and Resolve (so the planner cannot
// re-invoke a remembered name), making the exclusion the planner's view of
// the run's snapshot.
//
// Concurrent reuse: ExclusionView is a value type with read-only fields
// (the wrapped view + two immutable lookup sets). Each run constructs its
// own via [NewExclusionView]; DO NOT cache across runs (the exclusion sets
// derive from the run's resolved active revision).
type ExclusionView struct {
	inner           PlannerCatalogView
	excludedSources map[ToolSourceID]struct{}
	excludedNames   map[string]struct{}
}

// compile-time assertion: ExclusionView satisfies PlannerCatalogView.
var _ PlannerCatalogView = ExclusionView{}

// NewExclusionView wraps inner so tools whose Source is in pausedSources or
// whose Name is in disabledNames are hidden. The two slices are copied into
// lookup sets so the caller's backing arrays cannot mutate the view after
// construction. A view with both sets empty still wraps inner faithfully
// (it just hides nothing) — the projection only wraps when at least one set
// is non-empty, but the constructor is robust either way.
func NewExclusionView(inner PlannerCatalogView, pausedSources, disabledNames []string) ExclusionView {
	sources := make(map[ToolSourceID]struct{}, len(pausedSources))
	for _, s := range pausedSources {
		if s == "" {
			continue
		}
		sources[ToolSourceID(s)] = struct{}{}
	}
	names := make(map[string]struct{}, len(disabledNames))
	for _, n := range disabledNames {
		if n == "" {
			continue
		}
		names[n] = struct{}{}
	}
	return ExclusionView{inner: inner, excludedSources: sources, excludedNames: names}
}

// excluded reports whether t is hidden by the view's exclusion sets.
func (v ExclusionView) excluded(t Tool) bool {
	if _, ok := v.excludedNames[t.Name]; ok {
		return true
	}
	if t.Source != "" {
		if _, ok := v.excludedSources[t.Source]; ok {
			return true
		}
	}
	return false
}

// Resolve implements PlannerCatalogView. An excluded tool resolves as
// absent (found=false), so the planner cannot re-invoke a paused/disabled
// name from memory.
func (v ExclusionView) Resolve(name string) (Tool, bool) {
	t, ok := v.inner.Resolve(name)
	if !ok || v.excluded(t) {
		return Tool{}, false
	}
	return t, true
}

// List implements PlannerCatalogView. Returns the inner view's tools minus
// the excluded ones; the slice is always freshly allocated.
func (v ExclusionView) List() []Tool {
	in := v.inner.List()
	out := make([]Tool, 0, len(in))
	for _, t := range in {
		if v.excluded(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// LoadingOverrideView wraps a [PlannerCatalogView], rewriting each tool's
// LoadingMode from a per-run resolved effective-mode map and re-applying the
// prompt-time loading predicate against that EFFECTIVE mode — the run-start
// projection of the agent-config loading-mode override. Sibling of
// [ExclusionView]: the two compose independently — LoadingOverrideView
// changes prompt PRESENCE only, ExclusionView removes CAPABILITY.
//
// List() keeps only tools whose effective mode is in the constructor's
// visibleModes set (defaulting to [LoadingAlways], mirroring
// [CatalogFilter]'s default), and rewrites Tool.Loading on every returned
// tool. Resolve() NEVER filters on loading — matching [PlannerView.Resolve]
// — so a tool overridden to LoadingDeferred stays reachable through the
// two-turn tool_search discovery cycle; it still rewrites the returned
// Tool's Loading to the effective mode.
//
// Concurrent reuse: LoadingOverrideView is a value type with read-only
// fields (the wrapped view + two immutable lookup maps built at
// construction). Each run constructs its own via [NewLoadingOverrideView];
// DO NOT cache across runs — the effective map derives from the run's
// resolved active config revision (the concurrent-reuse contract).
type LoadingOverrideView struct {
	inner     PlannerCatalogView
	effective map[string]LoadingMode
	visible   map[LoadingMode]struct{}
}

// compile-time assertion: LoadingOverrideView satisfies PlannerCatalogView.
var _ PlannerCatalogView = LoadingOverrideView{}

// NewLoadingOverrideView wraps inner, rewriting each resolved Tool's Loading
// via effective (keyed by the tool's full catalog Name). A name absent from
// effective keeps the Loading value inner reports (the boot-effective mode)
// — callers pass an effective map containing ONLY the names an override
// actually changes, not every catalog entry. List() filters to visibleModes
// (an empty slice defaults to [LoadingAlways], mirroring CatalogFilter's
// default prompt-time view). Both effective and visibleModes are copied so
// the caller's backing storage cannot mutate the view after construction.
//
// inner is expected to expose BOTH loading modes (its own CatalogFilter
// should carry LoadingModes: [LoadingAlways, LoadingDeferred]) — this view
// performs the loading-based filtering itself, against the EFFECTIVE mode,
// not inner's boot-effective one.
func NewLoadingOverrideView(inner PlannerCatalogView, effective map[string]LoadingMode, visibleModes []LoadingMode) LoadingOverrideView {
	eff := make(map[string]LoadingMode, len(effective))
	for k, v := range effective {
		eff[k] = v
	}
	if len(visibleModes) == 0 {
		visibleModes = []LoadingMode{LoadingAlways}
	}
	vis := make(map[LoadingMode]struct{}, len(visibleModes))
	for _, m := range visibleModes {
		vis[m] = struct{}{}
	}
	return LoadingOverrideView{inner: inner, effective: eff, visible: vis}
}

// rewrite returns t with Loading set to its effective mode: the override
// named by t.Name in the view's effective map, or t.Loading unchanged
// (defaulted to LoadingAlways when empty, matching CatalogFilter.matches'
// convention) when no override applies.
func (v LoadingOverrideView) rewrite(t Tool) Tool {
	if mode, ok := v.effective[t.Name]; ok {
		t.Loading = mode
		return t
	}
	if t.Loading == "" {
		t.Loading = LoadingAlways
	}
	return t
}

// Resolve implements PlannerCatalogView. Never filters on loading (matching
// PlannerView.Resolve) — the two-turn tool_search discovery cycle depends on
// a deferred-overridden tool staying resolvable by name. Rewrites the
// returned Tool's Loading to its effective mode.
func (v LoadingOverrideView) Resolve(name string) (Tool, bool) {
	t, ok := v.inner.Resolve(name)
	if !ok {
		return Tool{}, false
	}
	return v.rewrite(t), true
}

// List implements PlannerCatalogView. Returns inner's tools rewritten to
// their effective mode, filtered to the constructor's visibleModes set; the
// slice is always freshly allocated.
func (v LoadingOverrideView) List() []Tool {
	in := v.inner.List()
	out := make([]Tool, 0, len(in))
	for _, t := range in {
		rt := v.rewrite(t)
		if _, ok := v.visible[rt.Loading]; !ok {
			continue
		}
		out = append(out, rt)
	}
	return out
}
