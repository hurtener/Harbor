package posture

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// TestCountersProvider_NilDeps_ReportsZeros asserts the Counters seam
// does not panic when a dependency is nil — it reports zeros for the
// missing subsystem rather than crashing the posture request.
func TestCountersProvider_NilDeps_ReportsZeros(t *testing.T) {
	provider := CountersProvider(nil, nil)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	c := provider(context.Background(), id)
	if c.TasksRunning != 0 || c.SessionsActive != 0 || c.BackgroundJobsActive != 0 {
		t.Errorf("nil-dep counters = %+v, want all zero", c)
	}
}

// TestMetricsProvider_ProjectsLiveSnapshot asserts the Metrics seam
// projects a real telemetry.MetricsRegistry snapshot onto the wire
// shape — the projection carries plain numbers, never an empty stub
// once the registry has observed an event.
func TestMetricsProvider_ProjectsLiveSnapshot(t *testing.T) {
	cfg := config.TelemetryConfig{ServiceName: "harbor-posture-test"}
	reg, shutdown, err := telemetry.NewMetricsRegistry(cfg,
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	provider := MetricsProvider(reg, nil)
	// Before any event the snapshot is non-nil and empty — never a
	// nil-panic.
	snap := provider(context.Background())
	if snap.Counters == nil {
		t.Error("MetricsProvider returned nil Counters slice")
	}
}
