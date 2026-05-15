package otlp_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"
)

func TestOTLPDriver_SelfRegisters_SelectedByEndpoint(t *testing.T) {
	// A non-empty OTelEndpoint selects the "otlp" driver. The
	// OTLP/gRPC exporter connects lazily, so construction succeeds
	// with no live collector.
	cfg := config.TelemetryConfig{
		LogFormat:    "json",
		LogLevel:     "info",
		ServiceName:  "harbor-test",
		OTelEndpoint: "127.0.0.1:4317",
	}
	tr, shutdown, err := telemetry.NewTracer(cfg)
	if err != nil {
		t.Fatalf("NewTracer with OTLP endpoint: %v", err)
	}
	if tr == nil {
		t.Fatal("nil *Tracer")
	}
	// Shutdown with a bounded deadline: no live collector means the
	// flush attempt may time out, but Shutdown must still return.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

func TestOTLPDriver_ForcedDriver_ConstructsExporter(t *testing.T) {
	// Force the otlp driver explicitly with a syntactically-valid
	// endpoint — construction must succeed (lazy connect).
	cfg := config.TelemetryConfig{
		ServiceName:  "harbor-test",
		OTelEndpoint: "collector.example.com:4317",
	}
	tr, shutdown, err := telemetry.NewTracer(cfg, telemetry.WithExporterDriver("otlp"))
	if err != nil {
		t.Fatalf("NewTracer forced otlp: %v", err)
	}
	if tr == nil {
		t.Fatal("nil *Tracer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx)
}

func TestOTLPDriver_EmptyEndpoint_FailsLoudly(t *testing.T) {
	// Forcing the otlp driver with an empty endpoint must fail loudly
	// — not silently degrade to noop (CLAUDE.md §13).
	cfg := config.TelemetryConfig{ServiceName: "harbor-test"}
	_, _, err := telemetry.NewTracer(cfg, telemetry.WithExporterDriver("otlp"))
	if err == nil {
		t.Fatal("NewTracer forced otlp with empty endpoint: want error, got nil")
	}
}
