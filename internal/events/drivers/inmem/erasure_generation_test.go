package inmem

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

func TestFenceGeneration_DropsQueuedEventAfterUnfence(t *testing.T) {
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     4,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         16,
	}
	opened, err := New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	b := opened.(*bus)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID:  "fence-generation-tenant",
		UserID:    "fence-generation-user",
		SessionID: "fence-generation-session",
	}}

	publish := func(n uint64) error {
		return opened.(events.AsyncPublisher).PublishAsync(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: n},
		})
	}

	// Hold the subscriber lifecycle lock so the first commit reaches its
	// post-persistence fan-out and remains blocked while later requests queue.
	b.mu.Lock()
	locked := true
	defer func() {
		if locked {
			b.mu.Unlock()
		}
	}()
	if err := publish(1); err != nil {
		t.Fatalf("first PublishAsync: %v", err)
	}
	waitForSequence(t, b, 1)
	if err := publish(2); err != nil {
		t.Fatalf("queued old PublishAsync: %v", err)
	}

	fencer := opened.(events.Fencer)
	if err := fencer.Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if err := fencer.Unfence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	if err := publish(3); err != nil {
		t.Fatalf("fresh PublishAsync: %v", err)
	}

	b.mu.Unlock()
	locked = false
	if err := opened.(events.Flusher).Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	got, err := opened.(events.Replayer).Replay(context.Background(), events.Cursor{
		SessionID: id.SessionID,
	}, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Replay returned %d events, want only fresh event: %+v", len(got), got)
	}
	if got[0].Sequence != 2 {
		t.Fatalf("fresh event sequence=%d, want 2 (old queued event must not consume a sequence)", got[0].Sequence)
	}
	payload, ok := got[0].Payload.(events.SubscriptionIdleClosedPayload)
	if !ok || payload.SubscriberID != 3 {
		t.Fatalf("Replay payload=%#v, want fresh SubscriberID 3", got[0].Payload)
	}
}

func waitForSequence(t *testing.T, b *bus, want uint64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		b.publishMu.Lock()
		got := b.nextSeq
		b.publishMu.Unlock()
		if got >= want {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("sequence did not reach %d (got %d)", want, got)
		}
	}
}
