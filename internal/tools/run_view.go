package tools

import (
	"context"
	"sort"
	"sync"
)

type runToolViewCtxKey struct{}
type runToolInvocationWrapperCtxKey struct{}
type unattendedToolInvocationCtxKey struct{}

// WithRunToolInvocationWrapper attaches the run's immutable descriptor
// middleware. A nil wrapper masks any wrapper from an outer run.
func WithRunToolInvocationWrapper(ctx context.Context, wrap func(ToolDescriptor) ToolDescriptor) context.Context {
	return context.WithValue(ctx, runToolInvocationWrapperCtxKey{}, wrap)
}

// WithUnattendedToolInvocation marks a tool call without a live steering
// inbox to receive an interactive decision.
func WithUnattendedToolInvocation(ctx context.Context) context.Context {
	return context.WithValue(ctx, unattendedToolInvocationCtxKey{}, true)
}

// IsUnattendedToolInvocation reports whether a tool call lacks a live
// steering inbox.
func IsUnattendedToolInvocation(ctx context.Context) bool {
	unattended, ok := ctx.Value(unattendedToolInvocationCtxKey{}).(bool)
	return ok && unattended
}

// ResolveToolInvocation resolves an already admitted tool name and applies
// the calling run's descriptor middleware. The view's visibility check
// must happen before this lookup, including any alias translation.
func ResolveToolInvocation(ctx context.Context, resolver interface {
	Resolve(string) (ToolDescriptor, bool)
}, name string) (ToolDescriptor, bool) {
	desc, ok := resolver.Resolve(name)
	if !ok {
		return desc, false
	}
	if wrap, ok := ctx.Value(runToolInvocationWrapperCtxKey{}).(func(ToolDescriptor) ToolDescriptor); ok && wrap != nil {
		desc = wrap(desc)
	}
	return desc, true
}

// runToolViewSeat is what WithRunToolView stores: the run's view (nil for
// the explicit "no view" seat) plus a once-per-seat memo of its discovery
// set. A seat lives for one dispatch decision, so the memo is bounded by
// that decision and shared safely by its parallel branches.
type runToolViewSeat struct {
	view PlannerCatalogView

	once     sync.Once
	tools    []Tool
	complete bool
}

// WithRunToolView seats the calling run's effective, sealed tool view on ctx.
// The run's dispatch entry point seats it once per decision, so every tool a
// run invokes — in particular the discovery meta-tools (`tool_search`,
// `tool_get`), the `skill_*` capability envelope and the `declarative_action`
// escape hatch — answers from the SAME view the planner declared and the
// dispatcher resolves against (owner scoping, exclusions, allowlist, loading
// overrides).
//
// A nil view is seated too, as an explicit "no view" that [RunToolView]
// reports as absent: it masks any view an outer context carried, so a nested
// or direct caller without its own view fails closed instead of inheriting
// another run's.
func WithRunToolView(ctx context.Context, view PlannerCatalogView) context.Context {
	return context.WithValue(ctx, runToolViewCtxKey{}, &runToolViewSeat{view: view})
}

func runToolViewSeatFrom(ctx context.Context) (*runToolViewSeat, bool) {
	seat, ok := ctx.Value(runToolViewCtxKey{}).(*runToolViewSeat)
	if !ok || seat == nil || seat.view == nil {
		return nil, false
	}
	return seat, true
}

// RunToolView returns the run tool view seated by [WithRunToolView]. Callers
// that surface or dispatch catalog tools on a run's behalf MUST treat absence
// as "nothing visible" and never fall back to the process-wide catalog.
func RunToolView(ctx context.Context) (PlannerCatalogView, bool) {
	seat, ok := runToolViewSeatFrom(ctx)
	if !ok {
		return nil, false
	}
	return seat.view, true
}

// RunToolDiscovery returns the seated view's discovery set (see
// [DiscoveryToolsOf]), computed at most once per seat so one decision's
// meta-tool calls share a single catalog scan. ok is false when no view is
// seated. The returned slice is shared: callers must not mutate it.
func RunToolDiscovery(ctx context.Context) (discovered []Tool, complete bool, ok bool) {
	seat, ok := runToolViewSeatFrom(ctx)
	if !ok {
		return nil, false, false
	}
	seat.once.Do(func() {
		seat.tools, seat.complete = discoveryTools(seat.view)
	})
	return seat.tools, seat.complete, true
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

// DiscoveryNames returns the sorted, de-duplicated names of view's discovery
// set — the run's full reachable tool set (prompt-time plus everything
// `tool_search` can surface). It is the ONE producer of the "allowed tools"
// capability input the skills surface consumes. A nil view yields nil (the
// default-deny capability set).
func DiscoveryNames(view PlannerCatalogView) []string {
	discovered, _ := DiscoveryToolsOf(view)
	return SortedToolNames(discovered)
}

// SortedToolNames returns the sorted, de-duplicated names of in (nil when
// empty).
func SortedToolNames(in []Tool) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	names := make([]string, 0, len(in))
	for _, t := range in {
		if _, dup := seen[t.Name]; dup {
			continue
		}
		seen[t.Name] = struct{}{}
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
