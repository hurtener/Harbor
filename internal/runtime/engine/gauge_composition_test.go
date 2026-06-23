package engine

// Composition test: the engine's CapacityEntryCount accessor, wired
// through the telemetry runtime-gauge seam, returns toward baseline
// after the capacity sweeper reaps idle trackers. This is the
// end-to-end proof of the "engine-capacity gauge returns toward baseline
// after N runs" intent — the accessor + the RegisterRuntimeGauges →
// Snapshot path, composed, not just unit-tested in isolation.
//
// (Production wiring of the engine gauges into a binary is deferred until
// a runtime that hosts an engine.Engine node-graph; the dev binary is
// planner/RunLoop-shaped and sources only the governance + events gauges.
// See D-251. This test pins the mechanism so that future wiring is a
// one-liner, not a rediscovery.)

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
)

func TestEngineCapacityGauge_ReturnsToBaseline(t *testing.T) {
	e := reapTestEngine(t)

	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(
		config.TelemetryConfig{ServiceName: "engine-gauge-test"},
		telemetry.WithMetricReader(reader),
	)
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	if err := reg.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		EngineCapacityEntries: func() int64 { return int64(e.CapacityEntryCount()) },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges: %v", err)
	}

	const name = "harbor_runtime_engine_capacity_entries"
	gauge := func() float64 {
		t.Helper()
		snap, serr := reg.Snapshot(context.Background())
		if serr != nil {
			t.Fatalf("Snapshot: %v", serr)
		}
		for _, g := range snap.Gauges {
			if g.Name == name {
				return g.Value
			}
		}
		t.Fatalf("gauge %q absent from snapshot", name)
		return 0
	}

	if v := gauge(); v != 0 {
		t.Fatalf("baseline gauge = %v, want 0", v)
	}

	// Acquire N per-run capacity trackers; the gauge tracks the live map.
	const n = 8
	for i := range n {
		rc := e.acquireCapacity("run-"+itoa(i), 2)
		rc.mu.Lock()
		rc.lastActivity = rc.lastActivity.Add(-2 * DefaultCapacityTTL) // age for the reap below
		rc.mu.Unlock()
	}
	if v := gauge(); v != n {
		t.Errorf("gauge after %d acquisitions = %v, want %d", n, v, n)
	}

	// The sweeper reaps the idle (aged) trackers; the gauge returns to baseline.
	e.reapIdleCapacities(time.Now())
	if v := gauge(); v != 0 {
		t.Errorf("gauge after reap = %v, want 0 (returned toward baseline)", v)
	}
}
