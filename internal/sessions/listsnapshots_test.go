package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
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
