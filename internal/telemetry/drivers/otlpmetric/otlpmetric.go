// Package otlpmetric is the OTLP/gRPC telemetry.MetricExporter driver —
// it ships metrics to an external OpenTelemetry collector (an OTLP
// collector, an observability vendor's endpoint).
//
// The otlpmetric driver is selected when TelemetryConfig.OTelEndpoint
// is non-empty. The endpoint is a host:port the collector's OTLP/gRPC
// receiver listens on. RFC §6.14 names OTLP as the default metrics
// exporter; telemetry.NewMetricsRegistry routes to this driver whenever
// a collector endpoint is configured (and to the prometheus driver
// otherwise — a pull /metrics endpoint needs no collector).
//
// The exporter connects LAZILY — Reader returns a ready
// sdkmetric.PeriodicReader without a live collector, and the first
// periodic export is when a connection is attempted. That keeps
// NewMetricsRegistry fast and lets a collector come up after the
// Runtime.
//
// V1 ships the insecure (no-TLS) transport: the collector is expected
// to be a sidecar or same-trust-zone process — identical stance to the
// Phase 55 otlp span-exporter driver. TLS to a remote collector is
// post-V1 (it needs a cert-config surface that does not exist yet —
// adding one is an RFC change, not a driver tweak).
//
// The driver self-registers from init() under the name "otlpmetric";
// pull it in via blank import at the binary entry point:
//
//	import _ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
package otlpmetric

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// driverName is the registry key for this driver.
const driverName = "otlpmetric"

func init() {
	telemetry.RegisterMetricExporter(driverName, exporter{})
}

// exporter satisfies telemetry.MetricExporter. Stateless — one value
// shared across every NewMetricsRegistry call (D-025: no
// per-construction mutable state on the driver itself; the constructed
// PeriodicReader is per-MetricsRegistry).
type exporter struct{}

// Reader constructs an OTLP/gRPC metric exporter targeting
// cfg.OTelEndpoint, wrapped in a sdkmetric.PeriodicReader (the
// OTel-recommended push reader: it periodically collects and exports).
// Fails loudly when the endpoint is empty — the otlpmetric driver is
// only selected when an endpoint is configured, so an empty endpoint
// here is a misconfiguration, not a "fall back to prometheus" case
// (silent degradation is forbidden, CLAUDE.md §13).
func (exporter) Reader(ctx context.Context, cfg config.TelemetryConfig) (sdkmetric.Reader, error) {
	if cfg.OTelEndpoint == "" {
		return nil, fmt.Errorf("otlpmetric: otel_endpoint is empty (the otlpmetric driver requires a collector endpoint)")
	}
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTelEndpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlpmetric: exporter construction failed for endpoint %q: %w", cfg.OTelEndpoint, err)
	}
	return sdkmetric.NewPeriodicReader(exp), nil
}
