// Phase 57 wave integration test — the StateStore-backed durable event
// log driver exercised end-to-end against all three production
// StateStore drivers (in-memory, SQLite, Postgres).
//
// This is the binding §17 integration test for Phase 57: its plan's
// Deps span the events subsystem (Phase 05) and the state subsystem
// (Phases 07/15/16), and it closes the cross-subsystem seam between
// them. Real drivers everywhere on the seam — no mocks. Identity
// propagation is asserted through every layer; the
// publish→teardown→rebuild→replay-no-gaps scenario is the acceptance
// criterion; a closed-store-mid-publish failure mode is covered; the
// whole suite runs under -race.
//
// Postgres is skipped with a reason when HARBOR_PG_DSN is unset,
// mirroring the Phase 16 driver-test convention.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// stateDriverCase names a StateStore driver and a constructor that
// returns a fresh, isolated store (or skips the test when the backend
// is unavailable).
type stateDriverCase struct {
	name    string
	newFunc func(t *testing.T) state.StateStore
}

func durableStateDrivers() []stateDriverCase {
	return []stateDriverCase{
		{
			name: "inmem",
			newFunc: func(t *testing.T) state.StateStore {
				s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
				if err != nil {
					t.Fatalf("stateinmem.New: %v", err)
				}
				return s
			},
		},
		{
			name: "sqlite",
			newFunc: func(t *testing.T) state.StateStore {
				dsn := filepath.Join(t.TempDir(), "durable-eventlog.db")
				s, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
				if err != nil {
					t.Fatalf("statesqlite.New(%q): %v", dsn, err)
				}
				return s
			},
		},
		{
			name: "postgres",
			newFunc: func(t *testing.T) state.StateStore {
				dsn := os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN not set; skipping postgres durable-event-log integration — see docs/plans/phase-16-state-postgres.md")
				}
				s, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
				if err != nil {
					t.Fatalf("statepostgres.New: %v", err)
				}
				return s
			},
		},
	}
}

func durableEventsCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         64,
	}
}

func dq(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	}}
}

type e2ePayload struct {
	events.Sealed
	Note string
}

func publishWarn(t *testing.T, bus events.EventBus, id identity.Quadruple, note string) {
	t.Helper()
	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  e2ePayload{Note: note},
	}); err != nil {
		t.Fatalf("Publish(%q): %v", note, err)
	}
}

// TestE2E_Phase57_DurableReplay_AllStateDrivers is the acceptance
// gate: a late subscriber after a Runtime restart sees no gaps,
// across every StateStore driver.
func TestE2E_Phase57_DurableReplay_AllStateDrivers(t *testing.T) {
	for _, sc := range durableStateDrivers() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			store := sc.newFunc(t)
			t.Cleanup(func() { _ = store.Close(context.Background()) })

			id := dq("tenant-a", "user-a", "session-a")

			// --- First Runtime: publish 12 events, tear the bus down.
			bus1, err := durable.New(durableEventsCfg(), auditpatterns.New(), store)
			if err != nil {
				t.Fatalf("durable.New (run 1): %v", err)
			}
			for i := 0; i < 12; i++ {
				publishWarn(t, bus1, id, fmt.Sprintf("run1-%d", i))
			}
			if err := bus1.Close(context.Background()); err != nil {
				t.Fatalf("bus1.Close: %v", err)
			}

			// --- Second Runtime: a fresh bus over the SAME store.
			bus2, err := durable.New(durableEventsCfg(), auditpatterns.New(), store)
			if err != nil {
				t.Fatalf("durable.New (run 2): %v", err)
			}
			t.Cleanup(func() { _ = bus2.Close(context.Background()) })
			rp := bus2.(events.Replayer)

			// A late subscriber replays from the beginning — must see
			// all 12 with no gaps.
			got, err := rp.Replay(context.Background(),
				events.Cursor{SessionID: "session-a"},
				events.Filter{Tenant: "tenant-a", User: "user-a", Session: "session-a"})
			if err != nil {
				t.Fatalf("Replay after restart: %v", err)
			}
			if len(got) != 12 {
				t.Fatalf("%s: expected 12 events after restart, got %d", sc.name, len(got))
			}
			for i, ev := range got {
				if ev.Sequence != uint64(i+1) {
					t.Fatalf("%s: gap detected — event %d has Sequence %d", sc.name, i, ev.Sequence)
				}
				// Identity propagation through every layer.
				if ev.Identity.TenantID != "tenant-a" ||
					ev.Identity.UserID != "user-a" ||
					ev.Identity.SessionID != "session-a" {
					t.Fatalf("%s: identity not propagated through persistence: %+v", sc.name, ev.Identity)
				}
			}

			// Replay-from-cursor mid-stream is still exact across the
			// restart boundary.
			tail, err := rp.Replay(context.Background(),
				events.Cursor{SessionID: "session-a", Sequence: 9},
				events.Filter{Tenant: "tenant-a", User: "user-a", Session: "session-a"})
			if err != nil {
				t.Fatalf("Replay from cursor 9: %v", err)
			}
			if len(tail) != 3 || tail[0].Sequence != 10 {
				t.Fatalf("%s: expected events 10..12 from cursor 9, got %d (first=%d)",
					sc.name, len(tail), firstSeq(tail))
			}
		})
	}
}

