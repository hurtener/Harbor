package sessions_test

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
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
	statesubsys "github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type harness struct {
	store    statesubsys.StateStore
	bus      eventsubsys.EventBus
	registry *sessionsubsys.Registry
	cleanup  func()
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
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
	reg, err := sessionsubsys.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	return &harness{
		store:    store,
		bus:      bus,
		registry: reg,
		cleanup: func() {
			_ = reg.CloseRegistry(context.Background())
			_ = bus.Close(context.Background())
			_ = store.Close(context.Background())
		},
	}
}

func openSession(t *testing.T, h *harness, ident identity.Identity) {
	t.Helper()
	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := h.registry.Open(ctx, ident.SessionID, ident); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func callerCtx(t *testing.T, ident identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), ident)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func TestSessionsSearcher_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	_, err = s.Search(context.Background(), types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("Search w/o identity: got %v, want ErrIdentityRequired", err)
	}
}

func TestSessionsSearcher_RejectsCrossTenantWithoutAdmin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	openSession(t, h, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	_, err = s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if !errors.Is(err, search.ErrCrossTenantRequiresAdmin) {
		t.Fatalf("cross-tenant w/o admin: got %v, want ErrCrossTenantRequiresAdmin", err)
	}
}

func TestSessionsSearcher_ScopesToCallerTenant_AndMatchesQuery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	// Seed three sessions across two tenants.
	openSession(t, h, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sess-alpha"})
	openSession(t, h, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sess-beta"})
	openSession(t, h, identity.Identity{TenantID: "t2", UserID: "u9", SessionID: "sess-gamma"})

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}

	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sess-alpha"})

	// Caller in t1: should NOT see sess-gamma (cross-tenant).
	resp, err := s.Search(ctx, types.SearchRequest{Query: "sess"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Rows {
		if r.TenantID != "t1" {
			t.Errorf("CROSS-TENANT LEAK: row %s tenant=%s, caller t1", r.ID, r.TenantID)
		}
	}
	if len(resp.Rows) != 2 {
		t.Errorf("rows: got %d, want 2 (both t1 sessions)", len(resp.Rows))
	}

	// Query-narrowed: only sess-alpha matches.
	resp, err = s.Search(ctx, types.SearchRequest{Query: "alpha"})
	if err != nil {
		t.Fatalf("Search alpha: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].ID != "sess-alpha" {
		t.Errorf("Search alpha: got %v, want [sess-alpha]", resp.Rows)
	}
}

func TestSessionsSearcher_PaginationMath(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	for i := range 30 {
		openSession(t, h, identity.Identity{
			TenantID:  "t1",
			UserID:    "u1",
			SessionID: fmt.Sprintf("sess-%02d", i),
		})
	}
	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "sess-00"})
	resp, err := s.Search(ctx, types.SearchRequest{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.TotalCount != 30 {
		t.Errorf("TotalCount: got %d, want 30", resp.TotalCount)
	}
	if len(resp.Rows) != 10 {
		t.Errorf("Rows on page 1 size 10: got %d, want 10", len(resp.Rows))
	}
	if resp.PageCount != 3 {
		t.Errorf("PageCount: got %d, want 3", resp.PageCount)
	}
	if !resp.HasMore {
		t.Error("HasMore on page 1 of 3: want true")
	}
}

func TestSessionsSearcher_AdminCrossTenant_Allows(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	openSession(t, h, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	openSession(t, h, identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"})

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return true },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	resp, err := s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("cross-tenant admin search: got %d rows, want 2", len(resp.Rows))
	}
}

func TestSessionsSearcher_PageSizeOverMax_Rejected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	ctx := callerCtx(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"})
	_, err = s.Search(ctx, types.SearchRequest{PageSize: 999})
	if !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("PageSize over max: got %v, want ErrInvalidRequest", err)
	}
}

func TestSessionsSearcher_Concurrent_NoCrossTalk(t *testing.T) {
	// D-025 concurrent-reuse stress.
	const N = 100
	h := newHarness(t)
	defer h.cleanup()

	// Seed 1 session per tenant.
	for i := range 10 {
		openSession(t, h, identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%d", i),
			UserID:    "u",
			SessionID: fmt.Sprintf("sess-%d", i),
		})
	}

	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			t := i % 10
			ident := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", t),
				UserID:    "u",
				SessionID: fmt.Sprintf("sess-%d", t),
			}
			ctx, _ := identity.With(context.Background(), ident)
			resp, qerr := s.Search(ctx, types.SearchRequest{})
			if qerr != nil {
				failures <- fmt.Sprintf("g%d: %v", i, qerr)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != ident.TenantID {
					failures <- fmt.Sprintf("g%d: row tenant=%s, caller=%s (LEAK)", i, r.TenantID, ident.TenantID)
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
		t.Fatalf("concurrent-reuse failures (%d): %v", len(msgs), msgs)
	}
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}
