package telemetry_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// activeSpanCtx returns a ctx carrying a real, valid span context for
// propagation round-trip tests.
func activeSpanCtx(t *testing.T) (context.Context, oteltrace.SpanContext, func()) {
	t.Helper()
	tr, _, done := recorderTracer(t)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "r1",
	}
	ctx, span := tr.SpanFromEvent(context.Background(), sampleEvent(q, events.EventTypeRuntimeRunCancelled, nil))
	sc := span.SpanContext()
	return ctx, sc, func() {
		span.End()
		done()
	}
}

func TestInjectHTTP_ExtractHTTP_RoundTripPreservesTrace(t *testing.T) {
	ctx, sc, cleanup := activeSpanCtx(t)
	defer cleanup()

	h := http.Header{}
	telemetry.InjectHTTP(ctx, h)
	if h.Get("traceparent") == "" {
		t.Fatal("InjectHTTP wrote no traceparent header")
	}

	extracted := telemetry.ExtractHTTP(context.Background(), h)
	gotSC := oteltrace.SpanContextFromContext(extracted)
	if !gotSC.IsValid() {
		t.Fatal("ExtractHTTP produced no valid span context")
	}
	if gotSC.TraceID() != sc.TraceID() {
		t.Errorf("trace id not preserved: got %s, want %s", gotSC.TraceID(), sc.TraceID())
	}
	if gotSC.SpanID() != sc.SpanID() {
		t.Errorf("span id not preserved: got %s, want %s", gotSC.SpanID(), sc.SpanID())
	}
}

func TestInjectHTTP_NilHeader_NoPanic(t *testing.T) {
	ctx, _, cleanup := activeSpanCtx(t)
	defer cleanup()
	telemetry.InjectHTTP(ctx, nil) // must not panic
	if got := telemetry.ExtractHTTP(context.Background(), nil); got == nil {
		t.Error("ExtractHTTP(nil) returned nil ctx")
	}
}

func TestExtractHTTP_NoSpan_ReturnsNoValidSpanContext(t *testing.T) {
	// A header with no traceparent yields a ctx with no valid span
	// context — InjectHTTP wrote nothing because the source ctx had
	// no active span.
	h := http.Header{}
	telemetry.InjectHTTP(context.Background(), h)
	if h.Get("traceparent") != "" {
		t.Errorf("InjectHTTP with no active span wrote a traceparent: %q", h.Get("traceparent"))
	}
	extracted := telemetry.ExtractHTTP(context.Background(), h)
	if oteltrace.SpanContextFromContext(extracted).IsValid() {
		t.Error("ExtractHTTP from an empty header produced a valid span context")
	}
}

func TestExtractHTTP_MalformedTraceparent_FailSafe(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "this-is-not-a-valid-traceparent")
	extracted := telemetry.ExtractHTTP(context.Background(), h) // must not panic
	if oteltrace.SpanContextFromContext(extracted).IsValid() {
		t.Error("ExtractHTTP with a garbage traceparent produced a valid span context")
	}
}

func TestInjectMeta_ExtractMeta_RoundTripPreservesTrace(t *testing.T) {
	ctx, sc, cleanup := activeSpanCtx(t)
	defer cleanup()

	meta := map[string]any{}
	telemetry.InjectMeta(ctx, meta)
	if _, ok := meta["traceparent"]; !ok {
		t.Fatal("InjectMeta wrote no traceparent into _meta")
	}

	extracted := telemetry.ExtractMeta(context.Background(), meta)
	gotSC := oteltrace.SpanContextFromContext(extracted)
	if !gotSC.IsValid() {
		t.Fatal("ExtractMeta produced no valid span context")
	}
	if gotSC.TraceID() != sc.TraceID() {
		t.Errorf("trace id not preserved: got %s, want %s", gotSC.TraceID(), sc.TraceID())
	}
}

func TestInjectMeta_NilMap_NoPanic(t *testing.T) {
	ctx, _, cleanup := activeSpanCtx(t)
	defer cleanup()
	telemetry.InjectMeta(ctx, nil) // must not panic
	if got := telemetry.ExtractMeta(context.Background(), nil); got == nil {
		t.Error("ExtractMeta(nil) returned nil ctx")
	}
}

func TestExtractMeta_NonStringValue_Ignored(t *testing.T) {
	// A _meta map with a non-string traceparent (defensive: MCP _meta
	// is untyped JSON) must not crash extraction.
	meta := map[string]any{"traceparent": 12345, "other": []int{1, 2}}
	extracted := telemetry.ExtractMeta(context.Background(), meta)
	if oteltrace.SpanContextFromContext(extracted).IsValid() {
		t.Error("ExtractMeta with a non-string traceparent produced a valid span context")
	}
}

func TestInjectEnv_ExtractEnv_RoundTripPreservesTrace(t *testing.T) {
	ctx, sc, cleanup := activeSpanCtx(t)
	defer cleanup()

	base := []string{"PATH=/usr/bin", "HOME=/root"}
	env := telemetry.InjectEnv(ctx, base)

	var found bool
	for _, e := range env {
		if strings.HasPrefix(e, telemetry.EnvTraceparent+"=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("InjectEnv did not append %s: %v", telemetry.EnvTraceparent, env)
	}
	// The base entries are preserved.
	if len(env) < len(base) {
		t.Errorf("InjectEnv dropped base env entries: %v", env)
	}

	extracted := telemetry.ExtractEnv(context.Background(), env)
	gotSC := oteltrace.SpanContextFromContext(extracted)
	if !gotSC.IsValid() {
		t.Fatal("ExtractEnv produced no valid span context")
	}
	if gotSC.TraceID() != sc.TraceID() {
		t.Errorf("trace id not preserved: got %s, want %s", gotSC.TraceID(), sc.TraceID())
	}
}

func TestInjectEnv_NoActiveSpan_ReturnsEnvUnchanged(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	env := telemetry.InjectEnv(context.Background(), base)
	for _, e := range env {
		if strings.HasPrefix(e, telemetry.EnvTraceparent+"=") {
			t.Errorf("InjectEnv with no active span appended %s: %v", telemetry.EnvTraceparent, env)
		}
	}
}

func TestInjectEnv_NoDuplicateOnReinject(t *testing.T) {
	ctx, _, cleanup := activeSpanCtx(t)
	defer cleanup()

	env := telemetry.InjectEnv(ctx, []string{"PATH=/usr/bin"})
	// Re-inject onto the already-stamped slice — must replace, not
	// duplicate, the HARBOR_TRACEPARENT entry.
	env = telemetry.InjectEnv(ctx, env)
	count := 0
	for _, e := range env {
		if strings.HasPrefix(e, telemetry.EnvTraceparent+"=") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("re-inject produced %d %s entries, want 1", count, telemetry.EnvTraceparent)
	}
}

func TestExtractEnv_EmptyEnviron_ReturnsCtxUnchanged(t *testing.T) {
	got := telemetry.ExtractEnv(context.Background(), nil)
	if oteltrace.SpanContextFromContext(got).IsValid() {
		t.Error("ExtractEnv(nil) produced a valid span context")
	}
}
