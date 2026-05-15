// Package telemetry's metrics surface (Phase 56) adds OpenTelemetry
// metrics on top of the Phase 04 Logger and the Phase 55 Tracer.
//
// The load-bearing decision — the same one Phase 55 applied to spans —
// is that metrics are a DERIVATION of the event bus, not a parallel
// instrumentation path. Subsystems emit events.Event records;
// MetricsRegistry.RegisterEvent is the single bridge that turns an
// event into a metric increment. There is deliberately no public
// Counter / Gauge / Meter accessor on MetricsRegistry — a contributor
// cannot sprinkle meter.Int64Counter(...) calls across subsystems and
// grow a second metrics channel (brief 06 §1).
//
// # The cardinality firewall (brief 06 "metrics cardinality footgun")
//
// The predecessor's docs warn "Never tag metrics by trace_id". Harbor
// makes that structural: RegisterEvent reads ev.Type and the two
// reserved, bounded, low-cardinality ev.Extra keys ("producer",
// "node") — and NOTHING from ev.Identity. The run quadruple (which is
// where RunID / TraceID-shaped values live) is physically unreachable
// from a metric label here. The static cardinality-lint in
// internal/telemetry/cardinalitylint is the CI gate that keeps any
// future hand-rolled instrument honest; this file is the production
// boundary that is closed by construction.
//
// # Reusable artifact (D-025)
//
// A *MetricsRegistry is built once at boot via NewMetricsRegistry,
// then shared across every emit path. It is immutable after
// construction and safe for concurrent use; the D-025 concurrent-reuse
// contract is enforced by TestConcurrentReuse_MetricsRegistry (N≥100
// goroutines, one shared instance, under -race). The OTel SDK's
// Int64Counter is itself safe for concurrent Add; the registry holds
// only read-only references after construction.
//
// # The metric exporter sits behind the §4.4 driver seam
//
// Mirroring the Phase 55 SpanExporter seam exactly: the otlpmetric
// driver (OTLP/gRPC, the default) and the prometheus driver (the
// built-in /metrics pull endpoint) self-register from their init();
// the factory below dispatches by name. NewMetricsRegistry does NOT
// touch any OTel global — it builds a private MeterProvider and the
// registry is passed explicitly, never via otel.SetMeterProvider.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrMetricsNotConfigured — NewMetricsRegistry received an invalid
	// TelemetryConfig (e.g. an empty ServiceName). Phase 02 validates
	// config upstream, but the constructor must not trust it.
	ErrMetricsNotConfigured = errors.New("telemetry: metrics registry not configured")
	// ErrMetricExporterUnknown — an explicitly-configured metric
	// exporter driver is not in the registry. The formatted message
	// lists the registered driver names so a misconfiguration is
	// obvious.
	ErrMetricExporterUnknown = errors.New("telemetry: unknown metric exporter driver")
	// ErrPrometheusHandlerUnavailable — PrometheusHandler was called on
	// a *MetricsRegistry that was not built with the prometheus
	// exporter driver active. The pull handler needs the prometheus
	// driver's Gatherer; an OTLP-backed registry has no pull surface.
	ErrPrometheusHandlerUnavailable = errors.New("telemetry: prometheus handler unavailable (registry not built with the prometheus exporter)")
)

// meterName is the instrumentation scope name handed to the OTel SDK
// when the MetricsRegistry obtains its Meter. It identifies Harbor as
// the instrumentation library in exported metrics.
const meterName = "github.com/hurtener/Harbor/internal/telemetry"

// Canonical metric + label names. Single-sourced here so a future
// instrument addition does not re-spell them and the cardinality-lint
// has a stable target set.
const (
	// metricEventsTotal is the Phase 56 core counter: total
	// events.Event records observed by the MetricsRegistry.
	metricEventsTotal = "harbor_events_total"

	// labelEventType / labelProducer / labelNode are the ONLY three
	// labels harbor_events_total carries. All three are bounded and
	// low-cardinality by construction: event_type is drawn from the
	// events canonical registry; producer / node are read from the
	// reserved, bounded events.Event.Extra keys. There is deliberately
	// no run_id / trace_id / task_id label — see the cardinality
	// firewall note in the package doc.
	labelEventType = "event_type"
	labelProducer  = "producer"
	labelNode      = "node"

	// extraKeyProducer / extraKeyNode are the reserved events.Event.Extra
	// keys RegisterEvent reads. The events.Event doc reserves Extra for
	// "Phase 56's bounded low-cardinality metric labels"; these are
	// those keys.
	extraKeyProducer = "producer"
	extraKeyNode     = "node"

	// producerUnknown is the producer label value used when an event
	// carries no Extra["producer"]. A stable sentinel keeps the label
	// set bounded — an absent producer is one series, not unbounded
	// empties.
	producerUnknown = "unknown"
)

