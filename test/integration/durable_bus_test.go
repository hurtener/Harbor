// Integration test for the StateStore-backed durable MessageBus driver,
// exercised end-to-end against real production StateStore drivers
// (in-memory and SQLite) with real events.EventBus instances on the
// seam.
//
// This is the binding §17 integration test for the durable bus driver:
// its Deps span the distributed subsystem (the MessageBus contract +
// conformance) over the state + events subsystems, and it closes the
// cross-subsystem seam between them. Real drivers everywhere — no mocks.
// Identity propagation is asserted through every layer; the
// cross-instance fan-out and the publish→restart→replay scenarios are
// the acceptance criteria; a forced StateStore write error is the
// failure mode; the whole suite runs under -race.
//
// SQLite uses a file DSN so two store handles over the same file model
// two instances sharing one durable store (Postgres-as-queue shape),
// and a full close+reopen is a TRUE process-style restart.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"

	_ "github.com/hurtener/Harbor/internal/distributed/drivers/durable"
)

const busPollInterval = 15 * time.Millisecond

// busStoreCase opens a StateStore over a fixed backing location so the
// test can open TWO handles over the same data (two instances) and
// close+reopen (restart).
type busStoreCase struct {
	name string
	open func(t *testing.T) state.StateStore
}

func busStoreCases(t *testing.T) []busStoreCase {
	t.Helper()
	shared, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = shared.Close(context.Background()) })

	dsn := filepath.Join(t.TempDir(), "durable-bus.db")
	return []busStoreCase{
		// In-memory: both "instances" share ONE store object (the
		// in-process model — no cross-PROCESS sharing for in-mem).
		{name: "inmem", open: func(_ *testing.T) state.StateStore { return shared }},
		// SQLite: each open is a fresh handle over the SAME file, so two
		// handles model two instances sharing one durable store.
		{name: "sqlite", open: func(t *testing.T) state.StateStore {
			s, err := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatalf("statesqlite.New(%q): %v", dsn, err)
			}
			return s
		}},
	}
}

func mkBus(t *testing.T) events.EventBus {
	t.Helper()
	eb, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	return eb
}

func openDurableBus(t *testing.T, store state.StateStore, eb events.EventBus) distributed.MessageBus {
	t.Helper()
	b, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: eb,
		State:    store,
		Cfg:      config.DistributedConfig{BusDriver: "durable", BusPollInterval: busPollInterval},
	})
	if err != nil {
		t.Fatalf("OpenBusDriver(durable): %v", err)
	}
	return b
}

func busIDCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func busSubscribe(t *testing.T, eb events.EventBus, id identity.Identity) events.Subscription {
	t.Helper()
	sub, err := eb.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{distributed.EventTypeDistributedBusEnvelope},
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return sub
}

func collectN(sub events.Subscription, want int, timeout time.Duration) []distributed.BusEnvelope {
	var out []distributed.BusEnvelope
	deadline := time.After(timeout)
	for len(out) < want {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			if p, ok := ev.Payload.(distributed.BusEnvelopePayload); ok {
				out = append(out, p.Envelope)
			}
		case <-deadline:
			return out
		}
	}
	return out
}

func busEnvelope(edge string, id identity.Quadruple, eventID string) distributed.BusEnvelope {
	return distributed.BusEnvelope{
		Edge: edge, Source: "test", Identity: id, TaskID: "task-1",
		EventID: events.EventID(eventID), Payload: []byte(`{"k":"v"}`), Timestamp: time.Now().UTC(),
	}
}

