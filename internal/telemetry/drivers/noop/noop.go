// Package noop is the default telemetry.SpanExporter driver — it
// drops every span without error.
//
// The noop driver is selected when TelemetryConfig.OTelEndpoint is
// empty (no collector configured). Spans are STILL created by the
// Tracer when this driver is active: span creation is what makes
// in-process trace propagation (traceparent / _meta / env carriers)
// work even with no exporter. The noop driver simply means the spans
// are never shipped anywhere — there is no collector to ship them to.
//
// The driver self-registers from init() under the name "noop"; pull
// it in via blank import at the binary entry point:
//
//	import _ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
package noop

import (
	"context"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// driverName is the registry key for this driver.
const driverName = "noop"

func init() {
	telemetry.RegisterExporter(driverName, exporter{})
}

// exporter satisfies telemetry.SpanExporter. It is stateless — a
// single value is shared across every NewTracer call (D-025: no
// per-construction mutable state).
type exporter struct{}

// Exporter returns a span exporter that discards every span. The OTel
// SDK's tracetest.NoopExporter is the canonical "drop everything"
// exporter — it implements sdktrace.SpanExporter and never errors,
// which is exactly the noop driver's contract. cfg is ignored: the
// noop driver has nothing to configure.
func (exporter) Exporter(_ context.Context, _ config.TelemetryConfig) (sdktrace.SpanExporter, error) {
	return tracetest.NewNoopExporter(), nil
}