// MetricExporter is the §4.4 seam. A driver constructs and returns a
// ready OTel SDK metric.Reader for the given config. Drivers
// (otlpmetric, prometheus) self-register from their init() via
// RegisterMetricExporter; callers depend only on this package.
//
// The prometheus driver's Reader additionally satisfies the
// PromGatherer interface below so PrometheusHandler can recover its
// scrape registry; the otlpmetric driver's Reader does not (an
// OTLP-push registry has no pull surface), and PrometheusHandler
// returns ErrPrometheusHandlerUnavailable for it.
type MetricExporter interface {
	// Reader returns a ready metric.Reader. ctx scopes any connection
	// setup; cfg carries the OTLP endpoint / service name.
	Reader(ctx context.Context, cfg config.TelemetryConfig) (sdkmetric.Reader, error)
}

// PromGatherer is the contract the prometheus driver's metric.Reader
// satisfies so PrometheusHandler can recover the prometheus.Gatherer it
// builds a promhttp handler from. This is NOT a §4.4 "Supports*"
// capability smell — it is a single driver-specific pull surface that
// only one of the two drivers can have (an OTLP-push reader has no
// scrape registry), so the absence is a genuine fact, not an optional
// feature toggle. The type assertion in PrometheusHandler is how the
// fact is discovered.
//
// The method is exported because the prometheus driver lives in a
// sub-package (internal/telemetry/drivers/prometheus) and must satisfy
// the interface across the package boundary; only the prometheus
// driver is expected to implement it.
type PromGatherer interface {
	// PrometheusGatherer returns the prometheus.Gatherer the driver's
	// scrape registry is registered with.
	PrometheusGatherer() prometheus.Gatherer
}

// metricExporterRegistry is the write-once-read-many driver registry.
// It is package-level mutable state, permitted under CLAUDE.md §4.4 /
// §5 as a driver registry (the §4.4 seam exception, same shape as the
// Phase 55 span-exporter registry).
var (
	metricExporterMu       sync.RWMutex
	metricExporterRegistry = map[string]MetricExporter{}
)

// RegisterMetricExporter installs a MetricExporter driver under name.
// Called from a driver package's init(). Re-registering the same name
// is a no-op (first registration wins — deterministic blank-import
// ordering is not guaranteed, so a no-op is the safe choice);
// registering an empty name or a nil exporter panics, because a silent
// accept would defeat the registry's single-source-of-truth invariant.
func RegisterMetricExporter(name string, e MetricExporter) {
	if name == "" {
		panic("telemetry: RegisterMetricExporter called with empty name")
	}
	if e == nil {
		panic("telemetry: RegisterMetricExporter called with nil MetricExporter")
	}
	metricExporterMu.Lock()
	defer metricExporterMu.Unlock()
	if _, exists := metricExporterRegistry[name]; exists {
		return
	}
	metricExporterRegistry[name] = e
}

// registeredMetricExporters returns a sorted snapshot of registered
// driver names — used to build the ErrMetricExporterUnknown message.
func registeredMetricExporters() []string {
	metricExporterMu.RLock()
	out := make([]string, 0, len(metricExporterRegistry))
	for name := range metricExporterRegistry {
		out = append(out, name)
	}
	metricExporterMu.RUnlock()
	sort.Strings(out)
	return out
}

// lookupMetricExporter returns the registered driver, or a wrapped
// ErrMetricExporterUnknown listing the registered names.
func lookupMetricExporter(name string) (MetricExporter, error) {
	metricExporterMu.RLock()
	e, ok := metricExporterRegistry[name]
	metricExporterMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)",
			ErrMetricExporterUnknown, name, registeredMetricExporters())
	}
	return e, nil
}

