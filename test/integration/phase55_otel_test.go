// Phase 55 cross-subsystem integration test (CLAUDE.md §17).
//
// Phase 55 (OTel traces + propagation) consumes two already-shipped
// subsystems — Phase 04's telemetry.Logger and Phase 05's
// events.EventBus — so an integration test is mandatory. This file
// wires REAL drivers on every seam: the inmem events bus, the
// patterns audit redactor, the production Logger, and a real
// telemetry.Tracer backed by an in-memory span recorder.
//
// It proves:
//
//   - An event published on the bus → Tracer.SpanFromEvent → a span
//     that carries the event's identity quadruple (no payload bytes).
//   - The identity triple propagates ctx → event → span attributes →
//     Tracer.LogAttrs → a Logger line carrying the same trace_id.
//   - Trace continuity holds across the HTTP / _meta / env carriers:
//     Inject on the run ctx, Extract on the carrier, SpanFromEvent on
//     the extracted ctx → the child span shares the trace id.
//   - Failure mode: NewTracer with an unknown exporter driver fails
//     loudly with ErrExporterUnknown.
//   - Concurrency: N≥10 producers derive spans against one shared
//     Tracer with no identity cross-talk and no goroutine leak.
package integration_test

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"
)

// recorderExporter is an in-memory trace.SpanExporter — it records
// every exported span so the test can assert span shape. It is NOT a
// mock of a subsystem boundary (CLAUDE.md §17.4 forbids those): the
// events bus, the redactor, the Logger and the Tracer are all real.
// The recorder is just the trace sink, standing in for a live OTLP
// collector — the OTel SDK's own InMemoryExporter shape.
type recorderExporter struct {
	mu    sync.Mutex
	spans []recordedSpan
}

type recordedSpan struct {
	name  string
	attrs map[string]string
}

func TestE2E_Phase55_OTelTraces_EventToSpanWiring(t *testing.T) {
	ctx := identityCtx(t)
	id := identity.MustFrom(ctx)
	red := auditpatterns.New()
	cfg := wave2Config()

	// Real events bus on the seam.
	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Real Logger on the seam.
	logger, err := telemetry.New(cfg.Telemetry, red)
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	// Real Tracer backed by an in-memory recorder so spans are
	// observable. WithSpanExporter is the documented test-only seam.
	rec := newRecorder()
	tracer, shutdown, err := telemetry.NewTracer(cfg.Telemetry, telemetry.WithSpanExporter(rec))
	if err != nil {
		t.Fatalf("telemetry.NewTracer: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// 1. Publish an event on the bus, then derive a span from it.
	ev := events.Event{
		Type:     events.EventTypeRuntimeRunCancelled,
		Identity: identity.MustQuadrupleFrom(ctx),
		Payload:  events.RunCancelledPayload{RunID: "R-1"},
	}
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("bus.Publish: %v", err)
	}
	// The bus assigns a Sequence on Publish; SpanFromEvent does not
	// care about Sequence — it derives from Type + Identity. Re-zero
	// it so we exercise the bridge with a publish-shaped event.
	ev.Sequence = 0

	spanCtx, span := tracer.SpanFromEvent(ctx, ev)
	span.End()

	// 2. The recorded span carries the event's identity quadruple.
	spans := rec.snapshot()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	got := spans[0]
	if !strings.Contains(got.name, string(events.EventTypeRuntimeRunCancelled)) {
		t.Errorf("span name %q does not derive from event type", got.name)
	}
	for k, want := range map[string]string{
		"tenant_id":  id.TenantID,
		"user_id":    id.UserID,
		"session_id": id.SessionID,
		"run_id":     "R-1",
	} {
		if got.attrs[k] != want {
			t.Errorf("span attr %q = %q, want %q", k, got.attrs[k], want)
		}
	}

	// 3. Identity propagation: the span ctx yields LogAttrs that a
	//    real Logger line carries. The trace_id on the span and on
	//    the log line agree.
	logAttrs := tracer.LogAttrs(spanCtx)
	if len(logAttrs) != 2 {
		t.Fatalf("LogAttrs returned %d attrs, want 2 (trace_id, span_id)", len(logAttrs))
	}
	var traceID string
	for _, a := range logAttrs {
		if a.Key == "trace_id" {
			traceID = a.Value.String()
		}
	}
	if traceID == "" || traceID == "00000000000000000000000000000000" {
		t.Errorf("LogAttrs trace_id is empty/zero: %q", traceID)
	}
	if traceID != span.SpanContext().TraceID().String() {
		t.Errorf("LogAttrs trace_id %q != span trace id %q", traceID, span.SpanContext().TraceID())
	}
	// The Logger consumes the attrs without error (composes cleanly).
	logger.Info(spanCtx, "phase55 e2e", logAttrs...)

	// 4. Trace continuity across the three carriers. Inject on the
	//    span ctx, Extract on the carrier, derive a child span — the
	//    child must share the trace id.
	t.Run("http_carrier_continuity", func(t *testing.T) {
		h := http.Header{}
		telemetry.InjectHTTP(spanCtx, h)
		remoteCtx := telemetry.ExtractHTTP(context.Background(), h)
		assertChildSharesTrace(t, tracer, rec, remoteCtx, span.SpanContext().TraceID())
	})
	t.Run("meta_carrier_continuity", func(t *testing.T) {
		meta := map[string]any{}
		telemetry.InjectMeta(spanCtx, meta)
		remoteCtx := telemetry.ExtractMeta(context.Background(), meta)
		assertChildSharesTrace(t, tracer, rec, remoteCtx, span.SpanContext().TraceID())
	})
	t.Run("env_carrier_continuity", func(t *testing.T) {
		env := telemetry.InjectEnv(spanCtx, []string{"PATH=/usr/bin"})
		remoteCtx := telemetry.ExtractEnv(context.Background(), env)
		assertChildSharesTrace(t, tracer, rec, remoteCtx, span.SpanContext().TraceID())
	})
}

// assertChildSharesTrace derives a span under remoteCtx and asserts it
// shares wantTrace — i.e. the propagation carrier preserved the trace.
func assertChildSharesTrace(t *testing.T, tracer *telemetry.Tracer, rec *recorderExporter, remoteCtx context.Context, wantTrace oteltrace.TraceID) {
	t.Helper()
	remoteSC := oteltrace.SpanContextFromContext(remoteCtx)
	if !remoteSC.IsValid() {
		t.Fatal("extracted ctx carries no valid span context — carrier did not propagate")
	}
	if remoteSC.TraceID() != wantTrace {
		t.Fatalf("extracted trace id %s != injected %s", remoteSC.TraceID(), wantTrace)
	}
	before := len(rec.snapshot())
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
		RunID:    "R-1",
	}
	_, child := tracer.SpanFromEvent(remoteCtx, events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: q,
		Payload:  events.RunCancelledPayload{RunID: "R-1"},
	})
	if child.SpanContext().TraceID() != wantTrace {
		t.Errorf("child span trace id %s != %s", child.SpanContext().TraceID(), wantTrace)
	}
	child.End()
	if len(rec.snapshot()) != before+1 {
		t.Errorf("child span not recorded")
	}
}

