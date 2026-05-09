package routers

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// ErrRouteNotFound — predicate / union routing produced no target and
// the router has no Default. Wraps with a human-readable suffix
// indicating which router (predicate vs union) and what went wrong.
// Phase 11's RunError will subsume this error code; Phase 14 ships the
// typed sentinel so callers can errors.Is.
var ErrRouteNotFound = errors.New("routers: no branch matched and no default configured")

// PredicateBranch is one (predicate, target) pair on a PredicateRouter.
// Predicates are evaluated in slice order; the first one returning
// true selects the target.
type PredicateBranch struct {
	Predicate func(messages.Envelope) bool
	Target    engine.NodeRef
}

// PredicateRouter selects the first branch whose predicate matches the
// incoming envelope. Branch order matters; the Default target catches
// "no match" (or returns ErrRouteNotFound when nil).
//
// Routing happens via RoutePolicy: the router-as-Node returns the
// envelope unchanged but writes the chosen target into
// Meta[MetaKeyRoutePolicy]. The engine's worker honors that policy
// when it emits to the router's outgoing channels (every adjacency
// receives the envelope, but only the targeted node will see it via
// downstream filtering — see "Honoring RoutePolicy in adjacency"
// below).
//
// For Phase 14, the recommended adjacency shape is:
//
//	router → branchA, branchB, branchC
//
// where every branch node guards its NodeFunc with a RoutePolicy
// check at entry (returning a no-op envelope if the policy does not
// target it). This keeps the engine's worker loop unchanged from
// Phase 10 — routers are pure node-level concerns.
//
// A more elegant adjacency shape (engine-level RoutePolicy honoring)
// is reserved for a follow-up phase that extends Phase 10's worker;
// Phase 14 stays out of engine.go to avoid colliding with the
// parallel Phase 11 fork.
type PredicateRouter struct {
	Branches []PredicateBranch
	Default  *engine.NodeRef
}

// AsNode wraps the router as an engine.Node. The wrapped NodeFunc
// examines the envelope, evaluates predicates in order, writes the
// chosen target into Meta[MetaKeyRoutePolicy], and returns the
// envelope unchanged. The engine's adjacency map then routes the
// output to all branches; each branch's NodeFunc reads Meta and
// drops the envelope if it isn't the targeted branch.
//
// Returns a Node whose Name is `name` and whose Func wraps the router.
// AllowCycle defaults to false — routers are downstream-only.
func (r *PredicateRouter) AsNode(name string) engine.Node {
	return engine.Node{
		Name: name,
		Func: func(_ context.Context, env messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			target, err := r.Select(env)
			if err != nil {
				return messages.Envelope{}, err
			}
			out := withRoutePolicy(env, target)
			return out, nil
		},
	}
}

// Select evaluates the router's predicates and returns the chosen
// target. Returns ErrRouteNotFound when no branch matches and no
// default is set. A nil receiver is rejected with ErrRouteNotFound
// rather than panicking — defensive coding, since a misconfigured
// graph might construct a PredicateRouter with no branches.
func (r *PredicateRouter) Select(env messages.Envelope) (engine.NodeRef, error) {
	if r == nil {
		return engine.NodeRef{}, fmt.Errorf("%w: nil PredicateRouter", ErrRouteNotFound)
	}
	for _, b := range r.Branches {
		if b.Predicate != nil && b.Predicate(env) {
			return b.Target, nil
		}
	}
	if r.Default != nil {
		return *r.Default, nil
	}
	return engine.NodeRef{}, fmt.Errorf("%w: predicate router with %d branches", ErrRouteNotFound, len(r.Branches))
}

// withRoutePolicy returns a copy of env whose Meta carries the
// RoutePolicy override. The original env is NOT mutated; a fresh Meta
// map is allocated when env.Meta is nil to avoid aliasing.
func withRoutePolicy(env messages.Envelope, target engine.NodeRef) messages.Envelope {
	out := env
	if out.Meta == nil {
		out.Meta = make(map[string]any, 1)
	} else {
		// Shallow-copy to avoid sharing the underlying map across
		// concurrent runs.
		copied := make(map[string]any, len(out.Meta)+1)
		for k, v := range out.Meta {
			copied[k] = v
		}
		out.Meta = copied
	}
	out.Meta[MetaKeyRoutePolicy] = RoutePolicy{ExplicitTarget: &target}
	return out
}
