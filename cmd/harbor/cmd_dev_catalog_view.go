// cmd/harbor/cmd_dev_catalog_view.go — the dev binary's
// `planner.ToolCatalogView` adapter. Phase 83i (D-152).
//
// `planner.RunContext.Catalog` is the planner-facing surface that
// renders into the `<available_tools>` prompt section (Phase 83a /
// 83b). Before 83i no production call site populated it, so the
// react planner's `<available_tools>` always rendered "(no tools
// registered for this run)" — the LLM had zero tool affordance and
// the agent could only Finish or hallucinate.
//
// runtimeCatalogView wraps the production `tools.ToolCatalog` + a
// per-run visibility filter (keyed on the run's identity triple +
// any GrantedScopes the dev token carries). The planner sees the
// FILTERED set; the catalog's internal store is immutable from the
// planner's perspective (the planner only reads).
//
// Concurrent-reuse contract (D-025): the view is a value type with
// two read-only fields. Each run constructs its own view (the filter
// depends on the run's identity). Sharing one view across runs would
// cross-contaminate visibility — DO NOT cache.

package main

import (
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// runtimeCatalogView adapts `tools.ToolCatalog` to the planner-facing
// `planner.ToolCatalogView` interface. Identity-filtered at construction.
type runtimeCatalogView struct {
	cat    tools.ToolCatalog
	filter tools.CatalogFilter
}

// Resolve implements planner.ToolCatalogView. Returns the schema-only
// Tool value the planner uses to build a CallTool decision — never
// the dispatch-side ToolDescriptor.
func (v runtimeCatalogView) Resolve(name string) (tools.Tool, bool) {
	desc, ok := v.cat.Resolve(name)
	return desc.Tool, ok
}

// List implements planner.ToolCatalogView. Returns the filtered slice
// of Tools visible to the run's identity.
func (v runtimeCatalogView) List() []tools.Tool {
	return v.cat.List(v.filter)
}

// newRuntimeCatalogView constructs the per-run view. `granted` is the
// operator-configured GrantedScopes list — sourced from
// `cfg.Tools.GrantedScopes` and threaded through the runloop driver
// (Phase 83m / Item 6 / D-156). The filter applies the standard
// AuthScopes subset check: tools whose declared AuthScopes are
// entirely contained in `granted` are visible; tools that require a
// missing scope are filtered out. An empty / nil `granted` keeps the
// prior "no scopes granted" default — tools with AuthScopes are
// invisible to the planner; tools without AuthScopes are always
// visible (the standard CatalogFilter rule).
func newRuntimeCatalogView(cat tools.ToolCatalog, q runtimeIdentity, granted []string) runtimeCatalogView {
	return runtimeCatalogView{
		cat: cat,
		filter: tools.CatalogFilter{
			TenantID:      q.Tenant,
			UserID:        q.User,
			SessionID:     q.Session,
			GrantedScopes: append([]string(nil), granted...),
		},
	}
}

// runtimeIdentity is a tiny local alias to keep the constructor
// readable without pulling in the identity package symbol.
type runtimeIdentity struct {
	Tenant, User, Session string
}

// Compile-time check that runtimeCatalogView satisfies the planner
// interface.
var _ planner.ToolCatalogView = runtimeCatalogView{}
