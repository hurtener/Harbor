package inmem_test

import (
	"context"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/conformancetest"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
)

// TestInmem_Conformance runs the canonical events.EventBus conformance
// suite against the in-memory driver. See conformancetest.Run for the
// full pinned scenario list.
func TestInmem_Conformance(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
		return conformancetest.Harness{
			NewBus: func(t *testing.T) events.EventBus {
				t.Helper()
				bus, err := inmem.New(replayCfg(), auditpatterns.New())
				if err != nil {
					t.Fatalf("inmem.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
			NewReplayDisabledBus: func(t *testing.T) events.EventBus {
				t.Helper()
				// defaultCfg leaves ReplayBufferSize at its zero value —
				// replay off.
				bus, err := inmem.New(defaultCfg(), auditpatterns.New())
				if err != nil {
					t.Fatalf("inmem.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
			NewBoundedRetentionBus: func(t *testing.T, capacity int) events.EventBus {
				t.Helper()
				bus, err := inmem.New(replayCfgN(capacity), auditpatterns.New())
				if err != nil {
					t.Fatalf("inmem.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
		}
	})
}
