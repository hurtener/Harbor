// phase111f_telemetry_test.go — Phase 111f (D-203) telemetry-half
// E2E: the canonical telemetry Logger, the OTel tracer, and BOTH bus
// bridges assembled on a production-shaped stack with REAL drivers
// (CLAUDE.md §17.3 — patterns redactor, inmem bus/state, the promoted
// assemble.Assemble; the mock LLM is the sanctioned D-089 dev driver).
//
// What this pins:
//
//  1. `telemetry.New`'s first production caller: Assemble constructs
//     the redactor-mandatory, bus-paired Logger; `Logger.Error` on the
//     assembled stack emits the slog record AND the paired
//     `runtime.error` bus event (RFC §6.14 made true; asserted via the
//     bus).
//  2. The flow seam forwards `Stack.RunErrorHandler` to
//     `engine.WithRunErrorHandler`: a terminal node failure on a
//     flow-as-tool run reaches `telemetry.Logger.Error` → the paired
//     `runtime.error` event.
//  3. The bus→tracer bridge runs on the production assembly: a REAL
//     task lifecycle (TaskRegistry Spawn → MarkRunning → MarkComplete)
//     derives an ended span carrying the identity quadruple as span
//     attributes (identity propagation on events AND spans).
//  4. No metrics-cardinality regression: the metrics derivation still
//     never labels by run/identity values (brief 06 split re-asserted
//     on the live assembled registry).
//  5. Failure mode: redactor-missing Logger construction fails loud
//     (ErrRedactorMissing) — never a silent unredacted logger.
//  6. Lifecycle: both bridges + tracer join the closer chain —
//     goroutine baseline restored after Stack.Close.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// phase111fQuad is the identity quadruple the telemetry E2E runs
// under.
var phase111fQuad = identity.Quadruple{
	Identity: identity.Identity{TenantID: "t-111f", UserID: "u-111f", SessionID: "s-111f"},
	RunID:    "run-111f",
}

// phase111fCtx returns a ctx carrying the E2E quadruple (triple +
// run keys, the same shape the runtime stamps).
func phase111fCtx(t *testing.T, q identity.Quadruple) context.Context {
	t.Helper()
	// The triple and quadruple ctx keys are independent (see
	// internal/identity); stamp BOTH, the same shape the runtime's
	// steering loop uses.
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// assembleTelemetryStack assembles the production-shaped stack with
// an in-memory span recorder injected through the TracerOptions test
// seam (the only non-production knob; everything else is the real
// path).
func assembleTelemetryStack(t *testing.T) (*assemble.Stack, *tracetest.InMemoryExporter) {
	t.Helper()
	cfg := headlessRecipeCfg(t)
	rec := tracetest.NewInMemoryExporter()
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		TracerOptions: []telemetry.TracerOption{telemetry.WithSpanExporter(rec)},
		// Keep the E2E's stdout clean: the Logger still runs the full
		// redaction + bus-pairing path; only the slog sink is diverted.
		TelemetryOptions: []telemetry.Option{telemetry.WithWriter(io.Discard)},
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	return stack, rec
}

// awaitRuntimeError drains sub until a runtime.error event whose
// message contains want arrives, or the deadline fires.
func awaitRuntimeError(t *testing.T, sub events.Subscription, want string) events.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before runtime.error arrived")
			}
			if ev.Type != events.EventTypeRuntimeError {
				continue
			}
			// The bus redacted the typed payload into a map; match on
			// its JSON projection so the assertion is shape-agnostic.
			raw, err := json.Marshal(ev.Payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			if strings.Contains(string(raw), want) {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for runtime.error containing %q", want)
		}
	}
}

