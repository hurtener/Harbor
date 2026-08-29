package durable_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
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

func publishLive(bus events.EventBus, ctx context.Context, ev events.Event) error {
	live, ok := bus.(events.LivePublisher)
	if !ok {
		return fmt.Errorf("bus does not implement events.LivePublisher")
	}
	return live.PublishLive(ctx, ev)
}

type blockingRedactor struct {
	inner        audit.Redactor
	started      chan struct{}
	release      chan struct{}
	ignoreCancel bool
	once         sync.Once
}

func (r *blockingRedactor) Redact(ctx context.Context, payload any) (any, error) {
	r.once.Do(func() { close(r.started) })
	if r.ignoreCancel {
		<-r.release
		// This test seam deliberately models a redactor that finishes after
		// its caller has been cancelled, so the post-fence ctx check is the
		// only cutoff that can protect the subscriber.
		return r.inner.Redact(context.Background(), payload)
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.inner.Redact(ctx, payload)
}

type liveBarrierPayload struct {
	events.Sealed
	Value string
}

type advancingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *advancingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *advancingClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
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
		liveDone <- publishLive(bus, context.Background(), liveChunk(id, "task-live", "delta-live"))
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
		if err := publishLive(bus, context.Background(), liveChunk(id, "task-live", fmt.Sprintf("delta-%d", i))); err != nil {
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

func TestDurable_PublishLive_MixedDurableBackpressureStaysTransient(t *testing.T) {
	inner := newInmemStore(t)
	store := &blockingBatchStore{
		StateStore: inner,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	clock := &advancingClock{now: time.Unix(1_700_000_000, 0)}
	cfg := durableCfg()
	cfg.SubscriberBufferSize = 1
	cfg.DropWindow = 10 * time.Millisecond
	cfg.ReplayBufferSize = 8
	bus, err := durable.New(context.Background(), cfg, auditpatterns.New(), store, durable.WithClock(clock))
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	rp := bus.(events.Replayer)
	id := quad("t-mixed-live", "u-mixed-live", "s-mixed-live")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  runtimeWarn("first durable"),
	}); err != nil {
		t.Fatalf("first durable Publish: %v", err)
	}
	// Keep durable seq1 unread in the one-slot subscriber and block the next
	// durable write. The live call must still fan out and must not touch this
	// StateStore call or the durable sequence authority.
	store.batchCalls.Store(0)
	store.armed.Store(true)
	durableDone := make(chan error, 1)
	go func() {
		durableDone <- bus.Publish(context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  runtimeWarn("blocked second durable"),
		})
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("second durable Publish did not reach blocked SaveBatchIf")
	}
	clock.Advance(20 * time.Millisecond)
	beforeLiveCalls := store.batchCalls.Load()
	liveDone := make(chan error, 1)
	go func() {
		liveDone <- publishLive(bus, context.Background(), liveChunk(id, "task-mixed", "live-delta"))
	}()
	select {
	case err := <-liveDone:
		if err != nil {
			t.Fatalf("PublishLive during blocked durable write: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PublishLive waited for blocked durable persistence")
	}
	if calls := store.batchCalls.Load(); calls != beforeLiveCalls {
		t.Fatalf("PublishLive changed StateStore calls from %d to %d", beforeLiveCalls, calls)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeBusDropped || ev.Sequence != 0 {
			t.Fatalf("mixed live drop notice = %+v, want bus.dropped Sequence 0", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("mixed live saturation did not emit a transient drop notice")
	}

	close(store.release)
	if err := <-durableDone; err != nil {
		t.Fatalf("second durable Publish: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeRuntimeWarning || ev.Sequence != 2 {
			t.Fatalf("next durable event = %+v, want runtime.warning Sequence 2", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("next durable event was not delivered")
	}
	if n := countDurableEntries(t, inner); n != 2 {
		t.Fatalf("durable entry count after mixed live publish = %d, want 2", n)
	}
	replay, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, filterFor(id))
	if err != nil {
		t.Fatalf("Replay after mixed live publish: %v", err)
	}
	if len(replay) != 2 || replay[0].Sequence != 1 || replay[1].Sequence != 2 {
		t.Fatalf("replay after mixed live publish = %#v, want durable sequences 1,2", replay)
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
			if err := publishLive(bus, context.Background(), liveChunk(ids[i], fmt.Sprintf("task-%d", i), fmt.Sprintf("delta-%d", i))); err != nil {
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

func TestDurable_PublishLive_FenceCutsOffInFlightRedaction(t *testing.T) {
	redactor := &blockingRedactor{
		inner:   auditpatterns.New(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	store := newInmemStore(t)
	bus, err := durable.New(context.Background(), durableCfg(), redactor, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := quad("t-fence-live", "u-fence-live", "s-fence-live")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	publishDone := make(chan error, 1)
	go func() {
		publishDone <- publishLive(bus, context.Background(), events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  liveBarrierPayload{Value: "in-flight"},
		})
	}()
	select {
	case <-redactor.started:
	case <-time.After(time.Second):
		t.Fatal("live publish did not reach redaction barrier")
	}
	if err := bus.(events.Fencer).Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	close(redactor.release)
	if err := <-publishDone; err != nil {
		t.Fatalf("in-flight PublishLive: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("live event reached subscriber after Fence returned: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDurable_PublishLive_CancelledAfterFenceUnfenceCannotReachReusedSession(t *testing.T) {
	redactor := &blockingRedactor{
		inner:        auditpatterns.New(),
		started:      make(chan struct{}),
		release:      make(chan struct{}),
		ignoreCancel: true,
	}
	store := newInmemStore(t)
	bus, err := durable.New(context.Background(), durableCfg(), redactor, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := quad("t-fence-reuse-live", "u-fence-reuse-live", "s-fence-reuse-live")
	sub, err := bus.Subscribe(context.Background(), filterFor(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	oldCtx, cancel := context.WithCancel(context.Background())
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- publishLive(bus, oldCtx, events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: id,
			Payload:  liveBarrierPayload{Value: "old-run"},
		})
	}()
	select {
	case <-redactor.started:
	case <-time.After(time.Second):
		t.Fatal("old live publish did not reach redaction barrier")
	}

	fencer := bus.(events.Fencer)
	if err := fencer.Fence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Fence: %v", err)
	}
	if err := fencer.Unfence(context.Background(), id.Identity); err != nil {
		t.Fatalf("Unfence: %v", err)
	}
	cancel()
	close(redactor.release)
	if err := <-publishDone; err != nil {
		t.Fatalf("cancelled old PublishLive: %v", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("cancelled old live event reached reused session: %+v", ev)
	default:
	}

	if err := publishLive(bus, context.Background(), liveChunk(id, "task-new", "new-live")); err != nil {
		t.Fatalf("new-session PublishLive: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Identity != id || ev.Sequence != 0 {
			t.Fatalf("new-session live event = %+v, want identity %+v and Sequence 0", ev, id)
		}
	case <-time.After(time.Second):
		t.Fatal("new-session live event was not delivered after Unfence")
	}
}
