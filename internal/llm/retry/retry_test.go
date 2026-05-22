package retry_test

import (
	"context"
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
	"github.com/hurtener/Harbor/internal/llm/retry"
)

type recordingDriver struct {
	respCB func(req llm.CompleteRequest, attempt int) (llm.CompleteResponse, error)
	seen   []llm.CompleteRequest
	mu     sync.Mutex
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

func snapshotWithProfile(profile llm.ModelProfile) llm.ConfigSnapshot {
	return llm.ConfigSnapshot{
		Driver:               "mock",
		ContextWindowReserve: 0.05,
		HeavyOutputThreshold: 32_768,
		ModelProfiles:        map[string]llm.ModelProfile{"m": profile},
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

func sampleRequest(validator func(llm.CompleteResponse) error) llm.CompleteRequest {
	t := "hello"
	return llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{
			{Role: llm.RoleUser, Content: llm.Content{Text: &t}},
		},
		Validator: validator,
	}
}

// TestWrap_NoValidator_PassesThrough — when no validator, the wrapper
// is a pure pass-through.
func TestWrap_NoValidator_PassesThrough(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "ok"}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	resp, err := client.Complete(ctxWithIdentity(t), sampleRequest(nil))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("Content = %q, want %q", resp.Content, "ok")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("inner saw %d requests; expected 1", got)
	}
}

// TestWrap_ValidatorPasses_NoRetry — happy path: validator returns nil
// on the first attempt; no retry.
func TestWrap_ValidatorPasses_NoRetry(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: `{"ok":true}`}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(r llm.CompleteResponse) error {
		if r.Content == "" {
			return errors.New("empty")
		}
		return nil
	}

	resp, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("expected non-empty content")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("inner saw %d requests; expected 1 (no retry)", got)
	}
}

// TestWrap_SingleRetryThenPass — validator fails once, second attempt
// passes. One retry-event emitted; final response returned.
func TestWrap_SingleRetryThenPass(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	sub, err := bus.Subscribe(t.Context(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeRetryWithFeedback},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	rec := newRecorder(func(_ llm.CompleteRequest, attempt int) (llm.CompleteResponse, error) {
		if attempt == 1 {
			return llm.CompleteResponse{Content: "bad"}, nil
		}
		return llm.CompleteResponse{Content: "good"}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	var validatorCalls atomic.Int64
	validator := func(r llm.CompleteResponse) error {
		validatorCalls.Add(1)
		if r.Content != "good" {
			return errors.New("not good")
		}
		return nil
	}

	resp, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "good" {
		t.Errorf("Content = %q, want %q", resp.Content, "good")
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Errorf("inner saw %d requests; expected 2", got)
	}
	if validatorCalls.Load() != 2 {
		t.Errorf("validator called %d times; expected 2", validatorCalls.Load())
	}

	// One retry-event.
	evs := drainEvents(t, sub, 1, 2*time.Second)
	if len(evs) != 1 {
		t.Fatalf("expected 1 retry_with_feedback event, got %d", len(evs))
	}
	payload, ok := evs[0].Payload.(llm.RetryWithFeedbackPayload)
	if !ok {
		t.Fatalf("event payload is %T, want RetryWithFeedbackPayload", evs[0].Payload)
	}
	if payload.Attempt != 1 {
		t.Errorf("payload.Attempt = %d, want 1", payload.Attempt)
	}
	if payload.MaxRetries != 3 {
		t.Errorf("payload.MaxRetries = %d, want 3", payload.MaxRetries)
	}
	if payload.Identity.TenantID == "" {
		t.Errorf("payload.Identity is zero-valued")
	}

	// Confirm the second attempt's messages were augmented with the
	// rejected response + corrective turn.
	seen := rec.snapshot()
	second := seen[1]
	if len(second.Messages) <= len(seen[0].Messages) {
		t.Errorf("second attempt should have more messages; got %d <= %d",
			len(second.Messages), len(seen[0].Messages))
	}
	lastMsg := second.Messages[len(second.Messages)-1]
	if lastMsg.Role != llm.RoleUser || lastMsg.Content.Text == nil ||
		!strings.Contains(*lastMsg.Content.Text, "failed validation") {
		t.Errorf("last message of retry should be the corrective user turn; got %+v", lastMsg)
	}
}

// TestWrap_BoundedRetries — validator keeps failing; wrapper exits
// after MaxRetries with ErrRetryExhausted containing the chain.
func TestWrap_BoundedRetries(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "always bad"}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 2})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(r llm.CompleteResponse) error {
		return fmt.Errorf("rejected: %s", r.Content)
	}

	_, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator))
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Fatalf("expected ErrRetryExhausted, got %v", err)
	}
	// Total attempts = MaxRetries + 1 (initial).
	if got := len(rec.snapshot()); got != 3 {
		t.Errorf("inner saw %d attempts; expected 3 (initial + 2 retries)", got)
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error should contain validator chain; got %v", err)
	}
}

// TestWrap_DefaultMaxRetries — when ModelProfile.MaxRetries is unset
// AND the snapshot was constructed without applyDefaults (programmatic
// caller), the wrapper falls back to DefaultMaxRetries (1).
func TestWrap_DefaultMaxRetries(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "bad"}, nil
	})
	// No MaxRetries → DefaultMaxRetries (1).
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(_ llm.CompleteResponse) error { return errors.New("nope") }
	_, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator))
	if !errors.Is(err, llm.ErrRetryExhausted) {
		t.Fatalf("expected ErrRetryExhausted, got %v", err)
	}
	// 1 retry → 2 total attempts.
	if got := len(rec.snapshot()); got != 2 {
		t.Errorf("inner saw %d attempts; expected 2", got)
	}
}

// TestWrap_InnerError_NoRetry — when the inner call errors (not the
// validator), retry does NOT trigger. The error bubbles immediately.
func TestWrap_InnerError_NoRetry(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{}, errors.New("transient: connection reset")
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 3})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	validator := func(_ llm.CompleteResponse) error { return errors.New("nope") }
	_, err := client.Complete(ctxWithIdentity(t), sampleRequest(validator))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if errors.Is(err, llm.ErrRetryExhausted) {
		t.Errorf("inner errors should NOT trigger retry; got ErrRetryExhausted")
	}
	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("inner saw %d attempts; expected 1", got)
	}
}

// TestWrap_CtxCancel_AbortsLoop — a cancelled ctx stops the loop
// immediately.
func TestWrap_CtxCancel_AbortsLoop(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{Content: "x"}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 5})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	ctx, cancel := context.WithCancel(ctxWithIdentity(t))
	cancel() // pre-cancel

	validator := func(_ llm.CompleteResponse) error { return errors.New("nope") }
	_, err := client.Complete(ctx, sampleRequest(validator))
	if err == nil {
		t.Fatalf("expected ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
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
	_ = retry.Wrap(nil, llm.ConfigSnapshot{}, llm.Deps{})
}

// TestWrap_Close_Idempotent — Close twice is a no-op.
func TestWrap_Close_Idempotent(t *testing.T) {
	t.Parallel()
	bus := testBus(t)
	rec := newRecorder(func(_ llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		return llm.CompleteResponse{}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 1})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	if err := client.Close(context.Background()); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := client.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	_, err := client.Complete(ctxWithIdentity(t), sampleRequest(nil))
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

func goroutineBaseline() int {
	runtime.GC()
	return runtime.NumGoroutine()
}