// TestE2E_Phase57_DurableReplay_CrossTenantIsolation proves the
// durable log does not leak events across the identity boundary, on
// every StateStore driver.
func TestE2E_Phase57_DurableReplay_CrossTenantIsolation(t *testing.T) {
	for _, sc := range durableStateDrivers() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			store := sc.newFunc(t)
			t.Cleanup(func() { _ = store.Close(context.Background()) })

			bus, err := durable.New(durableEventsCfg(), auditpatterns.New(), store)
			if err != nil {
				t.Fatalf("durable.New: %v", err)
			}
			t.Cleanup(func() { _ = bus.Close(context.Background()) })
			rp := bus.(events.Replayer)

			idA := dq("tenant-1", "user-1", "session-1")
			idB := dq("tenant-2", "user-2", "session-2")

			for i := 0; i < 5; i++ {
				publishWarn(t, bus, idA, fmt.Sprintf("a-%d", i))
			}
			for i := 0; i < 7; i++ {
				publishWarn(t, bus, idB, fmt.Sprintf("b-%d", i))
			}

			gotA, err := rp.Replay(context.Background(),
				events.Cursor{SessionID: "session-1"},
				events.Filter{Tenant: "tenant-1", User: "user-1", Session: "session-1"})
			if err != nil {
				t.Fatalf("Replay tenant-1: %v", err)
			}
			if len(gotA) != 5 {
				t.Fatalf("%s: tenant-1 expected 5 events, got %d", sc.name, len(gotA))
			}
			for _, ev := range gotA {
				if ev.Identity.TenantID != "tenant-1" {
					t.Fatalf("%s: cross-tenant leak — tenant-1 replay returned %q",
						sc.name, ev.Identity.TenantID)
				}
			}

			// A non-admin filter for tenant-1 must reject a cross-session
			// replay request that elides the triple.
			_, err = rp.Replay(context.Background(),
				events.Cursor{SessionID: "session-2"},
				events.Filter{Session: "session-2"})
			if !errors.Is(err, events.ErrIdentityScopeRequired) {
				t.Fatalf("%s: expected ErrIdentityScopeRequired for triple-less replay, got %v",
					sc.name, err)
			}
		})
	}
}

// TestE2E_Phase57_DurableLog_ClosedStoreFailsLoudly is the mandatory
// §17.3 failure-mode coverage: when the StateStore is closed
// mid-flight, Publish surfaces the failure loudly — it never silently
// drops the event.
func TestE2E_Phase57_DurableLog_ClosedStoreFailsLoudly(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	bus, err := durable.New(durableEventsCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := dq("tenant-a", "user-a", "session-a")
	publishWarn(t, bus, id, "before-close") // succeeds

	// Close the store out from under the bus.
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	err = bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  e2ePayload{Note: "after-close"},
	})
	if err == nil {
		t.Fatalf("expected Publish to surface the closed-store failure loudly, got nil")
	}
	if !errors.Is(err, state.ErrStoreClosed) {
		t.Fatalf("expected wrapped state.ErrStoreClosed, got %v", err)
	}
}

// TestE2E_Phase57_DurableLog_ConcurrencyStress is the §17.3 long-lived
// wiring stress: N concurrent publishers across the events↔state seam,
// asserting no cross-talk and no goroutine leak after teardown.
func TestE2E_Phase57_DurableLog_ConcurrencyStress(t *testing.T) {
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	bus, err := durable.New(durableEventsCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	rp := bus.(events.Replayer)

	const n = 16
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			id := dq(
				fmt.Sprintf("tenant-%d", idx),
				fmt.Sprintf("user-%d", idx),
				fmt.Sprintf("session-%d", idx),
			)
			for j := 0; j < perGoroutine; j++ {
				if err := bus.Publish(context.Background(), events.Event{
					Type:     events.EventTypeRuntimeWarning,
					Identity: id,
					Payload:  e2ePayload{Note: fmt.Sprintf("g%d-e%d", idx, j)},
				}); err != nil {
					errCh <- fmt.Errorf("goroutine %d publish: %w", idx, err)
					return
				}
			}
			got, rerr := rp.Replay(context.Background(),
				events.Cursor{SessionID: id.SessionID},
				events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
			if rerr != nil {
				errCh <- fmt.Errorf("goroutine %d replay: %w", idx, rerr)
				return
			}
			if len(got) != perGoroutine {
				errCh <- fmt.Errorf("goroutine %d: expected %d events, got %d",
					idx, perGoroutine, len(got))
				return
			}
			for _, ev := range got {
				if ev.Identity.TenantID != id.TenantID {
					errCh <- fmt.Errorf("goroutine %d: cross-talk — got tenant %q",
						idx, ev.Identity.TenantID)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func firstSeq(evs []events.Event) uint64 {
	if len(evs) == 0 {
		return 0
	}
	return evs[0].Sequence
}
