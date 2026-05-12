package output_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/output"
)

// recordingDriver is a minimal LLMClient stub: records every received
// request + returns a caller-supplied response (or error). Concurrent-
// reuse safe.
type recordingDriver struct {
	mu     sync.Mutex
	seen   []llm.CompleteRequest
	respCB func(req llm.CompleteRequest, attempt int) (llm.CompleteResponse, error)
	closed atomic.Bool
}

func newRecorder(cb func(req llm.CompleteRequest, attempt int) (llm.CompleteResponse, error)) *recordingDriver {
	return &recordingDriver{respCB: cb}
}

func (r *recordingDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if r.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	r.mu.Lock()
	r.seen = append(r.seen, req)
	attempt := len(r.seen)
	r.mu.Unlock()
	return r.respCB(req, attempt)
}

func (r *recordingDriver) Close(_ context.Context) error {
	r.closed.CompareAndSwap(false, true)
	return nil
}

func (r *recordingDriver) snapshot() []llm.CompleteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]llm.CompleteRequest, len(r.seen))
	copy(out, r.seen)
	return out
}

// testBus returns a fresh in-memory event bus and a fresh redactor.
func testBus(t *testing.T) events.EventBus {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(t.Context(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		ReplayBufferSize:         16,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = bus.Close(context.Background())
	})
	return bus
}

func snapshotWithProfile(model string, profile llm.ModelProfile) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles:        map[string]llm.ModelProfile{model: profile},
	}
}

func ctxWithIdentity(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithRun(t.Context(), id, "r")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func sampleSchema() []byte {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
}

func sampleRequest(model string, mode llm.ResponseFormatKind) llm.CompleteRequest {
	t := "hello"
	msgs := []llm.ChatMessage{
		{Role: llm.RoleUser, Content: llm.Content{Text: &t}},
	}
	req := llm.CompleteRequest{
		Model:    model,
		Messages: msgs,
	}
	if mode != "" {
		req.ResponseFormat = &llm.ResponseFormat{
			Kind:       mode,
			JSONSchema: sampleSchema(),
		}
	}
	return req
}

// TestWrap_PassesThrough_WhenProfileMissing — when the request's
// model has no profile, the wrapper delegates verbatim.
func TestWrap_PassesThrough_WhenProfileMissing(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok"}, nil
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	resp, err := client.Complete(ctxWithIdentity(t), sampleRequest("unknown/model", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want %q", resp.Content, "ok")
	}
	seen := rec.snapshot()
	if len(seen) != 1 {
		t.Fatalf("inner saw %d requests, want 1", len(seen))
	}
	if seen[0].ResponseFormat == nil || seen[0].ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("inner request was modified despite profile miss: %+v", seen[0].ResponseFormat)
	}
}

// TestWrap_PassesThrough_WhenOutputModeUnset — profile exists but
// OutputMode is unset (zero value) → no shaping, no chain.
func TestWrap_PassesThrough_WhenOutputModeUnset(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok"}, nil
	})
	cfg := snapshotWithProfile("any/model", llm.ModelProfile{
		ContextWindowTokens: 1000,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("any/model", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	seen := rec.snapshot()
	if len(seen) != 1 || seen[0].ResponseFormat == nil ||
		seen[0].ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("expected pass-through, got %+v", seen)
	}
}

// TestWrap_Native_HappyPath — Native mode forwards the json_schema
// request unchanged when the inner client returns success.
func TestWrap_Native_HappyPath(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: `{"name":"ok"}`}, nil
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("openai/gpt-4o", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	seen := rec.snapshot()
	if len(seen) != 1 || seen[0].ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("Native should preserve FormatJSONSchema; got %+v", seen[0].ResponseFormat)
	}
}

