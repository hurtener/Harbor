package routers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/routers"
)

type tagA struct{}
type tagB struct{}
type unknown struct{}

func tagOf(p any) string {
	switch p.(type) {
	case tagA:
		return "a"
	case tagB:
		return "b"
	}
	return ""
}

func makeUnion(def *engine.NodeRef) *routers.UnionRouter {
	return &routers.UnionRouter{
		Tag: tagOf,
		Branches: map[string]engine.NodeRef{
			"a": {Name: "alpha"},
			"b": {Name: "beta"},
		},
		Default: def,
	}
}

func TestUnionRouter_TagDispatch(t *testing.T) {
	t.Parallel()
	r := makeUnion(nil)
	envA := messages.Envelope{Payload: tagA{}, Headers: messages.Headers{TenantID: "T", UserID: "U"}, SessionID: "S", RunID: "R"}
	got, err := r.Select(envA)
	if err != nil {
		t.Fatalf("Select(tagA): %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("tagA: got=%v, want alpha", got)
	}

	envB := envA
	envB.Payload = tagB{}
	got, err = r.Select(envB)
	if err != nil {
		t.Fatalf("Select(tagB): %v", err)
	}
	if got.Name != "beta" {
		t.Errorf("tagB: got=%v, want beta", got)
	}
}

func TestUnionRouter_TagNotFound_RoutesToDefault(t *testing.T) {
	t.Parallel()
	def := engine.NodeRef{Name: "default"}
	r := makeUnion(&def)
	env := messages.Envelope{Payload: unknown{}, Headers: messages.Headers{TenantID: "T", UserID: "U"}, SessionID: "S", RunID: "R"}
	got, err := r.Select(env)
	if err != nil {
		t.Fatalf("Select(unknown): %v", err)
	}
	if got != def {
		t.Errorf("unknown tag: got=%v, want default", got)
	}
}

func TestUnionRouter_TagNotFound_NoDefault_ReturnsRouteNotFound(t *testing.T) {
	t.Parallel()
	r := makeUnion(nil)
	env := messages.Envelope{Payload: unknown{}, Headers: messages.Headers{TenantID: "T", UserID: "U"}, SessionID: "S", RunID: "R"}
	_, err := r.Select(env)
	if !errors.Is(err, routers.ErrRouteNotFound) {
		t.Fatalf("err=%v, want ErrRouteNotFound", err)
	}
}

func TestUnionRouter_NilReceiver_ReturnsRouteNotFound(t *testing.T) {
	t.Parallel()
	var r *routers.UnionRouter
	_, err := r.Select(messages.Envelope{})
	if !errors.Is(err, routers.ErrRouteNotFound) {
		t.Fatalf("nil receiver: err=%v, want ErrRouteNotFound", err)
	}
}

func TestUnionRouter_NilTag_ReturnsRouteNotFound(t *testing.T) {
	t.Parallel()
	r := &routers.UnionRouter{Tag: nil, Branches: map[string]engine.NodeRef{}}
	_, err := r.Select(messages.Envelope{})
	if !errors.Is(err, routers.ErrRouteNotFound) {
		t.Fatalf("nil Tag: err=%v, want ErrRouteNotFound", err)
	}
}

func TestUnionRouter_AsNode_WritesRoutePolicy(t *testing.T) {
	t.Parallel()
	r := makeUnion(nil)
	in := messages.Envelope{Payload: tagA{}, Headers: messages.Headers{TenantID: "T", UserID: "U"}, SessionID: "S", RunID: "R"}
	out, err := r.AsNode("router").Func(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("router NodeFunc: %v", err)
	}
	rp, ok := routers.FromMeta(out.Meta)
	if !ok {
		t.Fatalf("RoutePolicy missing")
	}
	if rp.ExplicitTarget == nil || rp.ExplicitTarget.Name != "alpha" {
		t.Errorf("ExplicitTarget=%+v, want alpha", rp.ExplicitTarget)
	}
}

func TestRoutePolicy_Overrides_Predicate(t *testing.T) {
	t.Parallel()
	// A union router's choice can be overridden by setting RoutePolicy
	// directly on the envelope before invocation. The router's
	// AsNode does not honor a pre-existing RoutePolicy (it always
	// computes its own); the override semantics live at the consumer
	// (engine worker) layer. Phase 14 ships the FromMeta extractor +
	// MetaKey constant so future engine-level RoutePolicy honoring is
	// a one-line consumer add.
	override := engine.NodeRef{Name: "override"}
	env := messages.Envelope{
		Meta: map[string]any{
			routers.MetaKeyRoutePolicy: routers.RoutePolicy{ExplicitTarget: &override},
		},
	}
	rp, ok := routers.FromMeta(env.Meta)
	if !ok {
		t.Fatalf("RoutePolicy missing on input")
	}
	if rp.ExplicitTarget.Name != "override" {
		t.Errorf("override target=%+v, want %q", rp.ExplicitTarget, "override")
	}
}
