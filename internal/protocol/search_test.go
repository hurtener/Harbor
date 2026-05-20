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

func TestSearchSurface_RejectsUnknownMethod(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry()
	surf, err := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })
	if err != nil {
		t.Fatalf("NewSearchSurface: %v", err)
	}
	_, err = surf.Dispatch(context.Background(), methods.MethodStart, &types.SearchRequest{})
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeUnknownMethod {
		t.Fatalf("got %v, want CodeUnknownMethod", err)
	}
}

func TestSearchSurface_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry()
	surf, _ := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })
	_, err := surf.Dispatch(context.Background(), methods.MethodSearchSessions, &types.SearchRequest{})
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("got %v, want CodeIdentityRequired", err)
	}
}

func TestSearchSurface_RejectsNilRequest(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry()
	surf, _ := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	_, err := surf.Dispatch(ctx, methods.MethodSearchSessions, nil)
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("got %v, want CodeInvalidRequest", err)
	}
}

func TestSearchSurface_NoSearcherForIndex_ReturnsUnknownMethod(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry()
	surf, _ := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	_, err := surf.Dispatch(ctx, methods.MethodSearchSessions, &types.SearchRequest{})
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeUnknownMethod {
		t.Fatalf("got %v, want CodeUnknownMethod", err)
	}
}

func TestSearchSurface_CrossTenantWithoutAdmin_MapsTo_CodeScopeMismatch(t *testing.T) {
	t.Parallel()
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
	defer bus.Close(context.Background())

	replayer, ok := bus.(eventsubsys.Replayer)
	if !ok {
		t.Fatal("bus does not implement Replayer")
	}
	es, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("events searcher: %v", err)
	}
	reg, _ := search.NewRegistry(es)
	surf, _ := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })

	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"})
	_, err = surf.Dispatch(ctx, methods.MethodSearchEvents, &types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	var pe *protoerrors.Error
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want *protoerrors.Error", err)
	}
	if pe.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("cross-tenant w/o admin: got code %q, want CodeScopeMismatch", pe.Code)
	}
}

func TestSearchSurface_QueryDispatch_ConcurrentSafe(t *testing.T) {
	// One shared surface across N goroutines — D-025 across the
	// Protocol boundary, not just the per-index Searcher.
	t.Parallel()
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
	defer bus.Close(context.Background())

	replayer, _ := bus.(eventsubsys.Replayer)
	es, _ := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	reg, _ := search.NewRegistry(es)
	surf, _ := protocol.NewSearchSurface(reg, func(context.Context) bool { return false })

	const N = 32
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func(i int) {
			ctx, _ := identity.With(context.Background(), identity.Identity{
				TenantID:  "t1",
				UserID:    "u",
				SessionID: "s",
			})
			_, derr := surf.Dispatch(ctx, methods.MethodSearchQuery, &types.SearchRequest{})
			errs <- derr
		}(i)
	}
	for i := 0; i < N; i++ {
		if e := <-errs; e != nil {
			t.Errorf("g%d: %v", i, e)
		}
	}
}
