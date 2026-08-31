package durable_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

func TestPersistOnlyChunks_ReplayInOrderWithoutSubscriberDuplicate(t *testing.T) {
	inner := newInmemStore(t)
	store := &blockingBatchStore{StateStore: inner}
	bus, rp := newDurableBus(t, store)
	id := quad("t-persist-chunks", "u-persist-chunks", "s-persist-chunks")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()
	store.batchCalls.Store(0)

	publisher := llm.NewBufferedChunkPublisherContext(context.Background(), bus, id, "task-persist-chunks", nil)
	const chunks = 33
	for i := range chunks {
		kind := "content"
		if i%3 == 1 {
			kind = "reasoning"
		}
		publisher.OnChunk(fmt.Sprintf("delta-%d", i), i == chunks-1, kind)
	}
	live := drainPersistN(t, sub, chunks, time.Second)
	if len(live) != chunks {
		t.Fatalf("live events = %d, want %d", len(live), chunks)
	}
	for i, ev := range live {
		if ev.Sequence != 0 {
			t.Fatalf("live[%d] sequence = %d, want 0", i, ev.Sequence)
		}
	}
	if calls := store.batchCalls.Load(); calls != 0 {
		t.Fatalf("SaveBatchIf calls before step-boundary flush = %d, want 0", calls)
	}
	if got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id)); err != nil {
		t.Fatalf("Replay before flush: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("replay before flush = %d events, want 0", len(got))
	}

	if err := publisher.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if calls := store.batchCalls.Load(); calls != 3 {
		t.Fatalf("SaveBatchIf calls after 33 chunks = %d, want 3", calls)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("persist-only flush fanned out duplicate: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
	replayed, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after flush: %v", err)
	}
	if len(replayed) != chunks {
		t.Fatalf("replayed events = %d, want %d", len(replayed), chunks)
	}
	for i, ev := range replayed {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("replayed[%d] sequence = %d, want %d", i, ev.Sequence, i+1)
		}
		payload, ok := ev.Payload.(events.RedactedMap)
		if !ok {
			t.Fatalf("replayed[%d] payload = %T, want RedactedMap", i, ev.Payload)
		}
		delta, _ := payload.Data["Delta"].(string)
		done, _ := payload.Data["Done"].(bool)
		if delta != fmt.Sprintf("delta-%d", i) || done != (i == chunks-1) {
			t.Fatalf("replayed[%d] payload = %+v, want ordered delta/done", i, payload.Data)
		}
	}

	// Reopening against the same StateStore keeps the chunk history and its
	// sequence authority; replay remains the durable source of truth.
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("close first bus: %v", err)
	}
	bus2, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), inner)
	if err != nil {
		t.Fatalf("restart durable bus: %v", err)
	}
	t.Cleanup(func() { _ = bus2.Close(context.Background()) })
	restarted, err := bus2.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if len(restarted) != chunks {
		t.Fatalf("replay after restart = %d events, want %d", len(restarted), chunks)
	}
	if restarted[len(restarted)-1].Sequence != chunks {
		t.Fatalf("replay after restart = %d events, last sequence %d, want %d/%d", len(restarted), restarted[len(restarted)-1].Sequence, chunks, chunks)
	}
}

func drainPersistN(t *testing.T, sub events.Subscription, n int, timeout time.Duration) []events.Event {
	t.Helper()
	got := make([]events.Event, 0, n)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for len(got) < n {
		select {
		case ev := <-sub.Events():
			got = append(got, ev)
		case <-deadline.C:
			t.Fatalf("received %d events, want %d", len(got), n)
		}
	}
	return got
}

type failingPersistStore struct {
	state.StateStore
	err   error
	fail  atomic.Bool
	calls atomic.Int64
}

func (s *failingPersistStore) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	s.calls.Add(1)
	if s.fail.Load() {
		return s.err
	}
	return s.StateStore.SaveBatchIf(ctx, expectations, writes)
}

func TestPersistOnlyChunks_FailureAndCancellationPropagate(t *testing.T) {
	storeErr := errors.New("persist chunk store failed")
	store := &failingPersistStore{StateStore: newInmemStore(t), err: storeErr}
	bus, rp := newDurableBus(t, store)
	store.fail.Store(true)
	id := quad("t-persist-failure", "u-persist-failure", "s-persist-failure")
	persist := bus.(events.PersistBatchPublisher)
	chunk := liveChunk(id, "task-persist-failure", "delta")
	if err := persist.PersistBatch(context.Background(), []events.Event{chunk}); !errors.Is(err, storeErr) {
		t.Fatalf("PersistBatch error = %v, want store error", err)
	}
	failedCalls := store.calls.Load()
	if failedCalls < 1 {
		t.Fatalf("failed PersistBatch SaveBatchIf calls = %d, want at least 1", failedCalls)
	}
	if got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id)); err != nil {
		t.Fatalf("Replay after failed PersistBatch: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("failed PersistBatch replayed %d events, want 0", len(got))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := persist.PersistBatch(ctx, []events.Event{chunk}); err == nil {
		t.Fatal("cancelled PersistBatch returned nil")
	}
	if store.calls.Load() != failedCalls {
		t.Fatalf("cancelled PersistBatch reached SaveBatchIf, calls = %d, want unchanged %d", store.calls.Load(), failedCalls)
	}
}
