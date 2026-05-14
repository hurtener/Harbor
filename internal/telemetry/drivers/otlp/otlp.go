// Package otlp is the OTLP/gRPC telemetry.SpanExporter driver — it
// ships spans to an external OpenTelemetry collector (Jaeger, an OTLP
// collector, an observability vendor's endpoint).
//
// The otlp driver is selected when TelemetryConfig.OTelEndpoint is
// non-empty. The endpoint is a host:port the collector's OTLP/gRPC
// receiver listens on. The exporter connects LAZILY — New returns a
// ready exporter without a live collector, and the first span batch
// flush is when a connection is attempted. That keeps NewTracer fast
// and lets a collector come up after the Runtime.
//
// V1 ships the insecure (no-TLS) transport: the collector is expected
// to be a sidecar or same-trust-zone process. TLS to a remote
// collector is post-V1 (it needs a cert-config surface that does not
// exist yet — adding one is an RFC change, not a driver tweak).
//
// The driver self-registers from init() under the name "otlp"; pull
// it in via blank import at the binary entry point:
//
//	import _ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"
package otlp

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// driverName is the registry key for this driver.
const driverName = "otlp"

func init() {
	telemetry.RegisterExporter(driverName, exporter{})
}

// exporter satisfies telemetry.SpanExporter. Stateless — one value
// shared across every NewTracer call (D-025: no per-construction
// mutable state on the driver itself; the constructed
// otlptrace.Exporter is per-Tracer).
type exporter struct{}

// Exporter constructs an OTLP/gRPC span exporter targeting
// cfg.OTelEndpoint. Fails loudly when the endpoint is empty — the
// otlp driver is only selected when an endpoint is configured, so an
// empty endpoint here is a misconfiguration, not a "fall back to
// noop" case (silent degradation is forbidden, CLAUDE.md §13).
func (exporter) Exporter(ctx context.Context, cfg config.TelemetryConfig) (sdktrace.SpanExporter, error) {
	if cfg.OTelEndpoint == "" {
		return nil, fmt.Errorf("otlp: otel_endpoint is empty (the otlp driver requires a collector endpoint)")
	}
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTelEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp: exporter construction failed for endpoint %q: %w", cfg.OTelEndpoint, err)
	}
	return exp, nil
}