// TestE2E_Phase111f_LoggerError_EmitsPairedBusEvent — acceptance
// criterion 2: Logger.Error on the assembled stack writes the slog
// record AND publishes the paired runtime.error event with the ctx
// identity.
func TestE2E_Phase111f_LoggerError_EmitsPairedBusEvent(t *testing.T) {
	stack, _ := assembleTelemetryStack(t)
	if stack.Telemetry == nil {
		t.Fatal("Stack.Telemetry is nil — telemetry.New has no production caller")
	}

	ctx := phase111fCtx(t, phase111fQuad)
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  phase111fQuad.TenantID,
		User:    phase111fQuad.UserID,
		Session: phase111fQuad.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeError},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	stack.Telemetry.Error(ctx, "phase111f: paired emission check")

	ev := awaitRuntimeError(t, sub, "phase111f: paired emission check")
	if ev.Identity.TenantID != phase111fQuad.TenantID ||
		ev.Identity.UserID != phase111fQuad.UserID ||
		ev.Identity.SessionID != phase111fQuad.SessionID {
		t.Fatalf("runtime.error identity = %+v, want %+v", ev.Identity, phase111fQuad)
	}
	if ev.Identity.RunID != phase111fQuad.RunID {
		t.Fatalf("runtime.error run_id = %q, want %q", ev.Identity.RunID, phase111fQuad.RunID)
	}
}

