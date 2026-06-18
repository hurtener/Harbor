package durable_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/conformancetest"

	// Side-effect: register the durable driver so OpenDriver works.
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/durable"
)

func newStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	return s
}

func mkBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	return bus
}

// openOver opens the durable driver over a caller-supplied store and
// bus. The store is NOT closed by this helper, so a test can close the
// registry + bus (simulating a restart) and reopen a fresh driver over
// the SAME store.
func openOver(t *testing.T, store state.StateStore, bus events.EventBus) tasks.TaskRegistry {
	t.Helper()
	r, err := tasks.OpenDriver("durable", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "durable"},
	})
	if err != nil {
		t.Fatalf("OpenDriver(durable): %v", err)
	}
	return r
}

func quadA() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"}}
}

func ctxFor(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestDurable_Conformance runs the canonical TaskRegistry suite (the
// D-031 gate) against the durable driver, verbatim — each subtest gets
// a fresh empty store, so the driver hydrates to empty and behaves
// identically to the in-process driver on the per-operation contract.
func TestDurable_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (tasks.TaskRegistry, func()) {
		store := newStore(t)
		bus := mkBus(t)
		r := openOver(t, store, bus)
		return r, func() {
			ctx := context.Background()
			_ = r.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		}
	})
}