// driverOTLPMetric / driverPrometheus are the canonical metric-exporter
// driver names. The prometheus driver is the default when no OTLP
// endpoint is configured (pull-only, no collector needed); otlpmetric
// is selected when TelemetryConfig.OTelEndpoint is non-empty.
const (
	driverOTLPMetric = "otlpmetric"
	driverPrometheus = "prometheus"
)

// metricsConfig holds construction-time knobs assembled from
// MetricsOption values. Unexported — the option functions are the only
// way to populate it.
type metricsConfig struct {
	// readerOverride, when non-nil, bypasses the driver registry.
	// Test-only seam: a test injects a sdkmetric.ManualReader so
	// recorded metrics are observable without a collector. Production
	// callers never set it.
	readerOverride sdkmetric.Reader
	// exporterName, when non-empty, forces a specific registered driver
	// regardless of cfg.OTelEndpoint. Used by tests to exercise the
	// ErrMetricExporterUnknown path deterministically and to select a
	// driver explicitly.
	exporterName string
}

// MetricsOption configures the MetricsRegistry at construction.
type MetricsOption func(*metricsConfig)

// WithMetricReader injects a pre-built sdkmetric.Reader, bypassing the
// driver registry. Test-only seam — a test uses a sdkmetric.ManualReader
// so recorded metrics are observable. Documented inline; no production
// caller uses it.
func WithMetricReader(r sdkmetric.Reader) MetricsOption {
	return func(c *metricsConfig) {
		if r != nil {
			c.readerOverride = r
		}
	}
}

// WithMetricExporterDriver forces a specific registered metric-exporter
// driver by name, regardless of cfg.OTelEndpoint. Used by tests to
// exercise the ErrMetricExporterUnknown path deterministically and to
// select the prometheus driver explicitly. Production callers rely on
// the OTelEndpoint-based selection in NewMetricsRegistry.
func WithMetricExporterDriver(name string) MetricsOption {
	return func(c *metricsConfig) {
		c.exporterName = name
	}
}

// MetricsRegistry is the canonical OTel metrics wrapper. Built once at
// boot via NewMetricsRegistry; safe for concurrent use; immutable
// after construction (D-025).
//
// The struct holds only read-only references after construction: the
// SDK MeterProvider, the reader (kept so PrometheusHandler can recover
// the prometheus driver's Gatherer), and the core Int64Counter
// instruments. No field mutates post-construction; per-call state
// (the attribute set for one event) is built on the stack in
// RegisterEvent, never stored on the registry.
type MetricsRegistry struct {
	provider    *sdkmetric.MeterProvider
	reader      sdkmetric.Reader
	eventsTotal metric.Int64Counter
}

// NewMetricsRegistry constructs a MetricsRegistry from validated
// config. The returned shutdown func flushes and shuts down the reader
// + provider; callers defer it at process teardown.
//
// Exporter selection: an empty cfg.OTelEndpoint selects the prometheus
// driver (pull-only /metrics; no collector needed); a non-empty
// endpoint selects the otlpmetric driver (OTLP/gRPC push).
// WithMetricExporterDriver / WithMetricReader override this for tests.
//
// NewMetricsRegistry touches NO OTel global — it builds a private
// MeterProvider and the registry is passed to callers explicitly. A
// global MeterProvider would be a parallel, ambient metrics channel;
// the explicit-handle discipline keeps metrics a derivation of the
// event bus (the registry is wired where events are emitted).
func NewMetricsRegistry(cfg config.TelemetryConfig, opts ...MetricsOption) (*MetricsRegistry, func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, nil, fmt.Errorf("%w: service_name is empty", ErrMetricsNotConfigured)
	}

	mc := &metricsConfig{}
	for _, opt := range opts {
		opt(mc)
	}

	ctx := context.Background()

	var reader sdkmetric.Reader
	switch {
	case mc.readerOverride != nil:
		reader = mc.readerOverride
	default:
		name := mc.exporterName
		if name == "" {
			if cfg.OTelEndpoint == "" {
				name = driverPrometheus
			} else {
				name = driverOTLPMetric
			}
		}
		driver, err := lookupMetricExporter(name)
		if err != nil {
			return nil, nil, err
		}
		reader, err = driver.Reader(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: metric exporter %q construction failed: %w", name, err)
		}
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(harborMetricResource(cfg)),
	)

	meter := provider.Meter(meterName)
	eventsTotal, err := meter.Int64Counter(
		metricEventsTotal,
		metric.WithDescription("Total events.Event records observed by the Harbor MetricsRegistry, labelled event_type / producer / node."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		// Fail loudly — a counter the SDK refuses to create is a
		// construction bug, not a degradable condition (CLAUDE.md §5).
		return nil, nil, fmt.Errorf("telemetry: core counter %q construction failed: %w", metricEventsTotal, err)
	}

	r := &MetricsRegistry{
		provider:    provider,
		reader:      reader,
		eventsTotal: eventsTotal,
	}

	shutdown := func(ctx context.Context) error {
		// ForceFlush drains pending metric data; Shutdown stops the
		// provider + reader. Both are idempotent on the SDK side.
		if err := provider.ForceFlush(ctx); err != nil {
			return fmt.Errorf("telemetry: metrics flush failed: %w", err)
		}
		if err := provider.Shutdown(ctx); err != nil {
			return fmt.Errorf("telemetry: metrics shutdown failed: %w", err)
		}
		return nil
	}
	return r, shutdown, nil
}

