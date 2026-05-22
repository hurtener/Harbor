// tracing.go — the telemetry package's tracing surface (Phase 55).
// It adds OpenTelemetry spans on top of the Phase 04 Logger. The
// canonical package doc comment is in logger.go.
//
// The load-bearing decision: spans are a DERIVATION of the event bus,
// not a parallel instrumentation path. Subsystems emit events.Event
// records; Tracer.SpanFromEvent is the single bridge that turns an
// event into a span. There is deliberately no public Start method on
// Tracer — a contributor cannot sprinkle tracer.Start(...) across
// subsystems and grow a second observability channel (brief 06 §1).
//
// A *Tracer is built once at boot via NewTracer, then shared across
// every emit path. It is immutable after construction and safe for
// concurrent use; the D-025 concurrent-reuse contract is enforced by
// TestConcurrentReuse_Tracer (N≥100 goroutines, one shared instance,
// under -race).
//
// The span exporter sits behind the §4.4 driver seam: the noop driver
// (default — no collector configured) and the otlp driver (OTLP/gRPC,
// opt-in via TelemetryConfig.OTelEndpoint) self-register from their
// init(); the factory below dispatches by name.

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrTracerNotConfigured — NewTracer received an invalid
	// TelemetryConfig (e.g. an empty ServiceName). Phase 02 validates
	// config upstream, but the constructor must not trust it.
	ErrTracerNotConfigured = errors.New("telemetry: tracer not configured")
	// ErrExporterUnknown — an explicitly-configured exporter driver is
	// not in the registry. The formatted message lists the registered
	// driver names so a misconfiguration is obvious.
	ErrExporterUnknown = errors.New("telemetry: unknown span exporter driver")
)

// tracerName is the instrumentation scope name handed to the OTel SDK
// when the Tracer obtains its trace.Tracer. It identifies Harbor as
// the instrumentation library in exported spans.
const tracerName = "github.com/hurtener/Harbor/internal/telemetry"

// SpanExporter is the §4.4 seam. A driver constructs and returns a
// ready OTel SDK trace.SpanExporter for the given config. Drivers
// (noop, otlp) self-register from their init() via RegisterExporter;
// callers depend only on this package.
type SpanExporter interface {
	// Exporter returns a ready trace.SpanExporter. ctx scopes any
	// connection setup; cfg carries the OTLP endpoint / service name.
	Exporter(ctx context.Context, cfg config.TelemetryConfig) (sdktrace.SpanExporter, error)
}

// exporterRegistry is the write-once-read-many driver registry. It is
// package-level mutable state, permitted under CLAUDE.md §4.4 / §5 as
// a driver registry (the §4.4 seam exception, same shape as the audit
// / events / state registries).
var (
	exporterMu       sync.RWMutex
	exporterRegistry = map[string]SpanExporter{}
)

// RegisterExporter installs a SpanExporter driver under name. Called
// from a driver package's init(). Re-registering the same name is a
// no-op (last registration wins is avoided — first wins, deterministic
// under blank-import ordering is not guaranteed so we keep it a
// no-op); registering an empty name panics, because a silent accept
// would defeat the registry's single-source-of-truth invariant.
func RegisterExporter(name string, e SpanExporter) {
	if name == "" {
		panic("telemetry: RegisterExporter called with empty name")
	}
	if e == nil {
		panic("telemetry: RegisterExporter called with nil SpanExporter")
	}
	exporterMu.Lock()
	defer exporterMu.Unlock()
	if _, exists := exporterRegistry[name]; exists {
		return
	}
	exporterRegistry[name] = e
}

// registeredExporters returns a sorted snapshot of registered driver
// names — used to build the ErrExporterUnknown message.
func registeredExporters() []string {
	exporterMu.RLock()
	out := make([]string, 0, len(exporterRegistry))
	for name := range exporterRegistry {
		out = append(out, name)
	}
	exporterMu.RUnlock()
	sort.Strings(out)
	return out
}

// lookupExporter returns the registered driver, or a wrapped
// ErrExporterUnknown listing the registered names.
func lookupExporter(name string) (SpanExporter, error) {
	exporterMu.RLock()
	e, ok := exporterRegistry[name]
	exporterMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %v)",
			ErrExporterUnknown, name, registeredExporters())
	}
	return e, nil
}

