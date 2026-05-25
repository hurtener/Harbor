package sessions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestListSnapshots — Phase 72c (D-108) added the SessionLister
// capability for `search.sessions`. The Registry must list both open
// and closed sessions, filter by tenant/user/session ID inclusion,
// honour the LastSeen time window, and exclude closed sessions when
// IncludeClosed is false.
func TestListSnapshots_FiltersAndIncludesClosed(t *testing.T) {
	t.Parallel()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer bus.Close(context.Background())
	defer store.Close(context.Background())

	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	defer reg.CloseRegistry(context.Background())

	open := func(t *testing.T, ident identity.Identity) {
		t.Helper()
		ctx, _ := identity.With(context.Background(), ident)
		if _, err := reg.Open(ctx, ident.SessionID, ident); err != nil {
			t.Fatalf("Open %s: %v", ident.SessionID, err)
		}
	}
	closed := func(t *testing.T, ident identity.Identity) {
		t.Helper()
		ctx, _ := identity.With(context.Background(), ident)
		if err := reg.Close(ctx, ident.SessionID, "test-close"); err != nil {
			t.Fatalf("Close %s: %v", ident.SessionID, err)
		}
	}

	open(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-alpha"})
	open(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-beta"})
	open(t, identity.Identity{TenantID: "t2", UserID: "u9", SessionID: "s-gamma"})

	// Close s-beta.
	closed(t, identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-beta"})

	// Default IncludeClosed=false → exclude closed.
	out, err := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{"t1"},
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	for _, s := range out {
		if s.Closed {
			t.Errorf("closed=true row leaked when IncludeClosed=false: %+v", s)
		}
	}
	if len(out) != 1 || out[0].ID != "s-alpha" {
		t.Errorf("t1 open-only: got %v", out)
	}

	// IncludeClosed=true → both s-alpha (open) and s-beta (closed).
	out, err = reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs:     []string{"t1"},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("ListSnapshots includeClosed: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("t1 include closed: got %d, want 2", len(out))
	}

	// Tenant filter: t2 only.
	out, err = reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{"t2"},
	})
	if err != nil {
		t.Fatalf("ListSnapshots t2: %v", err)
	}
	if len(out) != 1 || out[0].Identity.TenantID != "t2" {
		t.Errorf("t2 filter: got %v", out)
	}

	// Empty filter — every open session across both tenants.
	out, err = reg.ListSnapshots(context.Background(), sessions.SessionListFilter{})
	if err != nil {
		t.Fatalf("ListSnapshots empty: %v", err)
	}
	// 2 open (s-alpha + s-gamma; s-beta closed and IncludeClosed=false default)
	if len(out) != 2 {
		t.Errorf("empty filter: got %d open snapshots, want 2", len(out))
	}
}

// hydrationBus constructs the inmem events bus the way the existing
// tests do; isolated so the round-6 hydration tests below stay tight.
func hydrationBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// TestListSnapshots_HydratesIdIndexFromExistingStateRecord — round-6 P8
// fix. When a new Registry is constructed over a StateStore that already
// holds a session record (the "reboot" case), the in-memory idIndex
// starts empty. The first call to Open() for that identity used to
// return ErrSessionAlreadyOpen without touching idIndex, leaving
// ListSnapshots reading 0 rows even though the record was sitting right
// there in the store — the Sessions page rendered "No sessions match
// these filters" on every boot after the first.
//
// The fix lives in Registry.Open: when the StateStore returns an
// existing record for the exact triple, populate idIndex (and
// openSessions, for a still-open record) before returning the sentinel.
func TestListSnapshots_HydratesIdIndexFromExistingStateRecord(t *testing.T) {
	t.Parallel()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	// reg1 — opens a session, then closes the registry without closing
	// the session, simulating a process restart with persistent state.
	reg1, err := sessions.New(store, config.SessionsConfig{}, hydrationBus(t))
	if err != nil {
		t.Fatalf("sessions.New reg1: %v", err)
	}
	ident := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-persistent"}
	ctx, _ := identity.With(context.Background(), ident)
	if _, err := reg1.Open(ctx, ident.SessionID, ident); err != nil {
		t.Fatalf("reg1.Open: %v", err)
	}
	if err := reg1.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("reg1.CloseRegistry: %v", err)
	}

	// reg2 over the SAME store — fresh in-memory catalog.
	reg2, err := sessions.New(store, config.SessionsConfig{}, hydrationBus(t))
	if err != nil {
		t.Fatalf("sessions.New reg2: %v", err)
	}
	t.Cleanup(func() { _ = reg2.CloseRegistry(context.Background()) })

	// Before any Open call — idIndex is empty, list returns nothing.
	out, err := reg2.ListSnapshots(context.Background(), sessions.SessionListFilter{})
	if err != nil {
		t.Fatalf("reg2.ListSnapshots before Open: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("reg2 fresh idIndex: got %d rows, want 0 (hydration is on-demand)", len(out))
	}

	// Open should return ErrSessionAlreadyOpen AND hydrate idIndex.
	if _, err := reg2.Open(ctx, ident.SessionID, ident); !errors.Is(err, sessions.ErrSessionAlreadyOpen) {
		t.Fatalf("reg2.Open err=%v, want ErrSessionAlreadyOpen", err)
	}

	// After Open's hydration path — list now returns the session.
	out, err = reg2.ListSnapshots(context.Background(), sessions.SessionListFilter{})
	if err != nil {
		t.Fatalf("reg2.ListSnapshots after Open: %v", err)
	}
	if len(out) != 1 || out[0].ID != ident.SessionID {
		t.Fatalf("reg2.ListSnapshots after hydration: got %v, want one row id=%q", out, ident.SessionID)
	}
	if out[0].Closed {
		t.Errorf("hydrated session should still be open, got Closed=true: %+v", out[0])
	}
}

// TestListSnapshots_HydratesIdIndexFromClosedStateRecord — round-6 P8
// fix, second branch. The Reopen-after-close path of Open() must also
// populate idIndex so operators can audit closed sessions after a
// reboot.
func TestListSnapshots_HydratesIdIndexFromClosedStateRecord(t *testing.T) {
	t.Parallel()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	ident := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-closed-persistent"}
	ctx, _ := identity.With(context.Background(), ident)

	reg1, err := sessions.New(store, config.SessionsConfig{}, hydrationBus(t))
	if err != nil {
		t.Fatalf("sessions.New reg1: %v", err)
	}
	if _, err := reg1.Open(ctx, ident.SessionID, ident); err != nil {
		t.Fatalf("reg1.Open: %v", err)
	}
	if err := reg1.Close(ctx, ident.SessionID, "reg1 shutdown"); err != nil {
		t.Fatalf("reg1.Close: %v", err)
	}
	if err := reg1.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("reg1.CloseRegistry: %v", err)
	}

	// reg2 — fresh idIndex, store has a CLOSED record at our triple.
	reg2, err := sessions.New(store, config.SessionsConfig{}, hydrationBus(t))
	if err != nil {
		t.Fatalf("sessions.New reg2: %v", err)
	}
	t.Cleanup(func() { _ = reg2.CloseRegistry(context.Background()) })

	if _, err := reg2.Open(ctx, ident.SessionID, ident); !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Fatalf("reg2.Open err=%v, want ErrReopenAfterClose", err)
	}

	// Include closed so the audit view picks the row up.
	out, err := reg2.ListSnapshots(context.Background(), sessions.SessionListFilter{IncludeClosed: true})
	if err != nil {
		t.Fatalf("reg2.ListSnapshots: %v", err)
	}
	if len(out) != 1 || out[0].ID != ident.SessionID || !out[0].Closed {
		t.Fatalf("reg2.ListSnapshots: got %v, want one closed row id=%q", out, ident.SessionID)
	}
}

func TestListSnapshots_ClosedRegistryRejected(t *testing.T) {
	t.Parallel()

	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	bus, err := inmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         128,
	}, patterns.New())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer bus.Close(context.Background())
	defer store.Close(context.Background())

	reg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	_, err = reg.ListSnapshots(context.Background(), sessions.SessionListFilter{})
	if err == nil {
		t.Fatal("ListSnapshots on closed registry: want error, got nil")
	}
}
