package prometheus_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"

	// Self-registration under test.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"
)

func cfg() config.TelemetryConfig {
	return config.TelemetryConfig{LogFormat: "json", LogLevel: "info", ServiceName: "harbor-test"}
}

// TestPrometheusDriver_SelfRegisters proves the init() registration
// fired: NewMetricsRegistry with an empty OTelEndpoint selects the
// prometheus driver and the result is PrometheusHandler-capable.
func TestPrometheusDriver_SelfRegisters(t *testing.T) {
	reg, shutdown, err := telemetry.NewMetricsRegistry(cfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if _, err := telemetry.PrometheusHandler(reg); err != nil {
		t.Fatalf("PrometheusHandler on a prometheus-backed registry: %v", err)
	}
}

// TestPrometheusDriver_ExplicitSelection proves WithMetricExporterDriver
// can force the prometheus driver regardless of OTelEndpoint.
func TestPrometheusDriver_ExplicitSelection(t *testing.T) {
	c := cfg()
	c.OTelEndpoint = "127.0.0.1:4317" // would otherwise pick otlpmetric
	reg, shutdown, err := telemetry.NewMetricsRegistry(c,
		telemetry.WithMetricExporterDriver("prometheus"))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if _, err := telemetry.PrometheusHandler(reg); err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
}

// TestPrometheusDriver_IndependentRegistries proves each
// NewMetricsRegistry gets its OWN prometheus.Registry — building two
// in one process does not collide on the global default registerer
// (which would panic on a duplicate collector registration).
func TestPrometheusDriver_IndependentRegistries(t *testing.T) {
	reg1, shutdown1, err := telemetry.NewMetricsRegistry(cfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry #1: %v", err)
	}
	defer func() { _ = shutdown1(context.Background()) }()

	reg2, shutdown2, err := telemetry.NewMetricsRegistry(cfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry #2 (would panic if it shared the global registerer): %v", err)
	}
	defer func() { _ = shutdown2(context.Background()) }()

	if _, err := telemetry.PrometheusHandler(reg1); err != nil {
		t.Fatalf("PrometheusHandler reg1: %v", err)
	}
	if _, err := telemetry.PrometheusHandler(reg2); err != nil {
		t.Fatalf("PrometheusHandler reg2: %v", err)
	}
}
