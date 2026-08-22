package durable_test

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
)

func TestDurable_TwoIndependentBuses_GlobalSequenceIsContiguous(t *testing.T) {
	store := newInmemStore(t)
	left, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("left New: %v", err)
	}
	right, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("right New: %v", err)
	}
	t.Cleanup(func() { _ = left.Close(context.Background()); _ = right.Close(context.Background()) })

	id := quad("tenant", "user", "shared-session")
	const n = 128
	start := make(chan struct{})
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bus := left
			if i%2 == 1 {
				bus = right
			}
			errCh <- bus.Publish(context.Background(), events.Event{
				Type: events.EventTypeRuntimeWarning, Identity: id,
				OccurredAt: time.Date(2026, 8, 22, 4, 0, i, 0, time.UTC),
				Payload:    runtimeWarn(fmt.Sprintf("event-%d", i)),
			})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Publish: %v", err)
		}
	}
	got, err := left.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != n {
		t.Fatalf("events = %d, want %d", len(got), n)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Sequence < got[j].Sequence })
	for i, ev := range got {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("sequence[%d] = %d, want %d", i, ev.Sequence, i+1)
		}
	}
}

func TestDurable_TwoIndependentBuses_ShareSessionFence(t *testing.T) {
	store := newInmemStore(t)
	left, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	right, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = left.Close(context.Background()); _ = right.Close(context.Background()) })
	id := quad("tenant", "user", "erased-session")
	if err := left.(events.Fencer).Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if err := right.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("must-drop"),
	}); err != nil {
		t.Fatalf("Publish to remotely fenced session: %v", err)
	}
	got, err := right.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("remotely fenced history = %+v, want empty", got)
	}
	if err := left.(events.Fencer).Unfence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	if err := right.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("after-unfence"),
	}); err != nil {
		t.Fatalf("Publish after remote unfence: %v", err)
	}
}
