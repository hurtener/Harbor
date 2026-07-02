package durable_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/conformancetest"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
)

// discardLogger swallows the driver's boot-time degradation WARN so the
// conformance suite's best-effort-mode constructors stay quiet; the loud
// WARN itself is pinned by the driver-specific
// TestDurable_NoStateStore_DegradesLoudly, which stays put.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDurable_Conformance runs the canonical events.EventBus conformance
// suite against the durable driver. See conformancetest.Run for the full
// pinned scenario list. The default harness wires a REAL inmem StateStore
// (no mock at the seam); the replay-disabled and bounded-retention
// harnesses use the driver's documented best-effort ring mode (no
// StateStore configured), exactly as the driver's own tests do.
func TestDurable_Conformance(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
		return conformancetest.Harness{
			NewBus: func(t *testing.T) events.EventBus {
				t.Helper()
				store := newInmemStore(t)
				bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
				if err != nil {
					t.Fatalf("durable.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
			NewReplayDisabledBus: func(t *testing.T) events.EventBus {
				t.Helper()
				cfg := durableCfg()
				cfg.ReplayBufferSize = 0
				bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), nil, durable.WithLogger(discardLogger()))
				if err != nil {
					t.Fatalf("durable.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
			NewBoundedRetentionBus: func(t *testing.T, capacity int) events.EventBus {
				t.Helper()
				cfg := durableCfg()
				cfg.ReplayBufferSize = capacity
				bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), nil, durable.WithLogger(discardLogger()))
				if err != nil {
					t.Fatalf("durable.New: %v", err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				return bus
			},
		}
	})
}
