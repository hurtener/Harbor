package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// sqliteWiring builds a Registry backed by a SQLite store at `dsn`. The
// store is closed on cleanup; the file persists across registry
// instances so a "restart" test can open a fresh registry over the same
// file. The returned closeFn lets a test simulate a restart by tearing
// the registry + store down before re-opening.
func sqliteWiring(t *testing.T, dsn string) (*sessions.Registry, func()) {
	t.Helper()
	evCfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), evCfg, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	reg, err := sessions.New(store, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: 1 * time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	closeFn := func() {
		_ = reg.CloseRegistry(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	}
	return reg, closeFn
}

// TestEnsureOpen_CreateOnFirstUse — a brand-new session id materialises
// on first EnsureOpen; the returned session carries the chosen id +
// triple.
func TestEnsureOpen_CreateOnFirstUse(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "conversation-A")

	s, err := reg.EnsureOpen(ctxFor(id), id)
	if err != nil {
		t.Fatalf("EnsureOpen create: %v", err)
	}
	if s == nil || s.ID != "conversation-A" {
		t.Fatalf("EnsureOpen returned %+v, want session id conversation-A", s)
	}
	if s.Identity != id {
		t.Fatalf("EnsureOpen identity = %+v, want %+v", s.Identity, id)
	}
}

// TestEnsureOpen_SecondTurnIsNoOp — a second EnsureOpen on the same
// (open) session is not an error; it returns the live record. This is
// the steady-state "second turn of a conversation" path.
func TestEnsureOpen_SecondTurnIsNoOp(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "conversation-A")

	first, err := reg.EnsureOpen(ctxFor(id), id)
	if err != nil {
		t.Fatalf("EnsureOpen first: %v", err)
	}
	second, err := reg.EnsureOpen(ctxFor(id), id)
	if err != nil {
		t.Fatalf("EnsureOpen second (no-op expected): %v", err)
	}
	if second.ID != first.ID || !second.OpenedAt.Equal(first.OpenedAt) {
		t.Fatalf("second EnsureOpen returned a different record: first=%+v second=%+v", first, second)
	}
}

// TestEnsureOpen_ClosedSession_FailsLoud — a closed session is NOT
// silently revived. EnsureOpen on a closed session id surfaces
// ErrReopenAfterClose (RFC §6.9).
func TestEnsureOpen_ClosedSession_FailsLoud(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("t1", "u1", "conversation-A")

	if _, err := reg.EnsureOpen(ctxFor(id), id); err != nil {
		t.Fatalf("EnsureOpen create: %v", err)
	}
	if err := reg.Close(ctxFor(id), id.SessionID, "test:explicit"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := reg.EnsureOpen(ctxFor(id), id)
	if !errors.Is(err, sessions.ErrReopenAfterClose) {
		t.Fatalf("EnsureOpen on closed session: got %v, want ErrReopenAfterClose", err)
	}
}

// TestEnsureOpen_MissingIdentity_FailsClosed — an incomplete triple is
// rejected (§6 rule 9: identity is mandatory).
func TestEnsureOpen_MissingIdentity_FailsClosed(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	bad := identity.Identity{TenantID: "t1", UserID: "u1"} // empty session
	_, err := reg.EnsureOpen(context.Background(), bad)
	if !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("EnsureOpen incomplete identity: got %v, want ErrIdentityIncomplete", err)
	}
}

// TestEnsureOpen_MultipleSessionsCoexistUnderOneTokenIdentity — the
// connection identity is one (tenant, user); many sessions coexist
// under it, fully isolated. This is the core D-171 multi-session shape:
// one credential, N conversations.
func TestEnsureOpen_MultipleSessionsCoexistUnderOneTokenIdentity(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)

	a := ident("dev", "dev", "A")
	b := ident("dev", "dev", "B")
	if _, err := reg.EnsureOpen(ctxFor(a), a); err != nil {
		t.Fatalf("EnsureOpen A: %v", err)
	}
	if _, err := reg.EnsureOpen(ctxFor(b), b); err != nil {
		t.Fatalf("EnsureOpen B: %v", err)
	}

	snaps, err := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{"dev"}, UserIDs: []string{"dev"}, IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	got := map[string]bool{}
	for _, s := range snaps {
		got[s.ID] = true
	}
	if !got["A"] || !got["B"] {
		t.Fatalf("ListSnapshots under one (tenant,user) missing a session: got %v", got)
	}
}

