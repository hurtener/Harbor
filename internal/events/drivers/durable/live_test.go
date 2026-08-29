package durable_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// blockingBatchStore holds one durable SaveBatchIf call at the persistence
// boundary. PublishLive must remain deliverable while this store is blocked:
// the live lane is allowed to share the bus, but not the durable write path.
type blockingBatchStore struct {
	state.StateStore
	armed      atomic.Bool
	entered    chan struct{}
	release    chan struct{}
	enterOnce  sync.Once
	batchCalls atomic.Int64
}

func (s *blockingBatchStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.batchCalls.Add(1)
	if s.armed.Load() {
		s.enterOnce.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func liveChunk(id identity.Quadruple, taskID, delta string) events.Event {
	return events.Event{
		Type:     llm.EventTypeCompletionChunk,
		Identity: id,
		Payload: llm.CompletionChunkPayload{
			Identity: id,
			TaskID:   taskID,
			RunID:    id.RunID,
			Delta:    delta,
			Kind:     "content",
		},
	}
}

func TestDurable_PublishLive_BypassesBlockedStoreAndLeavesReplayIntact(t *testing.T) {
	inner := newInmemStore(t)
	store := &blockingBatchStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	bus, rp := newDurableBus(t, store)
	// durable.New may adopt the initial sequence authority. Only count calls
	// made after the test arms the intentionally blocked persistence boundary.
	store.batchCalls.Store(0)
	id := quad("t-live", "u-live", "s-live")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	store.armed.Store(true)
	durableDone := make(chan error, 1)
	go func() {
		durableDone <- bus.Publish(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  runtimeWarn("durable lifecycle"),
		})
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("durable Publish did not reach blocked SaveBatchIf")
	}

	liveDone := make(chan error, 1)
	go func() {
		liveDone <- bus.PublishLive(context.Background(), liveChunk(id, "task-live", "delta-live"))
	}()
	select {
	case err := <-liveDone:
		if err != nil {
			t.Fatalf("PublishLive while durable store blocked: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PublishLive waited for the blocked durable SaveBatchIf")
	}
	if calls := store.batchCalls.Load(); calls != 1 {
		t.Fatalf("live publish changed durable SaveBatchIf call count to %d, want 1 blocked lifecycle write", calls)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != llm.EventTypeCompletionChunk || ev.Sequence != 0 {
			t.Fatalf("received live event = %+v, want completion chunk with Sequence 0", ev)
		}
		if payload, ok := ev.Payload.(llm.CompletionChunkPayload); !ok || payload.Delta != "delta-live" {
			t.Fatalf("received live payload = %#v, want typed delta-live payload", ev.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("live chunk was not delivered while durable write was blocked")
	}

	close(store.release)
	if err := <-durableDone; err != nil {
		t.Fatalf("durable Publish: %v", err)
	}
	if calls := store.batchCalls.Load(); calls != 1 {
		t.Fatalf("durable write call count after release = %d, want 1", calls)
	}
	if n := countDurableEntries(t, inner); n != 1 {
		t.Fatalf("durable entry count after lifecycle publish = %d, want 1", n)
	}

	// A burst of live chunks must remain fan-out-only: no durable entry,
	// head, authority, sequence, or replay row is added for any chunk.
	const liveChunks = 32
	for i := range liveChunks {
		if err := bus.PublishLive(context.Background(), liveChunk(id, "task-live", fmt.Sprintf("delta-%d", i))); err != nil {
			t.Fatalf("PublishLive #%d: %v", i, err)
		}
	}
	if calls := store.batchCalls.Load(); calls != 1 {
		t.Fatalf("live burst changed durable SaveBatchIf call count to %d, want 1", calls)
	}
	if n := countDurableEntries(t, inner); n != 1 {
		t.Fatalf("durable entry count after %d live chunks = %d, want 1", liveChunks, n)
	}
	replay, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after live burst: %v", err)
	}
	if len(replay) != 1 || replay[0].Type != events.EventTypeRuntimeWarning || replay[0].Sequence != 1 {
		t.Fatalf("replay after live burst = %#v, want only durable lifecycle Sequence 1", replay)
	}

	// Durable sequencing resumes normally after the live lane: the next
	// lifecycle event owns Sequence 2 and is the only new replay row.
	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  runtimeWarn("next durable lifecycle"),
	}); err != nil {
		t.Fatalf("second durable Publish: %v", err)
	}
	replay, err = rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after second lifecycle: %v", err)
	}
	if len(replay) != 2 || replay[1].Sequence != 2 {
		t.Fatalf("replay after second lifecycle = %#v, want durable sequences 1,2", replay)
	}
}

func TestDurable_PublishLive_ConcurrentIdentityIsolation(t *testing.T) {
	bus, _ := newDurableBus(t, newInmemStore(t))
	const identities = 128
	ids := make([]identity.Quadruple, identities)
	subs := make([]events.Subscription, identities)
	for i := range identities {
		ids[i] = identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("live-tenant-%d", i),
			UserID:    fmt.Sprintf("live-user-%d", i),
			SessionID: fmt.Sprintf("live-session-%d", i),
		}, RunID: fmt.Sprintf("live-run-%d", i)}
		var err error
		subs[i], err = bus.Subscribe(context.Background(), filterFor(ids[i]))
		if err != nil {
			t.Fatalf("Subscribe(%d): %v", i, err)
		}
		defer subs[i].Cancel()
	}

	var wg sync.WaitGroup
	errCh := make(chan error, identities)
	for i := range identities {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := bus.PublishLive(context.Background(), liveChunk(ids[i], fmt.Sprintf("task-%d", i), fmt.Sprintf("delta-%d", i))); err != nil {
				errCh <- fmt.Errorf("PublishLive(%d): %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	for i, sub := range subs {
		select {
		case ev := <-sub.Events():
			if ev.Identity != ids[i] || ev.Sequence != 0 {
				t.Fatalf("subscriber %d received %+v, want identity %+v and Sequence 0", i, ev, ids[i])
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive its live chunk", i)
		}
	}
}
