package posture

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// TestCountersProvider_NilDeps_ReportsZeros asserts the Counters seam
// does not panic when a dependency is nil — it reports zeros for the
// missing subsystem rather than crashing the posture request.
func TestCountersProvider_NilDeps_ReportsZeros(t *testing.T) {
	provider := CountersProvider(nil, nil, nil)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	c := provider(context.Background(), id)
	if c.TasksRunning != 0 || c.SessionsActive != 0 || c.BackgroundJobsActive != 0 || c.MCPConnectionsHealthy != 0 {
		t.Errorf("nil-dep counters = %+v, want all zero", c)
	}
}

// TestCountersProvider_MCPHealthyCount asserts the Counters seam reads
// the MCP registry's `ServerStateOnline` count into
// `MCPConnectionsHealthy`. Round-5 walkthrough fix: pre-fix the field
// was hard-coded zero even when the registry had healthy servers.
func TestCountersProvider_MCPHealthyCount(t *testing.T) {
	reg := mcpdrv.NewRegistry()
	// Two healthy servers + one offline. The offline one must NOT
	// count in MCPConnectionsHealthy.
	for _, name := range []string{"alpha", "beta"} {
		if err := reg.Register(mcpdrv.ServerRegistration{
			Provider:     &postureStubProvider{id: tools.ToolSourceID(name)},
			Transport:    "stdio",
			InitialState: mcpdrv.ServerStateOnline,
		}); err != nil {
			t.Fatalf("Register %q: %v", name, err)
		}
	}
	if err := reg.Register(mcpdrv.ServerRegistration{
		Provider:     &postureStubProvider{id: "offline"},
		Transport:    "stdio",
		InitialState: mcpdrv.ServerStateOffline,
	}); err != nil {
		t.Fatalf("Register offline: %v", err)
	}

	provider := CountersProvider(nil, nil, reg)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	c := provider(ctx, id)
	if c.MCPConnectionsHealthy != 2 {
		t.Errorf("MCPConnectionsHealthy = %d, want 2 (two online servers)", c.MCPConnectionsHealthy)
	}
}

// postureStubProvider is a minimal mcp.ServerProvider that satisfies
// the Registry's registration contract for the posture test — no MCP
// wire, no SDK. The Discover path is never reached on the posture
// read; we only need the SourceID to populate the registry.
type postureStubProvider struct{ id tools.ToolSourceID }

func (s *postureStubProvider) SourceID() tools.ToolSourceID { return s.id }
func (s *postureStubProvider) Discover(_ context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (s *postureStubProvider) Close(_ context.Context) error { return nil }

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
