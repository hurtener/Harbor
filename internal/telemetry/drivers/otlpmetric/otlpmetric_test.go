package otlpmetric_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"

	// Self-registration under test.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
)

func cfg() config.TelemetryConfig {
	return config.TelemetryConfig{LogFormat: "json", LogLevel: "info", ServiceName: "harbor-test"}
}

// TestOTLPMetricDriver_SelfRegisters proves the init() registration
// fired: NewMetricsRegistry with a non-empty OTelEndpoint selects the
// otlpmetric driver. The OTLP/gRPC exporter connects lazily, so no live
// collector is needed at construction.
func TestOTLPMetricDriver_SelfRegisters(t *testing.T) {
	c := cfg()
	c.OTelEndpoint = "127.0.0.1:4317"
	reg, shutdown, err := telemetry.NewMetricsRegistry(c)
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if reg == nil {
		t.Fatal("NewMetricsRegistry returned a nil registry")
	}
}

// TestOTLPMetricDriver_ExplicitSelection proves WithMetricExporterDriver
// can force the otlpmetric driver even with an empty OTelEndpoint —
// which then fails loudly, because the otlpmetric driver requires an
// endpoint (no silent fall-back, CLAUDE.md §13).
func TestOTLPMetricDriver_ExplicitSelectionWithoutEndpointFailsLoudly(t *testing.T) {
	_, _, err := telemetry.NewMetricsRegistry(cfg(),
		telemetry.WithMetricExporterDriver("otlpmetric"))
	if err == nil {
		t.Fatal("expected a loud error for the otlpmetric driver with no endpoint, got nil")
	}
}

// TestOTLPMetricDriver_NoPullSurface proves an otlpmetric-backed
// registry has no Prometheus pull surface — PrometheusHandler fails
// loudly with ErrPrometheusHandlerUnavailable.
func TestOTLPMetricDriver_NoPullSurface(t *testing.T) {
	c := cfg()
	c.OTelEndpoint = "127.0.0.1:4317"
	reg, shutdown, err := telemetry.NewMetricsRegistry(c)
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	if _, err := telemetry.PrometheusHandler(reg); !errors.Is(err, telemetry.ErrPrometheusHandlerUnavailable) {
		t.Fatalf("want ErrPrometheusHandlerUnavailable, got %v", err)
	}
}