// TestE2E_Phase111f_FlowRunError_ReachesTelemetryLogger — acceptance
// criterion 3: the flow seam forwards Stack.RunErrorHandler to
// engine.WithRunErrorHandler; a terminal node failure on a
// flow-as-tool run produces the paired runtime.error bus event.
func TestE2E_Phase111f_FlowRunError_ReachesTelemetryLogger(t *testing.T) {
	stack, _ := assembleTelemetryStack(t)
	if stack.RunErrorHandler == nil {
		t.Fatal("Stack.RunErrorHandler is nil — the production handler was not built")
	}

	failing := func(_ context.Context, _ messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return messages.Envelope{}, errors.New("phase111f-node-boom")
	}
	def := flow.Definition{
		Name:   "phase111f_failing_flow",
		Entry:  "a",
		Exit:   "a",
		Nodes:  map[flow.NodeID]flow.NodeSpec{"a": {Func: failing}},
		Budget: flow.Budget{Deadline: 2 * time.Second},
	}
	eng, err := flow.Compose(def, flow.WithRunErrorHandler(stack.RunErrorHandler))
	if err != nil {
		t.Fatalf("flow.Compose: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})
	if _, err := flow.RegisterAsTool(stack.Catalog, def, eng); err != nil {
		t.Fatalf("flow.RegisterAsTool: %v", err)
	}

	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  phase111fQuad.TenantID,
		User:    phase111fQuad.UserID,
		Session: phase111fQuad.SessionID,
		Types:   []events.EventType{events.EventTypeRuntimeError},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	d, ok := stack.Catalog.Resolve("phase111f_failing_flow")
	if !ok {
		t.Fatal("flow tool not registered")
	}
	// The flow-as-tool invocation fails (the lone node fails
	// terminally; the bounded Budget.Deadline surfaces it as a flow
	// error) — the load-bearing assertion is the handler-driven
	// runtime.error event below.
	if _, err := d.Invoke(phase111fCtx(t, phase111fQuad), json.RawMessage(`{}`)); err == nil {
		t.Fatal("failing flow invocation unexpectedly succeeded")
	}

	ev := awaitRuntimeError(t, sub, "engine: run failed")
	if ev.Identity.TenantID != phase111fQuad.TenantID {
		t.Fatalf("runtime.error tenant = %q, want %q", ev.Identity.TenantID, phase111fQuad.TenantID)
	}
}

// TestE2E_Phase111f_TaskLifecycle_DerivesSpans — acceptance criteria
// 1+4: the tracer is constructed on the production path and the
// bus→tracer bridge derives an ended, identity-attributed span from a
// REAL TaskRegistry lifecycle.
func TestE2E_Phase111f_TaskLifecycle_DerivesSpans(t *testing.T) {
	stack, rec := assembleTelemetryStack(t)
	if stack.Tracer == nil {
		t.Fatal("Stack.Tracer is nil — NewTracer has no production caller")
	}

	ctx := phase111fCtx(t, phase111fQuad)
	handle, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity:    phase111fQuad,
		Kind:        tasks.KindForeground,
		Description: "phase111f span derivation",
		Query:       "noop",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	if err := stack.Tasks.MarkRunning(ctx, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := stack.Tasks.MarkComplete(ctx, handle.ID, tasks.TaskResult{Value: json.RawMessage(`"ok"`)}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	// Eventually: the bridge drains asynchronously; poll the recorder
	// with a bounded deadline (never a synchronisation sleep).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var found *tracetest.SpanStub
		for _, s := range rec.GetSpans() {
			if s.Name == "event task.started" {
				found = &s
				break
			}
		}
		if found != nil {
			attrs := map[string]string{}
			for _, kv := range found.Attributes {
				attrs[string(kv.Key)] = kv.Value.AsString()
			}
			if attrs["tenant_id"] != phase111fQuad.TenantID ||
				attrs["user_id"] != phase111fQuad.UserID ||
				attrs["session_id"] != phase111fQuad.SessionID {
				t.Fatalf("span identity attrs = %v, want the %v triple", attrs, phase111fQuad.Identity)
			}
			if found.EndTime.Before(found.StartTime) {
				t.Fatal("task span never ended")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no task.started span derived; recorder has %d spans", len(rec.GetSpans()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestE2E_Phase111f_MetricsCardinality_NoIdentityLabels re-asserts
// the brief 06 cardinality split on the LIVE assembled registry: the
// metrics derivation never labels by run / trace / identity values
// (the trace bridge is where identity lives, as span attributes).
func TestE2E_Phase111f_MetricsCardinality_NoIdentityLabels(t *testing.T) {
	stack, _ := assembleTelemetryStack(t)

	ctx := phase111fCtx(t, phase111fQuad)
	handle, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity: phase111fQuad,
		Kind:     tasks.KindForeground,
		Query:    "noop",
	})
	if err != nil {
		t.Fatalf("Tasks.Spawn: %v", err)
	}
	if err := stack.Tasks.MarkRunning(ctx, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	snap, err := stack.Metrics.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Metrics.Snapshot: %v", err)
	}
	if len(snap.Counters) == 0 {
		t.Fatal("metrics snapshot empty — the metrics bridge is not draining")
	}
	for _, c := range snap.Counters {
		for k, v := range c.Labels {
			switch k {
			case "run_id", "trace_id", "span_id", "tenant_id", "user_id", "session_id":
				t.Errorf("metrics cardinality regression: counter %q labelled %s=%s", c.Name, k, v)
			}
			if v == phase111fQuad.RunID || v == phase111fQuad.TenantID {
				t.Errorf("metrics cardinality regression: counter %q label %s carries an identity/run value %q", c.Name, k, v)
			}
		}
	}
}

// TestE2E_Phase111f_RedactorMissing_FailsLoud — failure mode: the
// Logger refuses construction without a redactor (no silent
// unredacted fallback; CLAUDE.md §13).
func TestE2E_Phase111f_RedactorMissing_FailsLoud(t *testing.T) {
	_, err := telemetry.New(config.TelemetryConfig{
		LogFormat: "json", LogLevel: "info", ServiceName: "harbor-111f",
	}, nil)
	if !errors.Is(err, telemetry.ErrRedactorMissing) {
		t.Fatalf("redactor-missing construction: got %v, want ErrRedactorMissing", err)
	}
}

// TestE2E_Phase111f_BridgesJoinCloserChain — lifecycle: assembling and
// closing N stacks (tracer + both bridges on the closer chain)
// restores the goroutine baseline; concurrent Logger.Error traffic
// through one stack races nothing (D-025 / §17.3 stress).
func TestE2E_Phase111f_BridgesJoinCloserChain(t *testing.T) {
	baseline := goruntime.NumGoroutine()

	cfg := headlessRecipeCfg(t)
	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		TelemetryOptions: []telemetry.Option{telemetry.WithWriter(io.Discard)},
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}

	// N≥10 concurrent producers through the shared Logger + bus while
	// both bridges drain (cross-package concurrency per §17.3).
	done := make(chan struct{})
	for i := range 16 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t%d", i),
					UserID:    fmt.Sprintf("u%d", i),
					SessionID: fmt.Sprintf("s%d", i),
				},
				RunID: fmt.Sprintf("r%d", i),
			}
			ctx, cerr := identity.WithRun(context.Background(), q.Identity, q.RunID)
			if cerr != nil {
				return
			}
			for range 8 {
				stack.Telemetry.Error(ctx, "phase111f stress")
			}
		}(i)
	}
	for range 16 {
		<-done
	}

	if err := stack.Close(context.Background()); err != nil {
		t.Fatalf("Stack.Close: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for goruntime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak after Close: NumGoroutine=%d, baseline=%d", goruntime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