// harborMetricResource builds the OTel resource describing this Harbor
// process for metrics. Kept minimal and identical in spirit to the
// Phase 55 trace resource: the service name is the one identifying
// attribute; resource.Default() is intentionally NOT merged in (it
// triggers host/OS detection that is noise for a runtime SDK).
func harborMetricResource(cfg config.TelemetryConfig) *resource.Resource {
	return resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
	)
}

// RegisterEvent increments the canonical harbor_events_total counter
// from ev. The labels are derived ONLY from:
//
//   - event_type — ev.Type, verbatim. RegisterEvent does NOT
//     re-validate against the events canonical registry; the bus
//     already did at Publish time, and a metric for an
//     otherwise-unknown type is still useful signal.
//   - producer — ev.Extra["producer"], or "unknown" when absent.
//   - node — ev.Extra["node"], or "" when absent.
//
// RegisterEvent reads NO field of ev.Identity. The run quadruple —
// tenant_id / user_id / session_id / run_id — never reaches a metric
// label. This is the cardinality firewall (brief 06): a developer
// cannot accidentally tag a metric by RunID / TraceID here because
// there is no code path that touches ev.Identity. The static
// cardinality-lint (internal/telemetry/cardinalitylint) is the CI gate
// that keeps any future instrument honest.
//
// ctx is honoured for cancellation but RegisterEvent does no I/O — the
// Int64Counter.Add is an in-memory aggregation; the reader exports
// asynchronously.
func (r *MetricsRegistry) RegisterEvent(ctx context.Context, ev events.Event) {
	producer := producerUnknown
	node := ""
	if ev.Extra != nil {
		if p := ev.Extra[extraKeyProducer]; p != "" {
			producer = p
		}
		// node is genuinely optional — an empty node label is a valid,
		// bounded series (events not scoped to a node). No sentinel.
		node = ev.Extra[extraKeyNode]
	}

	r.eventsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String(labelEventType, string(ev.Type)),
		attribute.String(labelProducer, producer),
		attribute.String(labelNode, node),
	))
}

// PrometheusHandler returns the http.Handler that serves the Prometheus
// text exposition format for reg. The Phase 60+ Runtime server mounts
// it at /metrics; RFC §6.14 promotes a built-in Prometheus endpoint to
// V1 for self-hosted setups.
//
// PrometheusHandler returns a wrapped ErrPrometheusHandlerUnavailable
// when reg was not constructed with the prometheus exporter driver
// active — an OTLP-backed registry pushes to a collector and has no
// pull surface. The returned handler is safe for concurrent use
// (promhttp.HandlerFor's handler is).
func PrometheusHandler(reg *MetricsRegistry) (http.Handler, error) {
	if reg == nil {
		return nil, fmt.Errorf("%w: nil registry", ErrPrometheusHandlerUnavailable)
	}
	pg, ok := reg.reader.(PromGatherer)
	if !ok {
		return nil, fmt.Errorf("%w: active reader is %T", ErrPrometheusHandlerUnavailable, reg.reader)
	}
	return promhttp.HandlerFor(pg.PrometheusGatherer(), promhttp.HandlerOpts{}), nil
}
