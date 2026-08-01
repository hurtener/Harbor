package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
)

// userAxisSurface builds a real events-backed search surface with the
// admin-tier predicate denied throughout.
func userAxisSurface(t *testing.T) *protocol.SearchSurface {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	replayer, ok := bus.(eventsubsys.Replayer)
	if !ok {
		t.Fatal("bus does not implement Replayer")
	}
	es, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
		Audit:      testSearchAudit,
	})
	if err != nil {
		t.Fatalf("events searcher: %v", err)
	}
	reg, err := search.NewRegistry(es)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	surf, err := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })
	if err != nil {
		t.Fatalf("NewSearchSurface: %v", err)
	}
	return surf
}

// TestMapSearchError_CrossUserIsScopeMismatch pins the wire code for the
// new refusals. It is the SAME code the tenant axis has published on this
// surface since the cluster shipped — one surface answering two codes for
// one class of refusal would be worse than the cross-surface divergence
// this deliberately keeps.
func TestMapSearchError_CrossUserIsScopeMismatch(t *testing.T) {
	t.Parallel()
	surf := userAxisSurface(t)
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "t1", UserID: "u-caller", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}

	cases := []struct {
		name   string
		method methods.Method
		filter types.SearchFilter
	}{
		{"per-index, named foreign user", methods.MethodSearchEvents,
			types.SearchFilter{UserIDs: []string{"u-victim"}}},
		{"per-index, multi-user fan-in", methods.MethodSearchEvents,
			types.SearchFilter{UserIDs: []string{"u-caller", "u-victim"}}},
		{"per-index, multi-session fan-in", methods.MethodSearchEvents,
			types.SearchFilter{SessionIDs: []string{"s1", "s2"}}},
		{"aggregate, named foreign user", methods.MethodSearchQuery,
			types.SearchFilter{UserIDs: []string{"u-victim"}}},
		{"aggregate, multi-session fan-in", methods.MethodSearchQuery,
			types.SearchFilter{SessionIDs: []string{"s1", "s2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, derr := surf.Dispatch(ctx, tc.method, &types.SearchRequest{Filter: tc.filter})
			var pe *protoerrors.Error
			if !errors.As(derr, &pe) {
				t.Fatalf("got %v, want *protoerrors.Error", derr)
			}
			if pe.Code != protoerrors.CodeScopeMismatch {
				t.Fatalf("got code %q, want CodeScopeMismatch", pe.Code)
			}
		})
	}
}

// TestSearchSurface_OwnScopeReadStillAnswers — the D-352 failure mode
// this fix was explicitly warned about: a stricter identity precondition
// that turns a working surface into a refusal is not a fix. An own-scope
// read, elided or self-named, must still answer.
func TestSearchSurface_OwnScopeReadStillAnswers(t *testing.T) {
	t.Parallel()
	surf := userAxisSurface(t)
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "t1", UserID: "u-caller", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	for _, filter := range []types.SearchFilter{
		{},
		{UserIDs: []string{"u-caller"}},
		{SessionIDs: []string{"s-my-other"}},
		{TenantIDs: []string{"t1"}, UserIDs: []string{"u-caller"}},
	} {
		for _, m := range []methods.Method{methods.MethodSearchEvents, methods.MethodSearchQuery} {
			resp, derr := surf.Dispatch(ctx, m, &types.SearchRequest{Filter: filter})
			if derr != nil {
				t.Fatalf("%s with own-scope filter %+v: %v", m, filter, derr)
			}
			if resp == nil {
				t.Fatalf("%s with own-scope filter %+v: nil response", m, filter)
			}
		}
	}
}
