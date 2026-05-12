package bifrost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// openBus is a small helper that constructs an in-memory bus for
// tests that subscribe to `llm.cost.recorded` and friends.
func openBus(t *testing.T) (events.EventBus, func()) {
	t.Helper()
	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
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
	return bus, func() { _ = bus.Close(context.Background()) }
}

// withIdentity returns a ctx that carries a Harbor identity.
func withIdentity(t *testing.T, ctx context.Context, sess string) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: sess}
	out, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return out
}

// TestDriver_Complete_Unary_HappyPath — text-only Complete via the
// stub client succeeds, translates the request faithfully, and
// surfaces the synthetic response + usage + cost.
func TestDriver_Complete_Unary_HappyPath(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenRouter, bus)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-1")
	text := "ping"
	resp, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "openai/gpt-5.3-chat",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("Content is empty")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Errorf("Usage not propagated: %+v", resp.Usage)
	}
	if resp.Cost.TotalCost == 0 {
		t.Errorf("Cost not propagated: %+v", resp.Cost)
	}
	if stub.calls.Load() != 1 {
		t.Errorf("stub.calls = %d want 1", stub.calls.Load())
	}
	req := stub.lastRequest()
	if req == nil || req.Provider != bfschemas.OpenRouter {
		t.Errorf("recorded request: %+v", req)
	}
}

// TestDriver_Complete_IdentityRejected — missing identity on ctx
// returns `llm.ErrIdentityMissing` BEFORE the stub is called.
func TestDriver_Complete_IdentityRejected(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, bus)

	text := "hi"
	_, err := drv.Complete(context.Background(), llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Errorf("err = %v; want ErrIdentityMissing", err)
	}
	if stub.calls.Load() != 0 {
		t.Errorf("stub was called despite missing identity: %d", stub.calls.Load())
	}
}

// TestDriver_Complete_ClosedReturnsErr — Complete after Close returns
// `ErrClientClosed`.
func TestDriver_Complete_AfterClose(t *testing.T) {
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := withIdentity(t, context.Background(), "s-1")
	text := "hi"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if !errors.Is(err, llm.ErrClientClosed) {
		t.Errorf("err = %v; want ErrClientClosed", err)
	}
}

// TestDriver_Close_Idempotent — second Close is a no-op.
func TestDriver_Close_Idempotent(t *testing.T) {
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	if err := drv.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := drv.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestDriver_Complete_StubErrorTranslation — bifrost-side error
// propagates through `translateError` and surfaces to the caller as
// a wrapped Go error with the status code preserved.
func TestDriver_Complete_StubErrorTranslation(t *testing.T) {
	stub := newStubClient()
	status := 401
	msg := "invalid api key"
	stub.chatHandler = func(req *bfschemas.BifrostChatRequest) (*bfschemas.BifrostChatResponse, *bfschemas.BifrostError) {
		return nil, &bfschemas.BifrostError{
			StatusCode: &status,
			Error:      &bfschemas.ErrorField{Message: msg},
		}
	}
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	ctx := withIdentity(t, context.Background(), "s-1")
	text := "x"
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err == nil {
		t.Fatalf("err is nil; expected translated bifrost error")
	}
	if !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), msg) {
		t.Errorf("err missing status/msg: %q", err.Error())
	}
}

// TestDriver_Complete_StreamCollectsCallbacks — stream path invokes
// OnContent per delta, OnReasoning is optional, and the assembled
// Content equals the concatenation of the deltas.
func TestDriver_Complete_StreamCollectsCallbacks(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, bus)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-1")
	var contentChunks []string
	var doneSeen bool
	text := "x"
	resp, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Stream:   true,
		OnContent: func(delta string, done bool) {
			if done {
				doneSeen = true
				return
			}
			contentChunks = append(contentChunks, delta)
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// `defaultStreamResponse` ships three chunks "hel","lo ","wor".
	if got := strings.Join(contentChunks, ""); got != "hello wor" {
		t.Errorf("content chunks = %q want %q", got, "hello wor")
	}
	if !doneSeen {
		t.Errorf("OnContent done=true was not invoked")
	}
	if resp.Content != "hello wor" {
		t.Errorf("resp.Content = %q want %q", resp.Content, "hello wor")
	}
	if resp.Usage.TotalTokens == 0 || resp.Cost.TotalCost == 0 {
		t.Errorf("stream usage/cost not propagated: usage=%+v cost=%+v", resp.Usage, resp.Cost)
	}
}

// TestDriver_Complete_StreamCancellation — cancelling the parent ctx
// mid-stream returns `ctx.Err()` promptly. The stub's blocking stream
// channel is abandoned (we don't wait for upstream drain).
func TestDriver_Complete_StreamCancellation(t *testing.T) {
	stub := newStubClient()
	stub.streamHandler = func(req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		return blockingStreamResponse(), nil
	}
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	ctx = withIdentity(t, ctx, "s-1")

	text := "x"
	deadline := time.Now().Add(2 * time.Second)
	// Cancel on first observed chunk — synchronous on the driver's
	// stream loop, so the second-chunk recv is the blocking site that
	// must honour ctx.Done(). AGENTS.md §11: no time.Sleep.
	var cancelOnce sync.Once
	_, err := drv.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
		Stream:   true,
		OnContent: func(_ string, _ bool) {
			cancelOnce.Do(cancel)
		},
	})
	if time.Now().After(deadline) {
		t.Errorf("Complete blocked past 2s deadline; cancellation didn't propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want context.Canceled", err)
	}
}

// TestDriver_Complete_CostEmit — successful Complete publishes
// `llm.cost.recorded` with the identity quadruple + model + cost +
// usage.
func TestDriver_Complete_CostEmit(t *testing.T) {
	bus, busCleanup := openBus(t)
	defer busCleanup()

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, bus)
	defer func() { _ = drv.Close(context.Background()) }()

	ctx := withIdentity(t, context.Background(), "s-emit")
	text := "x"
	_, err = drv.Complete(ctx, llm.CompleteRequest{
		Model:    "openai/gpt-5.3-chat",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != llm.EventTypeCostRecorded {
			t.Errorf("ev.Type = %q want %q", ev.Type, llm.EventTypeCostRecorded)
		}
		p, ok := ev.Payload.(llm.CostRecordedPayload)
		if !ok {
			t.Fatalf("payload type = %T", ev.Payload)
		}
		if p.Model != "openai/gpt-5.3-chat" {
			t.Errorf("payload Model = %q", p.Model)
		}
		if p.Identity.SessionID != "s-emit" {
			t.Errorf("payload Identity SessionID = %q", p.Identity.SessionID)
		}
		if p.Cost.TotalCost == 0 || p.Usage.TotalTokens == 0 {
			t.Errorf("payload cost/usage zero: %+v %+v", p.Cost, p.Usage)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("did not observe llm.cost.recorded within 2s")
	}
}

// TestDriver_init_RegistersBifrost — the package's init() registered
// the driver under "bifrost". (This DOES NOT call New — that would
// require an API key — but proves the registry knows about the name.)
func TestDriver_init_RegistersBifrost(t *testing.T) {
	names := llm.RegisteredDrivers()
	found := false
	for _, n := range names {
		if n == "bifrost" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("bifrost not in registered drivers: %v", names)
	}
}