// TestEnsureOpen_RestartRediscoversSessions — the headline restart test.
// Two sessions are created against a SQLite store; the registry + store
// are torn down (simulating a process restart); a fresh registry opened
// over the SAME file re-discovers both sessions via the persistent
// catalog. Closes the D-171 "sessions vanish after restart" gap.
func TestEnsureOpen_RestartRediscoversSessions(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "state.sqlite")

	// --- boot 1: create two sessions ---
	reg1, close1 := sqliteWiring(t, dsn)
	a := ident("dev", "dev", "A")
	b := ident("dev", "dev", "B")
	if _, err := reg1.EnsureOpen(ctxFor(a), a); err != nil {
		t.Fatalf("boot1 EnsureOpen A: %v", err)
	}
	if _, err := reg1.EnsureOpen(ctxFor(b), b); err != nil {
		t.Fatalf("boot1 EnsureOpen B: %v", err)
	}
	close1() // simulate restart: tear the whole stack down

	// --- boot 2: fresh registry over the same file ---
	reg2, close2 := sqliteWiring(t, dsn)
	defer close2()

	// The fresh registry's in-memory idIndex starts empty; ListSnapshots
	// must hydrate from the persisted catalog and surface both sessions.
	snaps, err := reg2.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{"dev"}, UserIDs: []string{"dev"}, IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("boot2 ListSnapshots: %v", err)
	}
	got := map[string]bool{}
	for _, s := range snaps {
		got[s.ID] = true
	}
	if !got["A"] || !got["B"] {
		t.Fatalf("boot2 did not re-discover prior sessions: got %v (want A and B)", got)
	}

	// And a past session is still inspectable after restart.
	snap, err := reg2.Inspect(ctxFor(a), "A")
	if err != nil {
		t.Fatalf("boot2 Inspect A: %v", err)
	}
	if snap.ID != "A" {
		t.Fatalf("boot2 Inspect A returned id %q", snap.ID)
	}
}

// TestEnsureOpen_RestartHonoursCreateOnFirstUseForExistingOpen — after a
// restart, an EnsureOpen on a still-open prior session is a no-op (not a
// re-create, not a spurious ErrSessionAlreadyOpen surfaced to the
// caller).
func TestEnsureOpen_RestartHonoursCreateOnFirstUseForExistingOpen(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "state.sqlite")

	reg1, close1 := sqliteWiring(t, dsn)
	a := ident("dev", "dev", "A")
	first, err := reg1.EnsureOpen(ctxFor(a), a)
	if err != nil {
		t.Fatalf("boot1 EnsureOpen A: %v", err)
	}
	close1()

	reg2, close2 := sqliteWiring(t, dsn)
	defer close2()
	again, err := reg2.EnsureOpen(ctxFor(a), a)
	if err != nil {
		t.Fatalf("boot2 EnsureOpen A (no-op expected): %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("boot2 EnsureOpen returned a different session: %q vs %q", again.ID, first.ID)
	}
}

// TestEnsureOpen_ConcurrentReuse_NoRaceNoLeak — D-025 + §6 rule 10: N
// concurrent EnsureOpen calls across distinct sessions under one
// (tenant, user) against a single shared registry race-clean, with no
// cross-talk (every session is its own record) and no goroutine leak.
func TestEnsureOpen_ConcurrentReuse_NoRaceNoLeak(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)

	const n = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := ident("dev", "dev", fmt.Sprintf("session-%d", i))
			_, errs[i] = reg.EnsureOpen(ctxFor(id), id)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureOpen[%d]: %v", i, err)
		}
	}

	// Every distinct session must be its own isolated record.
	snaps, err := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{
		TenantIDs: []string{"dev"}, UserIDs: []string{"dev"}, IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != n {
		t.Fatalf("expected %d isolated sessions, got %d", n, len(snaps))
	}

	// Goroutine-leak check: EnsureOpen starts no per-call goroutines, so
	// the count returns to baseline (a small slack for the runtime).
	if after := runtime.NumGoroutine(); after > baseline+4 {
		t.Fatalf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