// TestE2E_DurableBus_CrossInstanceAndRestart is the wave-level
// acceptance scenario across each real StateStore driver.
func TestE2E_DurableBus_CrossInstanceAndRestart(t *testing.T) {
	for _, sc := range busStoreCases(t) {
		t.Run(sc.name, func(t *testing.T) {
			idA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"}}
			idB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}}

			// --- cross-instance fan-out -----------------------------------
			storeA := sc.open(t)
			ebA := mkBus(t)
			bA := openDurableBus(t, storeA, ebA)

			storeB := sc.open(t)
			ebB := mkBus(t)
			subB := busSubscribe(t, ebB, idA.Identity)
			defer subB.Cancel()
			bB := openDurableBus(t, storeB, ebB)

			if err := bA.Publish(busIDCtx(t, idA.Identity), busEnvelope("cross", idA, "evt-cross")); err != nil {
				t.Fatalf("publish on A: %v", err)
			}
			// B's poller projects A's envelope (cross-instance).
			if got := collectN(subB, 1, 2*time.Second); len(got) != 1 || got[0].Edge != "cross" {
				t.Fatalf("cross-instance: B received %d envelopes, want [cross]", len(got))
			}

			// Identity isolation: a tenant-B subscriber never sees
			// tenant-A's envelope.
			subOther := busSubscribe(t, ebB, idB.Identity)
			defer subOther.Cancel()
			if got := collectN(subOther, 1, 300*time.Millisecond); len(got) != 0 {
				t.Errorf("identity isolation: tenant-B subscriber saw tenant-A traffic: %v", got)
			}

			_ = bA.Close(context.Background())
			_ = ebA.Close(context.Background())
			_ = bB.Close(context.Background())
			_ = ebB.Close(context.Background())
			if sc.name == "sqlite" {
				_ = storeA.Close(context.Background())
				_ = storeB.Close(context.Background())
			}

			// --- restart-replay ------------------------------------------
			store1 := sc.open(t)
			eb1 := mkBus(t)
			b1 := openDurableBus(t, store1, eb1)
			for i := range 3 {
				if err := b1.Publish(busIDCtx(t, idA.Identity), busEnvelope(fmt.Sprintf("r-%d", i), idA, fmt.Sprintf("evt-r-%d", i))); err != nil {
					t.Fatalf("restart publish %d: %v", i, err)
				}
			}
			_ = b1.Close(context.Background())
			_ = eb1.Close(context.Background())
			if sc.name == "sqlite" {
				_ = store1.Close(context.Background())
			}

			store2 := sc.open(t)
			eb2 := mkBus(t)
			sub2 := busSubscribe(t, eb2, idA.Identity)
			defer sub2.Cancel()
			b2 := openDurableBus(t, store2, eb2)
			t.Cleanup(func() {
				_ = b2.Close(context.Background())
				_ = eb2.Close(context.Background())
				if sc.name == "sqlite" {
					_ = store2.Close(context.Background())
				}
			})

			// The fresh instance replays the persisted envelopes. The
			// shared store also holds the earlier "cross" envelope, so we
			// drain generously and require AT LEAST the 3 restart
			// envelopes (want larger than the live set → drains until the
			// timeout).
			got := collectN(sub2, 16, 2*time.Second)
			seen := map[string]bool{}
			for _, e := range got {
				seen[e.Edge] = true
			}
			for i := range 3 {
				if !seen[fmt.Sprintf("r-%d", i)] {
					t.Errorf("restart-replay: missing envelope r-%d (got edges %v)", i, got)
				}
			}
		})
	}
}

// failingSaveStore forces Save to error, to exercise the fail-loud
// persistence path on the seam.
type failingSaveStore struct {
	state.StateStore
	err error
}

func (f failingSaveStore) Save(context.Context, state.StateRecord) error { return f.err }

// TestE2E_DurableBus_WriteError_FailsLoud asserts a StateStore write
// failure surfaces from Publish rather than being silently swallowed.
func TestE2E_DurableBus_WriteError_FailsLoud(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	defer func() { _ = inner.Close(context.Background()) }()
	eb := mkBus(t)
	defer func() { _ = eb.Close(context.Background()) }()

	b := openDurableBus(t, failingSaveStore{StateStore: inner, err: errors.New("disk full")}, eb)
	defer func() { _ = b.Close(context.Background()) }()

	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if err := b.Publish(busIDCtx(t, id.Identity), busEnvelope("x", id, "evt")); err == nil {
		t.Error("Publish against a failing StateStore: want error, got nil")
	}
}
