package durable_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/state"
)

type blockingHeadLoadStore struct {
	state.StateStore
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *blockingHeadLoadStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	rec, err := s.StateStore.Load(ctx, id, kind)
	if kind == "events.durable.head" && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return state.StateRecord{}, ctx.Err()
		}
	}
	return rec, err
}

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
	for i := range n {
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
	if err := left.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("pre-existing"),
	}); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}
	if err := left.(events.Fencer).Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if err := right.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("must-drop"),
	}); err != nil {
		t.Fatalf("Publish to remotely fenced session: %v", err)
	}
	assertAllReadsFenced(t, right, id)
	restarted, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	assertAllReadsFenced(t, restarted, id)
	if err := right.(events.Fencer).Unfence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	if err := left.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("after-unfence"),
	}); err != nil {
		t.Fatalf("Publish after remote unfence: %v", err)
	}
	got, err := left.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil || len(got) != 2 {
		t.Fatalf("Replay after Unfence = %+v, %v; want two events", got, err)
	}
}

func TestDurable_ReplayRechecksFenceAfterConcurrentRead(t *testing.T) {
	store := newInmemStore(t)
	writer, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatal(err)
	}
	id := quad("tenant", "user", "concurrent-fence")
	if err := writer.Publish(context.Background(), events.Event{
		Type: events.EventTypeRuntimeWarning, Identity: id, Payload: runtimeWarn("pre-existing"),
	}); err != nil {
		t.Fatalf("seed Publish: %v", err)
	}
	blocking := &blockingHeadLoadStore{StateStore: store, entered: make(chan struct{}), release: make(chan struct{})}
	reader, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), blocking)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close(context.Background()); _ = reader.Close(context.Background()) })

	blocking.armed.Store(true)
	type replayResult struct {
		events []events.Event
		err    error
	}
	result := make(chan replayResult, 1)
	go func() {
		got, replayErr := reader.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
		result <- replayResult{events: got, err: replayErr}
	}()
	select {
	case <-blocking.entered:
	case <-time.After(time.Second):
		t.Fatal("Replay did not reach blocked head load")
	}
	if err := writer.(events.Fencer).Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("concurrent Fence: %v", err)
	}
	close(blocking.release)
	select {
	case got := <-result:
		if got.err != nil || len(got.events) != 0 {
			t.Fatalf("Replay crossing Fence = %+v, %v; want empty", got.events, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Replay did not complete after release")
	}
}

func assertAllReadsFenced(t *testing.T, bus events.EventBus, id identity.Quadruple) {
	t.Helper()
	ctx := context.Background()
	filter := filterFor(id)
	if got, err := bus.(events.Replayer).Replay(ctx, events.Cursor{SessionID: id.SessionID}, filter); err != nil || len(got) != 0 {
		t.Fatalf("Replay fenced = %+v, %v", got, err)
	}
	history := bus.(events.HistoryReplayer)
	if _, _, _, err := history.Bounds(ctx, filter); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("Bounds fenced = %v, want ErrNoHistory", err)
	}
	wire := prototypes.EventFilter{TenantIDs: []string{id.TenantID}, UserIDs: []string{id.UserID}, SessionIDs: []string{id.SessionID}}
	if page, err := history.ListWindow(ctx, events.EventListQuery{Filter: wire, Limit: 10}); err != nil || len(page.Events) != 0 {
		t.Fatalf("ListWindow fenced = %+v, %v", page, err)
	}
	if page, err := bus.(events.EventMetadataReplayer).ListWindowMetadata(ctx, events.EventListQuery{Filter: wire, Limit: 10}); err != nil || len(page.Events) != 0 {
		t.Fatalf("ListWindowMetadata fenced = %+v, %v", page, err)
	}
	if page, err := bus.(events.ProjectionSource).Page(ctx, 0, 10); err != nil || len(page.Events) != 0 {
		t.Fatalf("Projection Page fenced = %+v, %v", page, err)
	}
}
