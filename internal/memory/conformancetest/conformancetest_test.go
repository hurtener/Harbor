package conformancetest_test

// Self-applied conformance smoke. The InMem driver wires identity-
// scoped audit emits through a real EventBus + StateStore so the
// suite exercises the production seams (no mocks; per AGENTS.md §17).
//
// Phase 25 SQLite + Postgres drivers MUST run this same suite via
// their own *_test.go files.

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestInMem_Conformance(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
		red, err := audit.Open(context.Background(), config.AuditConfig{})
		if err != nil {
			t.Fatalf("audit.Open: %v", err)
		}
		bus, err := events.Open(context.Background(), conformanceEventsConfig(), red)
		if err != nil {
			t.Fatalf("events.Open: %v", err)
		}
		store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state.Open: %v", err)
		}
		mem, err := memoryinmem.New(memory.ConfigSnapshot{
			Driver:   "inmem",
			Strategy: memory.StrategyNone,
		}, memory.Deps{State: store, Bus: bus})
		if err != nil {
			t.Fatalf("memoryinmem.New: %v", err)
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

func conformanceEventsConfig() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60_000_000_000, // 60s
		DropWindow:               1_000_000_000,  // 1s
	}
}
