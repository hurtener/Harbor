// Package telemetry is the public SDK facade over Harbor's
// internal/telemetry package — the redactor-mandatory structured
// Logger, the bus→metrics bridge, and the bus→tracer bridge that
// derive Harbor's three signals from the one canonical event stream
// (RFC §3.6, §6.14). Alias-based re-exports only: no
// behavior lives here. The
// observe-an-embedded-runtime recipe's manual-composition path is
// consumer-facing and flushed the names out. Exporter driver
// registration, the slog handler internals, and the cardinality lint
// are deliberately private.
package telemetry

import (
	internal "github.com/hurtener/Harbor/internal/telemetry"
)

// Logger / metrics / tracing vocabulary — aliases of the internal types.
type (
	// Logger is the redactor-mandatory structured logger; Error calls
	// pair a runtime.error bus event with the slog record.
	Logger = internal.Logger
	// Option customises New (bus emitter, writer).
	Option = internal.Option
	// BusEmitter is the Logger's runtime.error emission seam
	// (sdk/telemetry/eventbus provides the production adapter).
	BusEmitter = internal.BusEmitter
	// MetricsRegistry is the low-cardinality metrics projection of the
	// event stream.
	MetricsRegistry = internal.MetricsRegistry
	// MetricsOption customises NewMetricsRegistry.
	MetricsOption = internal.MetricsOption
	// Tracer turns lifecycle event pairs into OTel spans.
	Tracer = internal.Tracer
	// TracerOption customises NewTracer.
	TracerOption = internal.TracerOption
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrLoggerNotConfigured — invalid log format/level configuration.
	ErrLoggerNotConfigured = internal.ErrLoggerNotConfigured
	// ErrRedactorMissing — New was called with a nil redactor; there
	// is no unredacted-logger mode.
	ErrRedactorMissing = internal.ErrRedactorMissing
	// ErrMetricsNotConfigured — the metrics registry config is invalid.
	ErrMetricsNotConfigured = internal.ErrMetricsNotConfigured
	// ErrMetricExporterUnknown — the named metric exporter driver is
	// not registered (blank-import sdk/drivers/prod).
	ErrMetricExporterUnknown = internal.ErrMetricExporterUnknown
	// ErrPrometheusHandlerUnavailable — the registry was not built
	// with the prometheus exporter.
	ErrPrometheusHandlerUnavailable = internal.ErrPrometheusHandlerUnavailable
	// ErrBridgeMisconfigured — BridgeBusToMetrics missing a mandatory
	// dependency.
	ErrBridgeMisconfigured = internal.ErrBridgeMisconfigured
	// ErrTraceBridgeMisconfigured — BridgeBusToTracer missing a
	// mandatory dependency.
	ErrTraceBridgeMisconfigured = internal.ErrTraceBridgeMisconfigured
)

// New constructs the redactor-mandatory Logger.
var New = internal.New

// WithBusEmitter pairs Logger.Error with runtime.error bus events.
var WithBusEmitter = internal.WithBusEmitter

// WithWriter redirects the Logger's slog output (tests, captures).
var WithWriter = internal.WithWriter

// NewMetricsRegistry builds the metrics registry plus its shutdown
// func.
var NewMetricsRegistry = internal.NewMetricsRegistry

// BridgeBusToMetrics folds bus events into the registry's counters;
// returns the bridge's stop func.
var BridgeBusToMetrics = internal.BridgeBusToMetrics

// PrometheusHandler mounts the registry as an http.Handler when the
// prometheus exporter driver is configured.
var PrometheusHandler = internal.PrometheusHandler

// NewTracer builds the OTel tracer plus its shutdown func.
var NewTracer = internal.NewTracer

// BridgeBusToTracer turns lifecycle event pairs into spans; returns
// the bridge's stop func.
var BridgeBusToTracer = internal.BridgeBusToTracer

// DefaultTraceBridgeFilter scopes the trace bridge to the canonical
// lifecycle event pairs so a chatty bus never becomes span flood.
var DefaultTraceBridgeFilter = internal.DefaultTraceBridgeFilter