// TestWrap_Prompted_CoercesAndAppendsSystem — Prompted coerces to
// FormatJSONObject and appends a system message containing the schema.
func TestWrap_Prompted_CoercesAndAppendsSystem(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: `{}`}, nil
	})
	cfg := snapshotWithProfile("nim/some", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModePrompted,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("nim/some", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	seen := rec.snapshot()
	if len(seen) != 1 {
		t.Fatalf("inner saw %d requests, want 1", len(seen))
	}
	if seen[0].ResponseFormat == nil || seen[0].ResponseFormat.Kind != llm.FormatJSONObject {
		t.Errorf("Prompted should coerce to FormatJSONObject; got %+v", seen[0].ResponseFormat)
	}
	if len(seen[0].Messages) < 2 || seen[0].Messages[0].Role != llm.RoleSystem {
		t.Errorf("expected prepended system message; messages = %+v", seen[0].Messages)
	}
}

// TestWrap_Tools_EmitsToolEnvelope — Tools coerces to JSONObject and
// the system text describes the synthetic respond_with envelope.
func TestWrap_Tools_EmitsToolEnvelope(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: `{"name":"respond_with","arguments":{}}`}, nil
	})
	cfg := snapshotWithProfile("custom/model", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeTools,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("custom/model", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	seen := rec.snapshot()
	if seen[0].ResponseFormat == nil || seen[0].ResponseFormat.Kind != llm.FormatJSONObject {
		t.Errorf("Tools should coerce to FormatJSONObject; got %+v", seen[0].ResponseFormat)
	}
	if seen[0].Messages[0].Role != llm.RoleSystem {
		t.Fatalf("expected system message at index 0; got %+v", seen[0].Messages[0])
	}
	if seen[0].Messages[0].Content.Text == nil ||
		!strings.Contains(*seen[0].Messages[0].Content.Text, "respond_with") {
		t.Errorf("expected respond_with envelope text; got %v", seen[0].Messages[0].Content.Text)
	}
}

// TestWrap_DowngradesOnSchemaError — Native → Prompted → Text.
// Each step emits llm.mode_downgraded. Final step succeeds.
func TestWrap_DowngradesOnSchemaError(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	sub, err := bus.Subscribe(t.Context(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeModeDowngraded},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Inner fails on JSON-schema and json_object; succeeds on text.
	rec := newRecorder(func(req llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		switch attempt {
		case 1, 2:
			return llm.CompleteResponse{}, fmt.Errorf("provider: invalid json_schema response")
		default:
			return llm.CompleteResponse{Content: "fallback text"}, nil
		}
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	resp, err := client.Complete(ctxWithIdentity(t), sampleRequest("openai/gpt-4o", llm.FormatJSONSchema))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "fallback text" {
		t.Errorf("Content = %q, want %q", resp.Content, "fallback text")
	}
	seen := rec.snapshot()
	if len(seen) != 3 {
		t.Fatalf("inner saw %d requests, want 3 (native → prompted → text)", len(seen))
	}
	// Confirm shapes per step.
	if seen[0].ResponseFormat == nil || seen[0].ResponseFormat.Kind != llm.FormatJSONSchema {
		t.Errorf("step 0 should be FormatJSONSchema; got %+v", seen[0].ResponseFormat)
	}
	if seen[1].ResponseFormat == nil || seen[1].ResponseFormat.Kind != llm.FormatJSONObject {
		t.Errorf("step 1 should be FormatJSONObject; got %+v", seen[1].ResponseFormat)
	}
	if seen[2].ResponseFormat != nil {
		t.Errorf("step 2 should drop ResponseFormat; got %+v", seen[2].ResponseFormat)
	}

	// Drain mode_downgraded events with a bounded read.
	events := drainEvents(t, sub, 2, 2*time.Second)
	if len(events) != 2 {
		t.Fatalf("expected 2 mode_downgraded events, got %d", len(events))
	}
}

// TestWrap_DowngradeExhausted — when every mode fails with a schema
// error, the wrapper surfaces ErrDowngradeExhausted.
func TestWrap_DowngradeExhausted(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{}, fmt.Errorf("provider: invalid json_schema response")
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("openai/gpt-4o", llm.FormatJSONSchema))
	if !errors.Is(err, llm.ErrDowngradeExhausted) {
		t.Fatalf("expected ErrDowngradeExhausted, got %v", err)
	}
	seen := rec.snapshot()
	if len(seen) != 3 {
		t.Errorf("inner saw %d requests, want 3", len(seen))
	}
}

// TestWrap_NonSchemaErrorTerminates — a transient/auth/5xx error
// terminates the chain immediately (no downgrade).
func TestWrap_NonSchemaErrorTerminates(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{}, fmt.Errorf("transient: connection reset")
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("openai/gpt-4o", llm.FormatJSONSchema))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if errors.Is(err, llm.ErrDowngradeExhausted) {
		t.Errorf("expected non-downgrade error; got %v", err)
	}
	if len(rec.snapshot()) != 1 {
		t.Errorf("expected 1 attempt (no downgrade); got %d", len(rec.snapshot()))
	}
}

// TestWrap_PassesThrough_WhenNoResponseFormat — a plain-text request
// (no ResponseFormat) is passed verbatim regardless of OutputMode.
func TestWrap_PassesThrough_WhenNoResponseFormat(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok"}, nil
	})
	cfg := snapshotWithProfile("openai/gpt-4o", llm.ModelProfile{
		ContextWindowTokens: 1000,
		OutputMode:          llm.OutputModeNative,
	})
	client := output.Wrap(rec, cfg, llm.Deps{Bus: bus})

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("openai/gpt-4o", ""))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	seen := rec.snapshot()
	if len(seen) != 1 || seen[0].ResponseFormat != nil {
		t.Errorf("expected pass-through (no rf); got %+v", seen)
	}
}

// TestWrap_NilInner_Panics — composition error caught at boot.
func TestWrap_NilInner_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic")
		}
	}()
	_ = output.Wrap(nil, llm.ConfigSnapshot{}, llm.Deps{})
}

// TestWrap_Close_Idempotent — Close twice is a no-op.
func TestWrap_Close_Idempotent(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{}, nil
	})
	client := output.Wrap(rec, snapshotWithProfile("m", llm.ModelProfile{ContextWindowTokens: 1000}), llm.Deps{Bus: bus})
	if err := client.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Complete after Close → ErrClientClosed.
	_, err := client.Complete(ctxWithIdentity(t), sampleRequest("m", ""))
	if !errors.Is(err, llm.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed; got %v", err)
	}
}

// drainEvents reads up to `want` events with a bounded wall-clock
// deadline; returns what it saw.
func drainEvents(t *testing.T, sub events.Subscription, want int, deadline time.Duration) []events.Event {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	var out []events.Event
	for len(out) < want {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer.C:
			return out
		}
	}
	return out
}

// goroutineBaseline reports the current goroutine count + a small
// tolerance window. Used by the concurrent-reuse test.
func goroutineBaseline() int {
	runtime.GC()
	return runtime.NumGoroutine()
}
