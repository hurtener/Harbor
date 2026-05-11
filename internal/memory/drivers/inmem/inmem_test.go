package inmem_test

// Driver-level tests for the InMem MemoryStore. The behavioural
// surface is covered by the conformance suite; this file only adds
// driver-specific cases the suite cannot express (e.g. construction
// errors).

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	"github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestInMem_ConformanceSuite invokes the canonical conformance suite
// against the inmem driver. Phase 25's SQLite + Postgres drivers MUST
// invoke the same suite via their own *_test.go files.
func TestInMem_ConformanceSuite(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
		red, err := audit.Open(context.Background(), config.AuditConfig{})
		if err != nil {
			t.Fatalf("audit.Open: %v", err)
		}
		bus, err := events.Open(context.Background(), driverEventsConfig(), red)
		if err != nil {
			t.Fatalf("events.Open: %v", err)
		}
		store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state.Open: %v", err)
		}
		mem, err := inmem.New(memory.ConfigSnapshot{
			Driver:   "inmem",
			Strategy: memory.StrategyNone,
		}, memory.Deps{State: store, Bus: bus})
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		return conformancetest.Harness{
			Store: mem,
			Bus:   bus,
			Cleanup: func() {
				_ = mem.Close(context.Background())
				_ = bus.Close(context.Background())
				_ = store.Close(context.Background())
			},
		}
	})
}

func TestInMem_New_RejectsTruncationStrategy(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyTruncation,
	}, memory.Deps{State: store, Bus: bus})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

func TestInMem_New_RejectsRollingSummaryStrategy(t *testing.T) {
	bus, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: store, Bus: bus})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

func TestInMem_New_RejectsNilState(t *testing.T) {
	bus, _ := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: nil, Bus: bus})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestInMem_New_RejectsNilBus(t *testing.T) {
	_, store := buildDeps(t)
	_, err := inmem.New(memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: nil})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestInMem_New_DefaultsToStrategyNone(t *testing.T) {
	bus, store := buildDeps(t)
	mem, err := inmem.New(memory.ConfigSnapshot{
		Driver: "inmem",
		// Strategy intentionally empty — must default to none.
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	defer mem.Close(context.Background())
}

func driverEventsConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60_000_000_000,
		DropWindow:               1_000_000_000,
	}
}

func buildDeps(t *testing.T) (events.EventBus, state.StateStore) {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), driverEventsConfig(), red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return bus, store
}
