package llm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
)

// subscribeBus subscribes admin-scope to the given event types and
// returns the subscription + a teardown.
func subscribeBus(t *testing.T, bus events.EventBus, types ...events.EventType) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: types,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

func TestSafety_PlantedLeak_RawTextEmitsContextLeak(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeContextLeak)

	ctx := withIdentity(t)
	// Build a string ≥ 32 KiB (the heavy-output threshold default).
	leaky := strings.Repeat("X", 33*1024)
	req := llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &leaky}}},
	}
	_, err = client.Complete(ctx, req)
	if !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("err=%v, want ErrContextLeak", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != llm.EventTypeContextLeak {
			t.Errorf("event type=%q, want %q", ev.Type, llm.EventTypeContextLeak)
		}
		p, ok := ev.Payload.(llm.ContextLeakPayload)
		if !ok {
			t.Fatalf("event payload type=%T, want llm.ContextLeakPayload", ev.Payload)
		}
		if p.Model != "m" {
			t.Errorf("payload.Model=%q, want 'm'", p.Model)
		}
		if !strings.Contains(p.LeakSite, "Messages[0].Content.Text") {
			t.Errorf("payload.LeakSite=%q does not name the leak site", p.LeakSite)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe llm.context_leak within 2s")
	}
}

func TestSafety_TokenBudgetGuard(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// Tiny model: 200-token cap, 5% reserve → fail at >= 190 tokens.
	client, err := llm.Open(context.Background(), makeSnapshot("tiny", 200), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	sub := subscribeBus(t, deps.Bus, llm.EventTypeContextWindowExceeded)

	ctx := withIdentity(t)
	// ~3500 chars → ~875 tokens via chars/4 — comfortably over 190.
	huge := strings.Repeat("ab", 1500)
	_, err = client.Complete(ctx, llm.CompleteRequest{
		Model:    "tiny",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &huge}}},
	})
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("err=%v, want ErrContextWindowExceeded", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != llm.EventTypeContextWindowExceeded {
			t.Errorf("event type=%q, want %q", ev.Type, llm.EventTypeContextWindowExceeded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not observe llm.context_window_exceeded within 2s")
	}
}

func TestSafety_HappyPath_NoLeak(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	// 100K-token cap.
	client, err := llm.Open(context.Background(), makeSnapshot("m", 100_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	leakSub := subscribeBus(t, deps.Bus, llm.EventTypeContextLeak)
	budgetSub := subscribeBus(t, deps.Bus, llm.EventTypeContextWindowExceeded)

	ctx := withIdentity(t)
	text := "hello"
	resp, err := client.Complete(ctx, llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content == "" {
		t.Errorf("resp.Content empty")
	}
	// Neither warning event should fire on a small clean request.
	select {
	case ev := <-leakSub.Events():
		t.Errorf("unexpected leak event: %v", ev.Type)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case ev := <-budgetSub.Events():
		t.Errorf("unexpected window-exceeded event: %v", ev.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSafety_PartTextLeak(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()
	client, err := llm.Open(context.Background(), makeSnapshot("m", 1_000_000), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	ctx := withIdentity(t)
	// Leaky payload inside a multimodal text part.
	leaky := strings.Repeat("Y", 33*1024)
	req := llm.CompleteRequest{
		Model: "m",
		Messages: []llm.ChatMessage{{
			Role: llm.RoleUser,
			Content: llm.Content{Parts: []llm.ContentPart{
				{Type: llm.PartText, Text: leaky},
			}},
		}},
	}
	_, err = client.Complete(ctx, req)
	if !errors.Is(err, llm.ErrContextLeak) {
		t.Fatalf("err=%v, want ErrContextLeak", err)
	}
	if !strings.Contains(err.Error(), "Parts[0].Text") {
		t.Errorf("err=%q does not name the part-text leak site", err.Error())
	}
}
