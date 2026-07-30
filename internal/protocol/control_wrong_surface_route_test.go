package protocol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// control_wrong_surface_route_test.go — the guard for the wrong-surface
// refusals' ROUTE text.
//
// The refusal exists to send an operator somewhere useful. It shipped naming
// `POST /v1/search`, a route this runtime has never mounted, so following the
// message produced a 404 — worse than naming no route at all, because it reads
// like the caller found a bug in the transport rather than in their own call.
// The string was unguarded, so nothing caught it for the life of the surface.
//
// The expectation here is DERIVED from the transport's own RoutePattern rather
// than hardcoded, so a future route change moves the assertion with the code
// instead of pinning yesterday's answer.
func TestControlSurface_WrongSurfaceRefusalNamesARouteThatExists(t *testing.T) {
	t.Parallel()
	f := newAgentFixture(t, true)
	ctx, err := identity.WithVerified(context.Background(),
		agentIdent("tenant-route", "user-route", "sess-route"))
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}

	// RoutePattern is "POST /v1/control/{method}". The concrete route for one
	// method is that pattern with the placeholder substituted.
	prefix, _, ok := strings.Cut(control.RoutePattern, "{method}")
	if !ok {
		t.Fatalf("control.RoutePattern %q no longer carries a {method} placeholder — this "+
			"guard's derivation is stale", control.RoutePattern)
	}

	searchMethods := []methods.Method{
		methods.MethodSearchQuery,
		methods.MethodSearchSessions,
		methods.MethodSearchTasks,
		methods.MethodSearchEvents,
		methods.MethodSearchArtifacts,
	}
	for _, m := range searchMethods {
		if !methods.IsSearchMethod(m) {
			t.Fatalf("%q is not classified as a search method — this table is stale", m)
		}
		_, derr := f.surface.Dispatch(ctx, m, &types.SearchRequest{})
		var pe *protoerrors.Error
		if !errors.As(derr, &pe) {
			t.Fatalf("%s through the ControlSurface: got %v, want a typed protocol error", m, derr)
		}
		if pe.Code != protoerrors.CodeInvalidRequest {
			t.Errorf("%s: code = %s, want %s", m, pe.Code, protoerrors.CodeInvalidRequest)
		}
		want := strings.TrimPrefix(prefix, "POST ") + string(m)
		if !strings.Contains(pe.Message, want) {
			t.Errorf("%s: the refusal is %q, which does not name the route that actually "+
				"serves it (%q). An operator following this message lands on a 404.",
				m, pe.Message, want)
		}
		// The specific wrong answer that shipped. Named explicitly so the
		// regression cannot come back under a passing "contains" check that a
		// message mentioning BOTH routes would satisfy.
		if strings.Contains(pe.Message, "/v1/search") &&
			!strings.Contains(pe.Message, strings.TrimPrefix(prefix, "POST ")) {
			t.Errorf("%s: the refusal names /v1/search, a route this runtime does not mount", m)
		}
	}
}
