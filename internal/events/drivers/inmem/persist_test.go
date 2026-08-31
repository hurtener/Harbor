package inmem_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

func persistChunk(id identity.Quadruple, taskID, delta, kind string, done bool) events.Event {
	return events.Event{
		Type:     llm.EventTypeCompletionChunk,
		Identity: id,
		Payload: llm.CompletionChunkPayload{
			Identity: id,
			TaskID:   taskID,
			RunID:    id.RunID,
			Delta:    delta,
			Kind:     kind,
			Done:     done,
		},
	}
}

func TestPersistBatch_StoresReplayWithoutLiveDuplicate(t *testing.T) {
	cfg := defaultCfg()
	cfg.ReplayBufferSize = 64
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(1201)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	batch := []events.Event{
		persistChunk(id, "task-1201", "answer", "content", false),
		persistChunk(id, "task-1201", "thinking", "reasoning", false),
		persistChunk(id, "task-1201", "", "content", true),
	}
	live := bus.(events.LivePublisher)
	for i, ev := range batch {
		if err := live.PublishLive(context.Background(), ev); err != nil {
			t.Fatalf("PublishLive #%d: %v", i, err)
		}
	}
	liveEvents := drainN(t, sub, len(batch), time.Second)
	for i, ev := range liveEvents {
		if ev.Sequence != 0 {
			t.Fatalf("live event %d sequence = %d, want 0", i, ev.Sequence)
		}
	}
	rp := bus.(events.Replayer)
	if got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id)); err != nil {
		t.Fatalf("Replay before PersistBatch: %v", err)
	} else if len(got) != 0 {
		t.Fatalf("replay before PersistBatch = %d events, want 0", len(got))
	}

	persist := bus.(events.PersistBatchPublisher)
	if err := persist.PersistBatch(context.Background(), batch); err != nil {
		t.Fatalf("PersistBatch: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("persist-only batch fanned out duplicate event: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
	replayed, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id))
	if err != nil {
		t.Fatalf("Replay after PersistBatch: %v", err)
	}
	if len(replayed) != len(batch) {
		t.Fatalf("replay after PersistBatch = %d events, want %d", len(replayed), len(batch))
	}
	for i, ev := range replayed {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("replayed[%d] sequence = %d, want %d", i, ev.Sequence, i+1)
		}
		payload, ok := ev.Payload.(llm.CompletionChunkPayload)
		if !ok {
			t.Fatalf("replayed[%d] payload = %T, want CompletionChunkPayload", i, ev.Payload)
		}
		want := batch[i].Payload.(llm.CompletionChunkPayload)
		if payload.Delta != want.Delta || payload.Kind != want.Kind || payload.Done != want.Done {
			t.Fatalf("replayed[%d] payload = %+v, want delta/kind/done from %+v", i, payload, want)
		}
	}
}

func TestPersistBatch_CancellationPropagatesWithoutPersistence(t *testing.T) {
	cfg := defaultCfg()
	cfg.ReplayBufferSize = 8
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(1202)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = bus.(events.PersistBatchPublisher).PersistBatch(ctx, []events.Event{persistChunk(id, "task-1202", "delta", "content", true)})
	if err == nil {
		t.Fatal("PersistBatch with cancelled context returned nil")
	}
	if got, replayErr := bus.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id)); replayErr != nil {
		t.Fatalf("Replay after cancelled PersistBatch: %v", replayErr)
	} else if len(got) != 0 {
		t.Fatalf("cancelled PersistBatch persisted %d events, want 0", len(got))
	}
}

func TestPersistBatch_ConcurrentIdentityIsolation(t *testing.T) {
	cfg := defaultCfg()
	cfg.ReplayBufferSize = 256
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	const identities = 128
	ids := make([]identity.Quadruple, identities)
	errCh := make(chan error, identities)
	for i := range identities {
		ids[i] = identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("persist-tenant-%d", i),
			UserID:    fmt.Sprintf("persist-user-%d", i),
			SessionID: fmt.Sprintf("persist-session-%d", i),
		}, RunID: fmt.Sprintf("persist-run-%d", i)}
	}
	persist := bus.(events.PersistBatchPublisher)
	for i := range identities {
		go func(i int) {
			errCh <- persist.PersistBatch(context.Background(), []events.Event{
				persistChunk(ids[i], fmt.Sprintf("task-%d", i), fmt.Sprintf("delta-%d", i), "content", true),
			})
		}(i)
	}
	for range identities {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent PersistBatch: %v", err)
		}
	}
	rp := bus.(events.Replayer)
	for i, id := range ids {
		got, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id))
		if err != nil {
			t.Fatalf("Replay identity %d: %v", i, err)
		}
		if len(got) != 1 {
			t.Fatalf("identity %d replay = %d events, want 1", i, len(got))
		}
		if got[0].Identity != id || got[0].Sequence == 0 {
			t.Fatalf("identity %d replay event = %+v, want exact identity and durable sequence", i, got[0])
		}
	}
}
