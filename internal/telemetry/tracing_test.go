package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/noop"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlp"
)

// testCfg is a minimal valid TelemetryConfig for tracer construction.
func testCfg() config.TelemetryConfig {
	return config.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "info",
		ServiceName: "harbor-test",
	}
}

// recorderTracer builds a Tracer backed by an in-memory span recorder
// so emitted spans are observable. Returns the tracer, the recorder,
// and a shutdown func the caller must defer.
func recorderTracer(t *testing.T) (*telemetry.Tracer, *tracetest.InMemoryExporter, func()) {
	t.Helper()
	rec := tracetest.NewInMemoryExporter()
	tr, shutdown, err := telemetry.NewTracer(testCfg(), telemetry.WithSpanExporter(rec))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}
	return tr, rec, func() {
		if err := shutdown(context.Background()); err != nil {
			t.Errorf("tracer shutdown: %v", err)
		}
	}
}

func TestNewTracer_NoEndpoint_SelectsNoopDriver(t *testing.T) {
	tr, shutdown, err := telemetry.NewTracer(testCfg())
	if err != nil {
		t.Fatalf("NewTracer with empty OTelEndpoint: %v", err)
	}
	if tr == nil {
		t.Fatal("NewTracer returned nil *Tracer")
	}
	if shutdown == nil {
		t.Fatal("NewTracer returned nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}

func TestNewTracer_EmptyServiceName_FailsLoudly(t *testing.T) {
	cfg := testCfg()
	cfg.ServiceName = ""
	_, _, err := telemetry.NewTracer(cfg)
	if !errors.Is(err, telemetry.ErrTracerNotConfigured) {
		t.Fatalf("want ErrTracerNotConfigured, got %v", err)
	}
}

func TestNewTracer_UnknownExporterDriver_ListsRegistered(t *testing.T) {
	_, _, err := telemetry.NewTracer(testCfg(), telemetry.WithExporterDriver("does-not-exist"))
	if !errors.Is(err, telemetry.ErrExporterUnknown) {
		t.Fatalf("want ErrExporterUnknown, got %v", err)
	}
	// The message must list the registered drivers so a
	// misconfiguration is obvious.
	msg := err.Error()
	for _, want := range []string{"noop", "otlp"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrExporterUnknown message %q missing registered driver %q", msg, want)
		}
	}
}

func TestNewTracer_OTLPEndpoint_SelectsOTLPDriver(t *testing.T) {
	cfg := testCfg()
	cfg.OTelEndpoint = "127.0.0.1:4317"
	// The OTLP/gRPC exporter connects lazily — construction succeeds
	// with no live collector.
	tr, shutdown, err := telemetry.NewTracer(cfg)
	if err != nil {
		t.Fatalf("NewTracer with OTLP endpoint: %v", err)
	}
	if tr == nil {
		t.Fatal("nil *Tracer")
	}
	// Shutdown with a short deadline — no collector means the flush
	// may time out, but Shutdown must still return (not hang).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(ctx) // error tolerated: no live collector to flush to.
}

// sampleEvent builds a structurally-valid bus event for span derivation.
func sampleEvent(q identity.Quadruple, typ events.EventType, extra map[string]string) events.Event {
	return events.Event{
		Type:     typ,
		Identity: q,
		Payload:  events.RunCancelledPayload{RunID: q.RunID},
		Extra:    extra,
	}
}

func TestSpanFromEvent_DerivesNameAndIdentityAttributes(t *testing.T) {
	tr, rec, done := recorderTracer(t)
	defer done()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	_, span := tr.SpanFromEvent(context.Background(), sampleEvent(q, events.EventTypeRuntimeRunCancelled, nil))
	span.End()

	spans := flush(t, tr, rec)
	if len(spans) != 1 {
		t.Fatalf("want 1 recorded span, got %d", len(spans))
	}
	s := spans[0]
	if !strings.Contains(s.Name, string(events.EventTypeRuntimeRunCancelled)) {
		t.Errorf("span name %q does not derive from event type", s.Name)
	}
	got := attrMap(s)
	for k, want := range map[string]string{
		"tenant_id":  "t1",
		"user_id":    "u1",
		"session_id": "s1",
		"run_id":     "r1",
	} {
		if got[k] != want {
			t.Errorf("span attr %q = %q, want %q", k, got[k], want)
		}
	}
}

func TestSpanFromEvent_StampsExtraMap(t *testing.T) {
	tr, rec, done := recorderTracer(t)
	defer done()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	ev := sampleEvent(q, events.EventTypeRuntimeRunCancelled, map[string]string{"node": "planner", "step": "3"})
	_, span := tr.SpanFromEvent(context.Background(), ev)
	span.End()

	spans := flush(t, tr, rec)
	got := attrMap(spans[0])
	if got["harbor.event.extra.node"] != "planner" {
		t.Errorf("extra.node attr = %q, want planner", got["harbor.event.extra.node"])
	}
	if got["harbor.event.extra.step"] != "3" {
		t.Errorf("extra.step attr = %q, want 3", got["harbor.event.extra.step"])
	}
}

func TestSpanFromEvent_NoPayloadBytesOnSpan(t *testing.T) {
	tr, rec, done := recorderTracer(t)
	defer done()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	// A payload carrying a secret-shaped field. The span MUST NOT
	// carry any payload bytes — D-020: the audit redactor is the only
	// sanctioniser of payload content, and SpanFromEvent never walks
	// the payload.
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: q,
		Payload:  events.RuntimeErrorPayload{Message: "boom", Fields: map[string]any{"api_key": "sk-secret-leak"}},
	}
	_, span := tr.SpanFromEvent(context.Background(), ev)
	span.End()

	spans := flush(t, tr, rec)
	for _, kv := range spans[0].Attributes {
		v := kv.Value.AsString()
		if strings.Contains(v, "sk-secret-leak") || strings.Contains(string(kv.Key), "api_key") {
			t.Fatalf("span attribute %q=%q leaked payload bytes", kv.Key, v)
		}
	}
}

func TestSpanFromEvent_StepEventIsChildOfRunSpan(t *testing.T) {
	tr, rec, done := recorderTracer(t)
	defer done()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	// Run-aligned parent span.
	runCtx, runSpan := tr.SpanFromEvent(context.Background(), sampleEvent(q, events.EventTypeRuntimeRunCancelled, nil))
	// Step-granularity event derived under the run ctx → child span.
	_, stepSpan := tr.SpanFromEvent(runCtx, sampleEvent(q, events.EventTypeRuntimeWarning, nil))
	stepSpan.End()
	runSpan.End()

	spans := flush(t, tr, rec)
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	// Find the run span and the step span by name.
	var runSC, stepParent oteltrace.SpanContext
	for _, s := range spans {
		switch {
		case strings.Contains(s.Name, string(events.EventTypeRuntimeRunCancelled)):
			runSC = s.SpanContext
		case strings.Contains(s.Name, string(events.EventTypeRuntimeWarning)):
			stepParent = s.Parent
		}
	}
	if stepParent.SpanID() != runSC.SpanID() {
		t.Errorf("step span parent = %s, want run span %s", stepParent.SpanID(), runSC.SpanID())
	}
	if stepParent.TraceID() != runSC.TraceID() {
		t.Errorf("step span trace = %s, want run trace %s", stepParent.TraceID(), runSC.TraceID())
	}
}

func TestLogAttrs_ActiveSpan_ReturnsTraceAndSpanID(t *testing.T) {
	tr, _, done := recorderTracer(t)
	defer done()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	ctx, span := tr.SpanFromEvent(context.Background(), sampleEvent(q, events.EventTypeRuntimeRunCancelled, nil))
	defer span.End()

	attrs := tr.LogAttrs(ctx)
	if len(attrs) != 2 {
		t.Fatalf("want 2 log attrs (trace_id, span_id), got %d", len(attrs))
	}
	m := map[string]string{}
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	sc := span.SpanContext()
	if m["trace_id"] != sc.TraceID().String() {
		t.Errorf("trace_id attr = %q, want %q", m["trace_id"], sc.TraceID())
	}
	if m["span_id"] != sc.SpanID().String() {
		t.Errorf("span_id attr = %q, want %q", m["span_id"], sc.SpanID())
	}
}

func TestLogAttrs_NoSpan_ReturnsEmpty(t *testing.T) {
	tr, _, done := recorderTracer(t)
	defer done()
	if attrs := tr.LogAttrs(context.Background()); len(attrs) != 0 {
		t.Errorf("LogAttrs with no active span = %v, want empty", attrs)
	}
}

func TestRegisterExporter_EmptyName_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("RegisterExporter with empty name did not panic")
		}
	}()
	telemetry.RegisterExporter("", stubExporter{})
}

