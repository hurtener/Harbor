// Package routers ships Harbor's runtime routing surface — Phase 14
// of the runtime kernel chain (RFC §6.1).
//
// Three router shapes:
//
//   - PredicateRouter: ordered list of (predicate, target) pairs.
//     The first predicate that matches selects the target.
//   - UnionRouter: payload-tag dispatch (string discriminator → target).
//     Used for sum-type-shaped payloads (e.g. planner Decision variants).
//   - RoutePolicy: explicit-target override. Set on an envelope's
//     Meta["route_policy"] to bypass predicate / union routing and
//     route to a specific target. The planner-driven path.
//
// Routers wrap as engine.Node via AsNode(name); they consume the
// Phase 10 NodeContext.Emit surface to write into the chosen branch.
// A router does NOT transform payloads — it decides where to send them.
package routers

import (
	"github.com/hurtener/Harbor/internal/runtime/engine"
)

// MetaKeyRoutePolicy is the Envelope.Meta key under which RoutePolicy
// can ride. Centralised so callers and tests reference one symbol
// rather than the string literal.
const MetaKeyRoutePolicy = "route_policy"

// RoutePolicy is the override mechanism that bypasses predicate /
// union routing when set on an envelope's Meta["route_policy"]. Useful
// for planner-driven branch selection where the planner already knows
// the target (e.g. a deterministic-planner step that names the next
// node explicitly).
//
// ExplicitTarget is a *engine.NodeRef so a nil pointer means "no
// override; defer to predicate/union routing." A non-nil pointer
// pinning a specific target overrides whatever the wrapped router
// would have selected.
type RoutePolicy struct {
	// ExplicitTarget names a single downstream node by name. nil means
	// "no override."
	ExplicitTarget *engine.NodeRef
}

// FromMeta extracts a RoutePolicy from env.Meta if present. Returns
// (zero, false) when Meta is nil, the key is absent, or the value is
// not a RoutePolicy. Callers use the bool to decide whether to honor
// the override; the zero value is a safe fall-through.
func FromMeta(meta map[string]any) (RoutePolicy, bool) {
	if meta == nil {
		return RoutePolicy{}, false
	}
	v, ok := meta[MetaKeyRoutePolicy]
	if !ok {
		return RoutePolicy{}, false
	}
	rp, ok := v.(RoutePolicy)
	if !ok {
		return RoutePolicy{}, false
	}
	return rp, true
}
