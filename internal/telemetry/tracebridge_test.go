package telemetry_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	patternsaudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"

	// Blank-import the inmem events driver so events.Open resolves it —
	// the same self-registration path the production aggregator uses.
	// Test-scoped per the §13 carve-out: this is the driver under test's
	// own boundary, not a hand-curated production list.
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	// The event-type owners: importing them registers the canonical
	// lifecycle types (task.*, tool.*, pause.*) the bridge derives
	// spans from, and supplies the typed payloads Publish requires.
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
)

// bridgePayload returns the canonical typed payload for the event
// types the bridge tests publish (the bus rejects nil payloads).
func bridgePayload(typ events.EventType) events.EventPayload {
	switch typ {
	case tasks.EventTypeTaskStarted:
		return tasks.TaskStartedPayload{TaskID: "task-1"}
	case tasks.EventTypeTaskCompleted:
		return tasks.TaskCompletedPayload{TaskID: "task-1"}
	case tasks.EventTypeTaskFailed:
		return tasks.TaskFailedPayload{TaskID: "task-1", ErrorCode: "boom"}
	case tools.EventTypeToolInvoked:
		return tools.ToolInvokedPayload{ToolName: "demo"}
	case tools.EventTypeToolCompleted:
		return tools.ToolCompletedPayload{ToolName: "demo"}
	case tools.EventTypeToolFailed:
		return tools.ToolFailedPayload{ToolName: "demo"}
	case pauseresume.EventTypePauseRequested:
		return pauseresume.PauseRequestedPayload{}
	case events.EventTypeRuntimeError:
		return events.RuntimeErrorPayload{Message: "boom"}
	}
	return nil
}

// bridgeBusCfg is a minimal inmem events config for the bridge tests.
func bridgeBusCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              5 * time.Second,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         512,
	}
}