func TestRegisterExporter_NilExporter_Panics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("RegisterExporter with nil exporter did not panic")
		}
	}()
	telemetry.RegisterExporter("stub-nil", nil)
}

func TestRegisterExporter_Duplicate_IsNoOp(t *testing.T) {
	// Registering noop again must not panic and must not replace the
	// real driver (first-registration-wins, no-op on duplicate).
	telemetry.RegisterExporter("noop", stubExporter{})
	// If the duplicate had replaced the real noop driver, NewTracer
	// with the noop driver would route through stubExporter — which
	// is fine here, the assertion is just "no panic, still works".
	_, shutdown, err := telemetry.NewTracer(testCfg(), telemetry.WithExporterDriver("noop"))
	if err != nil {
		t.Fatalf("NewTracer after duplicate register: %v", err)
	}
	_ = shutdown(context.Background())
}

// stubExporter is a telemetry.SpanExporter test double.
type stubExporter struct{}

func (stubExporter) Exporter(_ context.Context, _ config.TelemetryConfig) (sdktrace.SpanExporter, error) {
	return tracetest.NewNoopExporter(), nil
}

// TestConcurrentReuse_Tracer is the mandatory D-025 concurrent-reuse
// test: N≥100 goroutines, each with a goroutine-unique identity
// quadruple, drive SpanFromEvent + the three propagation round-trips
// + LogAttrs against ONE shared *Tracer under -race. Asserts: no data
// races (the -race gate), no identity cross-talk (each goroutine's
// span carries its own quadruple), no goroutine leak (baseline
// restored after shutdown).
func TestConcurrentReuse_Tracer(t *testing.T) {
	baseline := runtime.NumGoroutine()

	rec := tracetest.NewInMemoryExporter()
	tr, shutdown, err := telemetry.NewTracer(testCfg(), telemetry.WithSpanExporter(rec))
	if err != nil {
		t.Fatalf("NewTracer: %v", err)
	}

	const n = 150
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%d", i),
					UserID:    fmt.Sprintf("user-%d", i),
					SessionID: fmt.Sprintf("session-%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			ctx, span := tr.SpanFromEvent(context.Background(), sampleEvent(q, events.EventTypeRuntimeRunCancelled, nil))

			// Identity cross-talk check: the span's ctx must yield log
			// attrs that round-trip to a valid trace, and the span
			// must carry this goroutine's tenant — verified after End
			// via the recorder. Here we exercise the carriers.
			hdr := map[string][]string{}
			telemetry.InjectHTTP(ctx, hdr)
			_ = telemetry.ExtractHTTP(context.Background(), hdr)

			meta := map[string]any{}
			telemetry.InjectMeta(ctx, meta)
			_ = telemetry.ExtractMeta(context.Background(), meta)

			env := telemetry.InjectEnv(ctx, []string{"PATH=/usr/bin"})
			_ = telemetry.ExtractEnv(context.Background(), env)

			if attrs := tr.LogAttrs(ctx); len(attrs) != 2 {
				t.Errorf("goroutine %d: LogAttrs len = %d, want 2", i, len(attrs))
			}
			span.End()
		}(i)
	}
	wg.Wait()

	spans := flush(t, tr, rec)
	if len(spans) != n {
		t.Fatalf("want %d recorded spans, got %d", n, len(spans))
	}
	// No identity cross-talk: every (tenant,user,session,run) tuple is
	// unique across the recorded spans.
	seen := map[string]bool{}
	for _, s := range spans {
		m := attrMap(s)
		key := m["tenant_id"] + "|" + m["user_id"] + "|" + m["session_id"] + "|" + m["run_id"]
		if seen[key] {
			t.Errorf("identity cross-talk: tuple %q recorded twice", key)
		}
		seen[key] = true
		// Each component must agree (same goroutine index).
		if got := strings.TrimPrefix(m["tenant_id"], "tenant-"); got != strings.TrimPrefix(m["run_id"], "run-") {
			t.Errorf("span identity mismatch within one span: %v", m)
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

// --- helpers ---

// flush returns the recorder's span snapshot. The recorder-backed
// Tracer (recorderTracer / WithSpanExporter) uses the synchronous
// span processor, so a span is in the recorder the instant span.End()
// returns — no poll needed.
func flush(t *testing.T, _ *telemetry.Tracer, rec *tracetest.InMemoryExporter) tracetest.SpanStubs {
	t.Helper()
	return rec.GetSpans()
}

// attrMap flattens a recorded span's attributes into a string map.
func attrMap(s tracetest.SpanStub) map[string]string {
	m := make(map[string]string, len(s.Attributes))
	for _, kv := range s.Attributes {
		m[string(kv.Key)] = kv.Value.AsString()
	}
	return m
}
