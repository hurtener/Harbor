package durable_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/distributed"
	"github.com/hurtener/Harbor/internal/distributed/conformancetest"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"

	// Side-effect: register the durable bus driver so OpenBusDriver works.
	_ "github.com/hurtener/Harbor/internal/distributed/drivers/durable"
)

// testPollInterval keeps the poller snappy so cross-instance /
// restart-replay tests resolve quickly.
const testPollInterval = 15 * time.Millisecond

func newStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	return s
}

func newEventBus(t *testing.T) events.EventBus {
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
		t.Fatalf("events inmem New: %v", err)
	}
	return eb
}

// openOver opens the durable bus over a caller-supplied store + event
// bus. The store is NOT closed here, so a test can close the bus
// (simulating a restart) and reopen a fresh instance over the SAME
// store.
func openOver(t *testing.T, store state.StateStore, eb events.EventBus) distributed.MessageBus {
	t.Helper()
	b, err := distributed.OpenBusDriver("durable", distributed.Dependencies{
		EventBus: eb,
		State:    store,
		Cfg:      config.DistributedConfig{BusDriver: "durable", BusPollInterval: testPollInterval},
	})
	if err != nil {
		t.Fatalf("OpenBusDriver(durable): %v", err)
	}
	return b
}

// TestDurable_Conformance runs the canonical MessageBus suite (the
// D-031 gate) against the durable driver verbatim.
func TestDurable_Conformance(t *testing.T) {
	conformancetest.RunBus(t, func(t *testing.T) (distributed.MessageBus, events.EventBus, func()) {
		store := newStore(t)
		eb := newEventBus(t)
		b := openOver(t, store, eb)
		return b, eb, func() {
			ctx := context.Background()
			_ = b.Close(ctx)
			_ = eb.Close(ctx)
			_ = store.Close(ctx)
		}
	})
}