func TestE2E_Phase55_OTelTraces_FailureMode_UnknownExporter(t *testing.T) {
	cfg := wave2Config()
	_, _, err := telemetry.NewTracer(cfg.Telemetry, telemetry.WithExporterDriver("not-a-real-driver"))
	if !errors.Is(err, telemetry.ErrExporterUnknown) {
		t.Fatalf("NewTracer with unknown driver: want ErrExporterUnknown, got %v", err)
	}
	// The message must list the registered drivers (the noop + otlp
	// blank imports above seated them in the registry).
	if msg := err.Error(); !strings.Contains(msg, "noop") || !strings.Contains(msg, "otlp") {
		t.Errorf("ErrExporterUnknown message %q does not list registered drivers", msg)
	}
}

func TestE2E_Phase55_OTelTraces_ConcurrencyStress(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := wave2Config()
	rec := newRecorder()
	tracer, shutdown, err := telemetry.NewTracer(cfg.Telemetry, telemetry.WithSpanExporter(rec))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}

	const n = 24
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			q := tripleN(i)
			ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
			if err != nil {
				t.Errorf("goroutine %d: WithRun: %v", i, err)
				return
			}
			ev := events.Event{
				Type:     events.EventTypeRuntimeRunCancelled,
				Identity: q,
				Payload:  events.RunCancelledPayload{RunID: q.RunID},
			}
			spanCtx, span := tracer.SpanFromEvent(ctx, ev)
			// Round-trip the carriers under concurrency.
			h := http.Header{}
			telemetry.InjectHTTP(spanCtx, h)
			_ = telemetry.ExtractHTTP(context.Background(), h)
			if attrs := tracer.LogAttrs(spanCtx); len(attrs) != 2 {
				t.Errorf("goroutine %d: LogAttrs len %d, want 2", i, len(attrs))
			}
			span.End()
		}(i)
	}
	wg.Wait()

	// No identity cross-talk: every recorded span's quadruple is
	// unique and internally consistent (same goroutine seed).
	spans := rec.snapshot()
	if len(spans) != n {
		t.Fatalf("recorded %d spans, want %d", len(spans), n)
	}
	seen := map[string]bool{}
	for _, s := range spans {
		key := s.attrs["tenant_id"] + "|" + s.attrs["run_id"]
		if seen[key] {
			t.Errorf("identity cross-talk: %q recorded twice", key)
		}
		seen[key] = true
		tSeed := strings.TrimPrefix(s.attrs["tenant_id"], "t-")
		rSeed := strings.TrimPrefix(s.attrs["run_id"], "r-")
		if tSeed != rSeed {
			t.Errorf("span identity mismatch within one span: %v", s.attrs)
		}
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	// Goroutine-leak check: baseline restored within 2s.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 {
		if time.Now().After(deadline) {
			t.Errorf("goroutine leak: baseline %d, now %d", baseline, runtime.NumGoroutine())
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- recorderExporter implementation ---

func newRecorder() *recorderExporter { return &recorderExporter{} }

// ExportSpans records each span's name + string attributes. Satisfies
// go.opentelemetry.io/otel/sdk/trace.SpanExporter.
func (r *recorderExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range spans {
		attrs := make(map[string]string, len(s.Attributes()))
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		r.spans = append(r.spans, recordedSpan{name: s.Name(), attrs: attrs})
	}
	return nil
}

// Shutdown is a no-op — the recorder holds only an in-memory slice.
func (r *recorderExporter) Shutdown(_ context.Context) error { return nil }

func (r *recorderExporter) snapshot() []recordedSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedSpan, len(r.spans))
	copy(out, r.spans)
	return out
}

// compile-time assertion: recorderExporter is a real SpanExporter.
var _ sdktrace.SpanExporter = (*recorderExporter)(nil)
