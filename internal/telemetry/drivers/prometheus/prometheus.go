// Package prometheus is the built-in Prometheus telemetry.MetricExporter
// driver — it exposes Harbor's metrics on a pull /metrics endpoint in
// the Prometheus text exposition format.
//
// RFC §6.14 promotes a built-in Prometheus /metrics endpoint to V1 for
// self-hosted setups (a popular operator preference; resolves brief 06
// Q-2). This driver is the default — telemetry.NewMetricsRegistry
// selects it when TelemetryConfig.OTelEndpoint is empty, because the
// Prometheus path needs no collector: the metrics are pulled by a
// Prometheus server scraping the /metrics handler.
//
// # Per-registry scrape registry — NOT the global default
//
// The OTel Prometheus exporter defaults to registering with
// prometheus.DefaultRegisterer (process-global mutable state). This
// driver deliberately does NOT use that: each call to Reader builds a
// FRESH prometheus.Registry and registers the exporter's collector
// with it via WithRegisterer. That keeps every telemetry.MetricsRegistry
// independent — two registries in one process (a test running N of
// them; a future multi-listener setup) do not collide on the global
// registerer, and the D-025 concurrent-reuse contract holds. The fresh
// registry is handed back through the PromGatherer contract so
// telemetry.PrometheusHandler can build a promhttp handler from it.
//
// The driver self-registers from init() under the name "prometheus";
// pull it in via blank import at the binary entry point:
//
//	import _ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"
package prometheus

import (
	"context"
	"fmt"

	promclient "github.com/prometheus/client_golang/prometheus"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// driverName is the registry key for this driver.
const driverName = "prometheus"

func init() {
	telemetry.RegisterMetricExporter(driverName, exporter{})
}

// exporter satisfies telemetry.MetricExporter. Stateless — one value
// shared across every NewMetricsRegistry call (D-025: no
// per-construction mutable state on the driver itself; the constructed
// reader + registry are per-MetricsRegistry).
type exporter struct{}

// Reader constructs a Prometheus metric.Reader backed by a fresh,
// per-registry prometheus.Registry. cfg is accepted for interface
// parity but the Prometheus driver has nothing to configure from it —
// the pull endpoint needs no endpoint, no credentials.
//
// The returned reader satisfies telemetry.PromGatherer so
// telemetry.PrometheusHandler can recover the scrape registry.
func (exporter) Reader(_ context.Context, _ config.TelemetryConfig) (sdkmetric.Reader, error) {
	registry := promclient.NewRegistry()
	exp, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("prometheus: exporter construction failed: %w", err)
	}
	return &reader{Exporter: exp, registry: registry}, nil
}

// reader wraps the OTel Prometheus exporter (which is itself a
// sdkmetric.Reader) and carries the per-registry prometheus.Registry so
// it can satisfy telemetry.PromGatherer. Embedding *otelprom.Exporter
// promotes the full sdkmetric.Reader method set; the only added method
// is PrometheusGatherer.
type reader struct {
	*otelprom.Exporter
	registry *promclient.Registry
}

// PrometheusGatherer returns the per-registry prometheus.Registry the
// OTel exporter's collector is registered with — telemetry.PrometheusHandler
// builds a promhttp handler from it. Satisfies telemetry.PromGatherer.
func (r *reader) PrometheusGatherer() promclient.Gatherer {
	return r.registry
}