// mkBridgeBus opens a real inmem bus over the real patterns redactor.
func mkBridgeBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), bridgeBusCfg(), patternsaudit.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// mkRecorderTracer constructs a Tracer over an in-memory span
// recorder (the WithSpanExporter test seam → synchronous processor,
// so spans are observable the instant End returns).
func mkRecorderTracer(t *testing.T) (*telemetry.Tracer, *tracetest.InMemoryExporter) {
	t.Helper()
	rec := tracetest.NewInMemoryExporter()
	tracer, shutdown, err := telemetry.NewTracer(config.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "info",
		ServiceName: "harbor-tracebridge-test",
	}, telemetry.WithSpanExporter(rec))
	if err != nil {
		t.Fatalf("telemetry.NewTracer: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return tracer, rec
}

// bridgeQuad is the identity quadruple the single-run tests publish
// under.
var bridgeQuad = identity.Quadruple{
	Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
	RunID:    "run-1",
}

// publish fires one typed event for q on the bus.
func publish(t *testing.T, bus events.EventBus, typ events.EventType, q identity.Quadruple) {
	t.Helper()
	if err := bus.Publish(context.Background(), events.Event{Type: typ, Identity: q, Payload: bridgePayload(typ)}); err != nil {
		t.Fatalf("bus.Publish(%s): %v", typ, err)
	}
}

// waitSpans polls the recorder until at least n ended spans are
// visible or the deadline fires — an eventually-style assertion with
// a bounded real-time timeout (never a synchronisation sleep).
func waitSpans(t *testing.T, rec *tracetest.InMemoryExporter, n int) tracetest.SpanStubs {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		spans := rec.GetSpans()
		if len(spans) >= n {
			return spans
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d spans; recorder has %d", n, len(spans))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// spanByName returns the first recorded span whose Name matches.
func spanByName(t *testing.T, spans tracetest.SpanStubs, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span named %q (have %d spans)", name, len(spans))
	return tracetest.SpanStub{}
}

// stubAttr returns the string value of an attribute on a span stub.
func stubAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}

func TestBridgeBusToTracer_FailsLoudOnNilDeps(t *testing.T) {
	tracer, _ := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	if _, err := telemetry.BridgeBusToTracer(context.Background(), nil, tracer, events.Filter{Admin: true}); !errors.Is(err, telemetry.ErrTraceBridgeMisconfigured) {
		t.Fatalf("nil bus: got %v, want ErrTraceBridgeMisconfigured", err)
	}
	if _, err := telemetry.BridgeBusToTracer(context.Background(), bus, nil, events.Filter{Admin: true}); !errors.Is(err, telemetry.ErrTraceBridgeMisconfigured) {
		t.Fatalf("nil tracer: got %v, want ErrTraceBridgeMisconfigured", err)
	}
}

func TestBridgeBusToTracer_LifecyclePairing_NestsAndEnds(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, telemetry.DefaultTraceBridgeFilter())
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}
	defer stop()

	publish(t, bus, "task.started", bridgeQuad)
	publish(t, bus, "tool.invoked", bridgeQuad)
	publish(t, bus, "tool.completed", bridgeQuad)
	publish(t, bus, "task.completed", bridgeQuad)

	spans := waitSpans(t, rec, 2)
	toolSpan := spanByName(t, spans, "event tool.invoked")
	taskSpan := spanByName(t, spans, "event task.started")

	// The tool span nests under the task span (same quadruple).
	if toolSpan.Parent.SpanID() != taskSpan.SpanContext.SpanID() {
		t.Errorf("tool span parent = %s, want the task span %s",
			toolSpan.Parent.SpanID(), taskSpan.SpanContext.SpanID())
	}
	// Identity + run IDs ride as span attributes on both spans.
	for _, s := range []tracetest.SpanStub{toolSpan, taskSpan} {
		if got := stubAttr(s, "tenant_id"); got != "t1" {
			t.Errorf("span %q tenant_id = %q, want t1", s.Name, got)
		}
		if got := stubAttr(s, "user_id"); got != "u1" {
			t.Errorf("span %q user_id = %q, want u1", s.Name, got)
		}
		if got := stubAttr(s, "session_id"); got != "s1" {
			t.Errorf("span %q session_id = %q, want s1", s.Name, got)
		}
		if got := stubAttr(s, "run_id"); got != "run-1" {
			t.Errorf("span %q run_id = %q, want run-1", s.Name, got)
		}
		if s.EndTime.Before(s.StartTime) {
			t.Errorf("span %q ended before it started", s.Name)
		}
	}
	// The happy-path closers leave the status unset (not Error).
	if taskSpan.Status.Code == codes.Error {
		t.Errorf("task span status = Error, want non-error on task.completed")
	}
}

func TestBridgeBusToTracer_FailureCloser_MarksStatusError(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, telemetry.DefaultTraceBridgeFilter())
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}
	defer stop()

	publish(t, bus, "task.started", bridgeQuad)
	publish(t, bus, "task.failed", bridgeQuad)

	spans := waitSpans(t, rec, 1)
	taskSpan := spanByName(t, spans, "event task.started")
	if taskSpan.Status.Code != codes.Error {
		t.Errorf("task span status = %v, want Error on task.failed", taskSpan.Status.Code)
	}
}

func TestBridgeBusToTracer_NonLifecycleEvent_AttachesAsSpanEvent(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	// Broad filter: lifecycle pairs PLUS a non-lifecycle type.
	f := telemetry.DefaultTraceBridgeFilter()
	f.Types = append(f.Types, "pause.requested")
	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, f)
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}
	defer stop()

	publish(t, bus, "task.started", bridgeQuad)
	publish(t, bus, "pause.requested", bridgeQuad)
	publish(t, bus, "task.completed", bridgeQuad)

	spans := waitSpans(t, rec, 1)
	taskSpan := spanByName(t, spans, "event task.started")
	found := false
	for _, evt := range taskSpan.Events {
		if evt.Name == "pause.requested" {
			found = true
		}
	}
	if !found {
		t.Errorf("task span events = %v, want a pause.requested span event", taskSpan.Events)
	}
}