// driverNoop / driverOTLP are the canonical exporter driver names.
// The noop driver is the default when no OTLP endpoint is configured;
// otlp is selected when TelemetryConfig.OTelEndpoint is non-empty.
const (
	driverNoop = "noop"
	driverOTLP = "otlp"
)

// tracerConfig holds construction-time knobs assembled from
// TracerOption values. Unexported — the option functions are the only
// way to populate it.
type tracerConfig struct {
	// exporterOverride, when non-nil, bypasses the driver registry.
	// Test-only seam: the integration test injects an in-memory
	// recorder exporter so spans are observable without a live
	// collector. Production callers never set it.
	exporterOverride sdktrace.SpanExporter
	// exporterName, when non-empty, forces a specific registered
	// driver regardless of cfg.OTelEndpoint. Used by tests that want
	// to exercise the ErrExporterUnknown path deterministically.
	exporterName string
}

// TracerOption configures the Tracer at construction.
type TracerOption func(*tracerConfig)

// WithSpanExporter injects a pre-built trace.SpanExporter, bypassing
// the driver registry. Test-only seam — the integration test uses an
// in-memory recorder so emitted spans are observable. Documented
// inline; no production caller uses it.
func WithSpanExporter(e sdktrace.SpanExporter) TracerOption {
	return func(c *tracerConfig) {
		if e != nil {
			c.exporterOverride = e
		}
	}
}

// WithExporterDriver forces a specific registered exporter driver by
// name, regardless of cfg.OTelEndpoint. Used by tests to exercise the
// ErrExporterUnknown path deterministically and to select the noop
// driver explicitly. Production callers rely on the OTelEndpoint-based
// selection in NewTracer.
func WithExporterDriver(name string) TracerOption {
	return func(c *tracerConfig) {
		c.exporterName = name
	}
}

// Tracer is the canonical OTel tracer wrapper. Built once at boot via
// NewTracer; safe for concurrent use; immutable after construction
// (D-025).
//
// The struct holds only read-only references after construction: the
// SDK TracerProvider, the trace.Tracer obtained from it, and the W3C
// TextMapPropagator. No field mutates post-construction; per-span
// state lives in the returned ctx, never on the Tracer.
type Tracer struct {
	provider   *sdktrace.TracerProvider
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
}

// NewTracer constructs a Tracer from validated config. The returned
// shutdown func flushes and shuts down the exporter + provider;
// callers defer it at process teardown.
//
// Exporter selection: an empty cfg.OTelEndpoint selects the noop
// driver (spans are still created so in-process propagation works); a
// non-empty endpoint selects the otlp driver. WithExporterDriver /
// WithSpanExporter override this for tests.
//
// NewTracer sets the OTel global TextMapPropagator to the W3C
// TraceContext propagator. That global is write-once mutable package
// state in the OTel SDK; setting it to the same value on every
// NewTracer call is idempotent, so repeated construction (tests) is
// safe.
func NewTracer(cfg config.TelemetryConfig, opts ...TracerOption) (*Tracer, func(context.Context) error, error) {
	if cfg.ServiceName == "" {
		return nil, nil, fmt.Errorf("%w: service_name is empty", ErrTracerNotConfigured)
	}

	tc := &tracerConfig{}
	for _, opt := range opts {
		opt(tc)
	}

	ctx := context.Background()

	var exporter sdktrace.SpanExporter
	switch {
	case tc.exporterOverride != nil:
		exporter = tc.exporterOverride
	default:
		name := tc.exporterName
		if name == "" {
			if cfg.OTelEndpoint == "" {
				name = driverNoop
			} else {
				name = driverOTLP
			}
		}
		driver, err := lookupExporter(name)
		if err != nil {
			return nil, nil, err
		}
		exporter, err = driver.Exporter(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: exporter %q construction failed: %w", name, err)
		}
	}

	// Production uses the batching span processor (bounded, async,
	// the OTel-recommended default). The test seam (WithSpanExporter)
	// uses the synchronous processor so an in-memory recorder sees
	// spans the instant span.End() returns — no batch-timeout poll in
	// tests.
	var spanProcessor sdktrace.TracerProviderOption
	if tc.exporterOverride != nil {
		spanProcessor = sdktrace.WithSyncer(exporter)
	} else {
		spanProcessor = sdktrace.WithBatcher(exporter)
	}
	provider := sdktrace.NewTracerProvider(
		spanProcessor,
		sdktrace.WithResource(harborResource(cfg)),
	)

	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	// The global propagator is what stdlib-shaped carriers consult.
	// Setting it to the same composite on every NewTracer is
	// idempotent — repeated test construction does not race here
	// because otel.SetTextMapPropagator is internally synchronised.
	otel.SetTextMapPropagator(propagator)

	t := &Tracer{
		provider:   provider,
		tracer:     provider.Tracer(tracerName),
		propagator: propagator,
	}

	shutdown := func(ctx context.Context) error {
		// ForceFlush drains the batcher; Shutdown stops it. Both are
		// idempotent on the SDK side.
		if err := provider.ForceFlush(ctx); err != nil {
			return fmt.Errorf("telemetry: tracer flush failed: %w", err)
		}
		if err := provider.Shutdown(ctx); err != nil {
			return fmt.Errorf("telemetry: tracer shutdown failed: %w", err)
		}
		return nil
	}
	return t, shutdown, nil
}

