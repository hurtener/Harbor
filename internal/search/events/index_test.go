package events_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
)

// busHarness builds a real in-mem bus with replay enabled.
type busHarness struct {
	bus     eventsubsys.EventBus
	cleanup func()
}

func newBusHarness(t *testing.T) *busHarness {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	return &busHarness{
		bus: bus,
		cleanup: func() {
			_ = bus.Close(context.Background())
		},
	}
}

func publishRuntimeError(t *testing.T, bus eventsubsys.EventBus, ident identity.Quadruple) {
	t.Helper()
	if err := bus.Publish(context.Background(), eventsubsys.Event{
		Type:     eventsubsys.EventTypeRuntimeError,
		Identity: ident,
		Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "test error"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func TestEventsSearcher_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	replayer, ok := h.bus.(eventsubsys.Replayer)
	if !ok {
		t.Fatal("bus does not implement Replayer")
	}
	s, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	_, err = s.Search(context.Background(), types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("got %v, want ErrIdentityRequired", err)
	}
}

func TestEventsSearcher_ScopesToCallerSession(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	publishRuntimeError(t, h.bus, identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	})
	publishRuntimeError(t, h.bus, identity.Quadruple{
		Identity: identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"},
	})

	replayer := h.bus.(eventsubsys.Replayer)
	s, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}

	// Caller in t1/s1: should see only their event.
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	resp, err := s.Search(ctx, types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Rows {
		if r.TenantID != "t1" {
			t.Errorf("CROSS-TENANT LEAK: row tenant=%s, caller t1", r.TenantID)
		}
	}
}

func TestEventsSearcher_RejectsCrossTenantWithoutAdmin(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	replayer := h.bus.(eventsubsys.Replayer)
	s, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	_, err = s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if !errors.Is(err, search.ErrCrossTenantRequiresAdmin) {
		t.Fatalf("got %v, want ErrCrossTenantRequiresAdmin", err)
	}
}

func TestEventsSearcher_FacetType(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()

	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}}
	if err := h.bus.Publish(context.Background(), eventsubsys.Event{
		Type:     eventsubsys.EventTypeRuntimeError,
		Identity: q,
		Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "boom"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := h.bus.Publish(context.Background(), eventsubsys.Event{
		Type:     eventsubsys.EventTypeRuntimeWarning,
		Identity: q,
		Payload:  eventsubsys.RedactedMap{Data: map[string]any{"msg": "warn"}},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	replayer := h.bus.(eventsubsys.Replayer)
	s, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	resp, err := s.Search(ctx, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "events.type", Value: "runtime.error"}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Rows {
		if r.Facets["type"] != "runtime.error" {
			t.Errorf("facet should have filtered: got %q", r.Facets["type"])
		}
	}
}

func TestEventsSearcher_Concurrent_NoCrossTalk(t *testing.T) {
	const N = 100
	h := newBusHarness(t)
	defer h.cleanup()

	for i := range 10 {
		ident := identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", i),
			UserID:    "u",
			SessionID: fmt.Sprintf("s-%d", i),
		}}
		publishRuntimeError(t, h.bus, ident)
	}

	replayer := h.bus.(eventsubsys.Replayer)
	s, err := eventsearch.New(replayer, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			tIdx := i % 10
			ident := identity.Identity{TenantID: fmt.Sprintf("t-%d", tIdx), UserID: "u", SessionID: fmt.Sprintf("s-%d", tIdx)}
			ctx, _ := identity.With(context.Background(), ident)
			resp, qerr := s.Search(ctx, types.SearchRequest{})
			if qerr != nil {
				failures <- fmt.Sprintf("g%d: %v", i, qerr)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != ident.TenantID {
					failures <- fmt.Sprintf("g%d: LEAK tenant=%s caller=%s", i, r.TenantID, ident.TenantID)
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	var msgs []string
	for f := range failures {
		msgs = append(msgs, f)
	}
	if len(msgs) > 0 {
		t.Fatalf("concurrent-reuse failures: %v", msgs)
	}
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}