func TestBridgeBusToTracer_OrphanEvents_RecordStandaloneSpans(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	f := telemetry.DefaultTraceBridgeFilter()
	f.Types = append(f.Types, "runtime.error")
	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, f)
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}
	defer stop()

	// A closer with no opener (bridge attached mid-run) and a
	// non-lifecycle event with no enclosing span both become
	// standalone instantaneous spans — never silently dropped.
	publish(t, bus, "tool.failed", bridgeQuad)
	publish(t, bus, "runtime.error", bridgeQuad)

	spans := waitSpans(t, rec, 2)
	failSpan := spanByName(t, spans, "event tool.failed")
	if failSpan.Status.Code != codes.Error {
		t.Errorf("orphan tool.failed span status = %v, want Error", failSpan.Status.Code)
	}
	spanByName(t, spans, "event runtime.error")
}

func TestBridgeBusToTracer_FilterHonoured(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	// Task-only filter: tool events never reach the bridge.
	f := events.Filter{Admin: true, Types: []events.EventType{"task.started", "task.completed"}}
	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, f)
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}

	publish(t, bus, "tool.invoked", bridgeQuad)
	publish(t, bus, "tool.completed", bridgeQuad)
	publish(t, bus, "task.started", bridgeQuad)
	publish(t, bus, "task.completed", bridgeQuad)

	spans := waitSpans(t, rec, 1)
	stop()
	for _, s := range rec.GetSpans() {
		if s.Name == "event tool.invoked" {
			t.Errorf("filtered-out tool.invoked produced a span")
		}
	}
	_ = spans
}

func TestBridgeBusToTracer_StopEndsOpenSpans_AndIsIdempotent(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)

	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, telemetry.DefaultTraceBridgeFilter())
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}

	publish(t, bus, "task.started", bridgeQuad)
	// Wait until the bridge has observed the opener: a subsequent
	// closer-less stop must end it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		stop()
		spans := rec.GetSpans()
		if len(spans) == 1 {
			evFound := false
			for _, evt := range spans[0].Events {
				if evt.Name == "harbor.trace_bridge.stopped_before_close" {
					evFound = true
				}
			}
			if !evFound {
				t.Errorf("stop-ended span lacks the stopped_before_close span event: %v", spans[0].Events)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop did not flush the open span; recorder has %d spans", len(rec.GetSpans()))
		}
	}
	// Idempotent: a second (and third) stop is a no-op.
	stop()
	stop()
}

// TestBridgeBusToTracer_ConcurrentPublish_NoLeak is the D-025 / §11
// concurrent-reuse + goroutine-leak gate: N≥100 concurrent publishes
// through one shared bridge under -race; identity isolation asserted
// per-span; goroutine baseline restored after stop.
func TestBridgeBusToTracer_ConcurrentPublish_NoLeak(t *testing.T) {
	tracer, rec := mkRecorderTracer(t)
	bus := mkBridgeBus(t)
	baseline := runtime.NumGoroutine()

	stop, err := telemetry.BridgeBusToTracer(context.Background(), bus, tracer, telemetry.DefaultTraceBridgeFilter())
	if err != nil {
		t.Fatalf("BridgeBusToTracer: %v", err)
	}

	const n = 120
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t%d", i),
					UserID:    fmt.Sprintf("u%d", i),
					SessionID: fmt.Sprintf("s%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			publish(t, bus, "task.started", q)
			publish(t, bus, "task.completed", q)
		}(i)
	}
	wg.Wait()

	spans := waitSpans(t, rec, n)
	// No cross-talk: every span carries exactly its own goroutine's
	// identity (tenant/run pair must agree).
	for _, s := range spans {
		tenant := stubAttr(s, "tenant_id")
		run := stubAttr(s, "run_id")
		if tenant == "" || run == "" {
			t.Errorf("span %q missing identity attributes", s.Name)
			continue
		}
		if "t"+run[len("run-"):] != tenant {
			t.Errorf("identity cross-talk: span has tenant %q with run %q", tenant, run)
		}
	}

	stop()
	// Goroutine baseline restored after stop (bounded eventually-poll).
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
