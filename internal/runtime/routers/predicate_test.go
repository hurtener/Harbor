package routers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/routers"
)

func envWithPriority(p int) messages.Envelope {
	return messages.Envelope{
		Headers: messages.Headers{
			TenantID: "T", UserID: "U", Priority: p,
		},
		SessionID: "S",
		RunID:     "R",
	}
}

func TestPredicateRouter_FirstMatch(t *testing.T) {
	t.Parallel()
	low := engine.NodeRef{Name: "low"}
	mid := engine.NodeRef{Name: "mid"}
	hi := engine.NodeRef{Name: "hi"}
	r := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(e messages.Envelope) bool { return e.Headers.Priority < 5 }, Target: low},
			{Predicate: func(e messages.Envelope) bool { return e.Headers.Priority < 10 }, Target: mid},
			{Predicate: func(e messages.Envelope) bool { return true }, Target: hi},
		},
	}

	got, err := r.Select(envWithPriority(3))
	if err != nil {
		t.Fatalf("Select(prio=3): %v", err)
	}
	if got != low {
		t.Errorf("prio=3: got=%v, want low", got)
	}

	// prio=7 should match the second predicate even though the third
	// would also match — first-match wins.
	got, err = r.Select(envWithPriority(7))
	if err != nil {
		t.Fatalf("Select(prio=7): %v", err)
	}
	if got != mid {
		t.Errorf("prio=7: got=%v, want mid", got)
	}
}

func TestPredicateRouter_NoMatch_RoutesToDefault(t *testing.T) {
	t.Parallel()
	def := engine.NodeRef{Name: "default"}
	r := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(e messages.Envelope) bool { return false }, Target: engine.NodeRef{Name: "never"}},
		},
		Default: &def,
	}
	got, err := r.Select(envWithPriority(3))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got != def {
		t.Errorf("got=%v, want default", got)
	}
}

func TestPredicateRouter_NoMatch_NoDefault_ReturnsRunError(t *testing.T) {
	t.Parallel()
	r := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(e messages.Envelope) bool { return false }, Target: engine.NodeRef{Name: "never"}},
		},
	}
	_, err := r.Select(envWithPriority(3))
	if !errors.Is(err, routers.ErrRouteNotFound) {
		t.Fatalf("err=%v, want ErrRouteNotFound", err)
	}
}

func TestPredicateRouter_NilReceiver_ReturnsRouteNotFound(t *testing.T) {
	t.Parallel()
	var r *routers.PredicateRouter
	_, err := r.Select(envWithPriority(0))
	if !errors.Is(err, routers.ErrRouteNotFound) {
		t.Fatalf("nil receiver Select: err=%v, want ErrRouteNotFound", err)
	}
}

func TestPredicateRouter_AsNode_WritesRoutePolicy(t *testing.T) {
	t.Parallel()
	target := engine.NodeRef{Name: "alpha"}
	r := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(_ messages.Envelope) bool { return true }, Target: target},
		},
	}
	node := r.AsNode("router")
	if node.Name != "router" {
		t.Errorf("Name=%q, want %q", node.Name, "router")
	}
	in := envWithPriority(1)
	out, err := node.Func(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("router NodeFunc: %v", err)
	}
	rp, ok := routers.FromMeta(out.Meta)
	if !ok {
		t.Fatalf("RoutePolicy not in Meta")
	}
	if rp.ExplicitTarget == nil || *rp.ExplicitTarget != target {
		t.Errorf("RoutePolicy.ExplicitTarget=%+v, want %+v", rp.ExplicitTarget, target)
	}
	// Original envelope's Meta must NOT be mutated.
	if in.Meta != nil {
		t.Errorf("original Meta mutated: %+v", in.Meta)
	}
}

func TestPredicateRouter_AsNode_PreservesExistingMeta(t *testing.T) {
	t.Parallel()
	target := engine.NodeRef{Name: "alpha"}
	r := &routers.PredicateRouter{
		Branches: []routers.PredicateBranch{
			{Predicate: func(_ messages.Envelope) bool { return true }, Target: target},
		},
	}
	in := envWithPriority(1)
	in.Meta = map[string]any{"trace_hint": "abc"}
	out, err := r.AsNode("router").Func(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("router NodeFunc: %v", err)
	}
	if out.Meta["trace_hint"] != "abc" {
		t.Errorf("trace_hint dropped: %+v", out.Meta)
	}
	if _, ok := routers.FromMeta(out.Meta); !ok {
		t.Errorf("route_policy missing")
	}
	// Original Meta must not be aliased — the router copies it.
	if len(in.Meta) != 1 {
		t.Errorf("original Meta mutated: %+v", in.Meta)
	}
}

func TestRoutePolicy_FromMeta_NilMeta(t *testing.T) {
	t.Parallel()
	if _, ok := routers.FromMeta(nil); ok {
		t.Error("nil meta should report ok=false")
	}
}

func TestRoutePolicy_FromMeta_WrongType(t *testing.T) {
	t.Parallel()
	m := map[string]any{routers.MetaKeyRoutePolicy: "not a RoutePolicy"}
	if _, ok := routers.FromMeta(m); ok {
		t.Error("wrong-type value should report ok=false")
	}
}

func TestRoutePolicy_FromMeta_HappyPath(t *testing.T) {
	t.Parallel()
	target := engine.NodeRef{Name: "x"}
	m := map[string]any{
		routers.MetaKeyRoutePolicy: routers.RoutePolicy{ExplicitTarget: &target},
	}
	rp, ok := routers.FromMeta(m)
	if !ok {
		t.Fatal("ok=false")
	}
	if rp.ExplicitTarget == nil || *rp.ExplicitTarget != target {
		t.Errorf("ExplicitTarget=%+v, want %+v", rp.ExplicitTarget, target)
	}
}