// harborResource builds the OTel resource describing this Harbor
// process. Kept minimal: the service name is the one identifying
// attribute; richer resource detection is post-V1. resource.Default()
// is intentionally NOT merged in — it triggers host/OS detection that
// is noise for a runtime SDK and slows construction in tests.
func harborResource(cfg config.TelemetryConfig) *resource.Resource {
	return resource.NewSchemaless(
		semconv.ServiceName(cfg.ServiceName),
	)
}

// SpanFromEvent starts a span derived from ev. The span name is
// derived from ev.Type; the identity quadruple and ev.Extra become
// span attributes. NO EventPayload bytes are stamped onto the span —
// payload content is not span-safe and the audit redactor is the only
// sanctioniser of payload bytes (D-020). The returned ctx carries the
// new span; the caller is responsible for ending the span (the
// integration / unit tests do so explicitly).
//
// Run / step alignment: an event carrying a run_id produces a span
// attributed to that run. When ctx already carries a parent span (the
// run span), the new span is a child of it — so a step-granularity
// event type produces a child span under the run span and the trace
// tree mirrors the run/step hierarchy.
func (t *Tracer) SpanFromEvent(ctx context.Context, ev events.Event) (context.Context, trace.Span) {
	attrs := make([]attribute.KeyValue, 0, 5+len(ev.Extra))
	attrs = append(attrs,
		attribute.String("harbor.event.type", string(ev.Type)),
		attribute.String("tenant_id", ev.Identity.TenantID),
		attribute.String("user_id", ev.Identity.UserID),
		attribute.String("session_id", ev.Identity.SessionID),
	)
	if ev.Identity.RunID != "" {
		attrs = append(attrs, attribute.String("run_id", ev.Identity.RunID))
	}
	// ev.Extra is the bounded, low-cardinality, metric-label-safe map
	// (see the events.Event doc). Safe to stamp onto a span verbatim.
	// Keys are sorted so span attributes are deterministic.
	if len(ev.Extra) > 0 {
		keys := make([]string, 0, len(ev.Extra))
		for k := range ev.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			attrs = append(attrs, attribute.String("harbor.event.extra."+k, ev.Extra[k]))
		}
	}

	spanName := "event " + string(ev.Type)
	ctx, span := t.tracer.Start(ctx, spanName, trace.WithAttributes(attrs...))
	return ctx, span
}

// LogAttrs returns the trace_id / span_id slog.Attr pair from the
// span context in ctx. Returns an empty slice when no valid span is
// active — the Phase 04 Logger elides absent attributes, so an empty
// slice composes cleanly: logger.With(tracer.LogAttrs(ctx)...).
//
// LogAttrs is a free function in spirit (it does not touch Tracer
// fields) but is a method so the call site reads as "the tracer's log
// attributes" and so a future change can consult tracer state without
// a signature break.
func (t *Tracer) LogAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", sc.TraceID().String()),
		slog.String("span_id", sc.SpanID().String()),
	}
}
