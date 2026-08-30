package durable

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type blockingLoadStore struct {
	state.StateStore
	armed     atomic.Bool
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	freeOnce  sync.Once
}

func (s *blockingLoadStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if s.armed.Load() {
		s.enterOnce.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return state.StateRecord{}, ctx.Err()
		}
	}
	return s.StateStore.Load(ctx, q, kind)
}

func (s *blockingLoadStore) free() {
	s.freeOnce.Do(func() { close(s.release) })
}

func TestFenceGeneration_DropsQueuedEventAfterUnfence(t *testing.T) {
	cfg := config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     4,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         16,
		LegacyWritersDrained:     true,
	}
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	opened, err := New(context.Background(), cfg, auditpatterns.New(), store)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		_ = opened.Close(context.Background())
		_ = store.Close(context.Background())
	})
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
	waitForDurableSequence(t, b, 1)
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
	if len(got) != 2 {
		t.Fatalf("Replay returned %d events, want pre-fence and fresh events (old queued event must be absent): %+v", len(got), got)
	}
	wantIDs := []uint64{1, 3}
	for i, ev := range got {
		var subscriberID string
		switch payload := ev.Payload.(type) {
		case events.SubscriptionIdleClosedPayload:
			subscriberID = fmt.Sprint(payload.SubscriberID)
		case events.RedactedMap:
			subscriberID = fmt.Sprint(payload.Data["SubscriberID"])
		}
		if subscriberID != fmt.Sprint(wantIDs[i]) {
			t.Fatalf("Replay payload[%d]=%#v, want SubscriberID %d", i, ev.Payload, wantIDs[i])
		}
	}
	if got[1].Sequence != 2 {
		t.Fatalf("fresh event sequence=%d, want 2 (old queued event must not consume a sequence)", got[1].Sequence)
	}
}

func TestFenceGeneration_DurableAsyncAdmissionDoesNotWaitForLoad(t *testing.T) {
	cfg := config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     4,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         16,
		LegacyWritersDrained:     true,
	}
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	store := &blockingLoadStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	opened, err := New(context.Background(), cfg, auditpatterns.New(), store)
	if err != nil {
		_ = inner.Close(context.Background())
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		store.free()
		_ = opened.Close(context.Background())
		_ = inner.Close(context.Background())
	})
	store.armed.Store(true)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID:  "fence-generation-async-load-tenant",
		UserID:    "fence-generation-async-load-user",
		SessionID: "fence-generation-async-load-session",
	}}
	admitted := make(chan error, 1)
	go func() {
		admitted <- opened.(events.AsyncPublisher).PublishAsync(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
		})
	}()
	select {
	case err := <-admitted:
		if err != nil {
			t.Fatalf("PublishAsync: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("PublishAsync waited on durable StateStore.Load")
	}
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("commit worker did not reach blocked StateStore.Load")
	}
	store.free()
	if err := opened.(events.Flusher).Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}

func waitForDurableSequence(t *testing.T, b *bus, want uint64) {
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

func TestFenceGeneration_DurableMultiReplicaDropsQueuedEventAfterUnfence(t *testing.T) {
	cfg := config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 4,
		SubscriberBufferSize:     4,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         16,
		LegacyWritersDrained:     true,
	}
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	openedA, err := New(context.Background(), cfg, auditpatterns.New(), store)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("New bus A: %v", err)
	}
	openedB, err := New(context.Background(), cfg, auditpatterns.New(), store)
	if err != nil {
		_ = openedA.Close(context.Background())
		_ = store.Close(context.Background())
		t.Fatalf("New bus B: %v", err)
	}
	t.Cleanup(func() {
		_ = openedA.Close(context.Background())
		_ = openedB.Close(context.Background())
		_ = store.Close(context.Background())
	})
	busA := openedA.(*bus)
	id := identity.Quadruple{Identity: identity.Identity{
		TenantID:  "fence-generation-multi-tenant",
		UserID:    "fence-generation-multi-user",
		SessionID: "fence-generation-multi-session",
	}}

	publish := func(n uint64) error {
		return openedA.(events.AsyncPublisher).PublishAsync(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: n},
		})
	}

	// Hold bus A's lifecycle lock after the first event is persisted so its
	// worker cannot reach the queued event until bus B completes Fence→Unfence.
	busA.mu.Lock()
	locked := true
	defer func() {
		if locked {
			busA.mu.Unlock()
		}
	}()
	if err := publish(1); err != nil {
		t.Fatalf("first PublishAsync: %v", err)
	}
	waitForDurableSequence(t, busA, 1)
	if err := publish(2); err != nil {
		t.Fatalf("queued old PublishAsync: %v", err)
	}

	fencerB := openedB.(events.Fencer)
	if err := fencerB.Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("bus B Fence: %v", err)
	}
	if err := fencerB.Unfence(context.Background(), id.Identity); err != nil {
		t.Fatalf("bus B Unfence: %v", err)
	}

	busA.mu.Unlock()
	locked = false
	if err := openedA.(events.Flusher).Flush(context.Background()); err != nil {
		t.Fatalf("Flush queued old event: %v", err)
	}
	busA.publishMu.Lock()
	nextSeq := busA.nextSeq
	busA.publishMu.Unlock()
	if nextSeq != 1 {
		t.Fatalf("stale queued event advanced bus A sequence to %d, want 1", nextSeq)
	}
	if err := publish(3); err != nil {
		t.Fatalf("fresh post-Unfence PublishAsync: %v", err)
	}
	if err := openedA.(events.Flusher).Flush(context.Background()); err != nil {
		t.Fatalf("Flush fresh event: %v", err)
	}

	got, err := openedA.(events.Replayer).Replay(context.Background(), events.Cursor{
		SessionID: id.SessionID,
	}, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Replay returned %d events, want first and fresh only: %+v", len(got), got)
	}
	wantIDs := []uint64{1, 3}
	for i, ev := range got {
		var subscriberID string
		switch payload := ev.Payload.(type) {
		case events.SubscriptionIdleClosedPayload:
			subscriberID = fmt.Sprint(payload.SubscriberID)
		case events.RedactedMap:
			subscriberID = fmt.Sprint(payload.Data["SubscriberID"])
		}
		if subscriberID != fmt.Sprint(wantIDs[i]) {
			t.Fatalf("Replay payload[%d]=%#v, want SubscriberID %d", i, ev.Payload, wantIDs[i])
		}
	}
	if got[1].Sequence != 2 {
		t.Fatalf("fresh event sequence=%d, want 2 (stale queued event must not consume a sequence)", got[1].Sequence)
	}
}
