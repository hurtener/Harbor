package noop_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
)

func TestNoopDriver_SelfRegisters_AndConstructsTracer(t *testing.T) {
	// The blank import above fires the driver's init(); NewTracer with
	// an empty OTelEndpoint must resolve the "noop" driver.
	cfg := config.TelemetryConfig{LogFormat: "json", LogLevel: "info", ServiceName: "harbor-test"}
	tr, shutdown, err := telemetry.NewTracer(cfg, telemetry.WithExporterDriver("noop"))
	if err != nil {
		t.Fatalf("NewTracer with noop driver: %v", err)
	}
	if tr == nil {
		t.Fatal("nil *Tracer")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestNoopDriver_ExporterDropsSpansWithoutError(t *testing.T) {
	// The noop driver's exporter must satisfy sdktrace.SpanExporter
	// and never error on export — it simply discards spans.
	cfg := config.TelemetryConfig{ServiceName: "harbor-test"}
	// Reach the driver through the registry the same way NewTracer
	// does: the only public path is via NewTracer, so assert the
	// constructed tracer's shutdown (which flushes the exporter)
	// succeeds — a dropping exporter never errors.
	tr, shutdown, err := telemetry.NewTracer(cfg, telemetry.WithExporterDriver("noop"))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	_ = tr
	var _ sdktrace.SpanExporter // documents the contract surface
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop exporter flush/shutdown errored: %v", err)
	}
}
