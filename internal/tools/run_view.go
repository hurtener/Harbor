package tools

import "context"

type runToolViewCtxKey struct{}

// WithRunToolView seats the calling run's effective, sealed tool view on ctx.
// The run's dispatch entry point seats it once per decision, so every tool a
// run invokes — in particular the discovery meta-tools (`tool_search`,
// `tool_get`) and the `declarative_action` escape hatch — answers from the
// SAME view the planner declared and the dispatcher resolves against (owner
// scoping, exclusions, allowlist, loading overrides). A nil view seats
// nothing, so a reader fails closed.
func WithRunToolView(ctx context.Context, view PlannerCatalogView) context.Context {
	if view == nil {
		return ctx
	}
	return context.WithValue(ctx, runToolViewCtxKey{}, view)
}

// RunToolView returns the run tool view seated by [WithRunToolView]. Callers
// that surface or dispatch catalog tools on a run's behalf MUST treat absence
// as "nothing visible" and never fall back to the process-wide catalog.
func RunToolView(ctx context.Context) (PlannerCatalogView, bool) {
	v, ok := ctx.Value(runToolViewCtxKey{}).(PlannerCatalogView)
	return v, ok && v != nil
}

// DiscoveryToolsOf returns view's discovery set: every tool the run may
// discover and invoke, both loading modes, in namespace-precedence order
// (always-loaded first, then deferred). The boolean reports whether the view
// supplied a complete deferred snapshot; a custom view exposing only
// List/Resolve yields its prompt-time List and false.
func DiscoveryToolsOf(view PlannerCatalogView) ([]Tool, bool) {
	if view == nil {
		return nil, false
	}
	return discoveryTools(view)
}
