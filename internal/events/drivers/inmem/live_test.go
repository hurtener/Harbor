package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

func liveWarning(id identity.Quadruple, n uint64) events.Event {
	return events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: n},
	}
}

func liveFilter(id identity.Quadruple) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
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

func TestPublishLive_DeliversWithoutReplayOrSequenceAdvance(t *testing.T) {
	cfg := defaultCfg()
	cfg.ReplayBufferSize = 32
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(901)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := publishLive(bus, context.Background(), liveWarning(id, 1)); err != nil {
		t.Fatalf("PublishLive: %v", err)
	}
	got := drainN(t, sub, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("received %d live events, want 1", len(got))
	}
	if got[0].Sequence != 0 {
		t.Fatalf("live event Sequence = %d, want 0", got[0].Sequence)
	}

	rp, ok := bus.(events.Replayer)
	if !ok {
		t.Fatal("inmem bus does not implement events.Replayer")
	}
	if replay, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id)); err != nil {
		t.Fatalf("Replay before durable publish: %v", err)
	} else if len(replay) != 0 {
		t.Fatalf("live event entered replay history: %#v", replay)
	}

	if err := bus.Publish(context.Background(), liveWarning(id, 2)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	durable := drainN(t, sub, 1, time.Second)
	if len(durable) != 1 || durable[0].Sequence != 1 {
		t.Fatalf("first durable event after live publish = %#v, want one event at Sequence 1", durable)
	}
	replay, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id))
	if err != nil {
		t.Fatalf("Replay after durable publish: %v", err)
	}
	if len(replay) != 1 || replay[0].Sequence != 1 {
		t.Fatalf("replay after live publish = %#v, want only durable Sequence 1", replay)
	}
}

func TestPublishLive_AppliesIdentityAndTypeFilters(t *testing.T) {
	bus := newBus(t)
	target := mkID(902)
	other := mkID(903)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: target.TenantID, User: target.UserID, Session: target.SessionID,
		Types: []events.EventType{events.EventTypeRuntimeWarning},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	for _, ev := range []events.Event{
		liveWarning(other, 1),
		{Type: events.EventTypeRuntimeError, Identity: target, Payload: events.SubscriptionIdleClosedPayload{SubscriberID: 2}},
	} {
		if err := publishLive(bus, context.Background(), ev); err != nil {
			t.Fatalf("PublishLive(%s): %v", ev.Type, err)
		}
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("filtered live event was delivered: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}

	if err := publishLive(bus, context.Background(), liveWarning(target, 3)); err != nil {
		t.Fatalf("PublishLive matching event: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Identity != target || ev.Type != events.EventTypeRuntimeWarning || ev.Sequence != 0 {
			t.Fatalf("matching live event = %+v, want target/runtime.warning/Sequence 0", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("matching live event was not delivered")
	}
}

func TestPublishLive_ConcurrentIdentityIsolation(t *testing.T) {
	cfg := defaultCfg()
	cfg.SubscriberBufferSize = 4
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	const identities = 128
	ids := make([]identity.Quadruple, identities)
	subs := make([]events.Subscription, identities)
	for i := range identities {
		ids[i] = identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("live-tenant-%d", i),
			UserID:    fmt.Sprintf("live-user-%d", i),
			SessionID: fmt.Sprintf("live-session-%d", i),
		}, RunID: fmt.Sprintf("live-run-%d", i)}
		subs[i], err = bus.Subscribe(context.Background(), liveFilter(ids[i]))
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
			if err := publishLive(bus, context.Background(), liveWarning(ids[i], uint64(i))); err != nil {
				errCh <- fmt.Errorf("publish %d: %w", i, err)
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
			if ev.Identity != ids[i] {
				t.Fatalf("subscriber %d received identity %+v, want %+v", i, ev.Identity, ids[i])
			}
			if ev.Sequence != 0 {
				t.Fatalf("subscriber %d received live Sequence %d, want 0", i, ev.Sequence)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive its live event", i)
		}
	}
}

func TestPublishLive_RejectsInvalidEventsWithoutFanout(t *testing.T) {
	bus := newBus(t)
	id := mkID(904)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	bad := liveWarning(id, 1)
	bad.Sequence = 99
	if err := publishLive(bus, context.Background(), bad); !errors.Is(err, events.ErrSequenceProvided) {
		t.Fatalf("PublishLive(sequence=99) = %v, want ErrSequenceProvided", err)
	}
	missing := liveWarning(id, 2)
	missing.Identity.SessionID = ""
	if err := publishLive(bus, context.Background(), missing); !errors.Is(err, events.ErrIdentityRequired) {
		t.Fatalf("PublishLive(missing identity) = %v, want ErrIdentityRequired", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("invalid live event reached subscriber: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestPublishLive_CancelledContextDoesNotFanout(t *testing.T) {
	bus := newBus(t)
	id := mkID(905)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := publishLive(bus, ctx, liveWarning(id, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishLive(cancelled) = %v, want context.Canceled", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("cancelled live event reached subscriber: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestPublishLive_SaturationDropNoticeStaysTransient(t *testing.T) {
	cfg := defaultCfg()
	cfg.SubscriberBufferSize = 2
	cfg.DropWindow = time.Nanosecond
	cfg.ReplayBufferSize = 8
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(906)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Keep the two-slot subscriber saturated while a live burst forces the
	// drop notice path. The notice must remain Sequence 0 and must not enter
	// the replay ring or advance the next durable sequence.
	for i := uint64(0); i < 4; i++ {
		if err := publishLive(bus, context.Background(), liveWarning(id, i)); err != nil {
			t.Fatalf("PublishLive(%d): %v", i, err)
		}
	}
	var sawDrop bool
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.EventTypeBusDropped {
				sawDrop = true
				if ev.Sequence != 0 {
					t.Fatalf("live drop notice Sequence = %d, want 0", ev.Sequence)
				}
			}
		default:
			goto drained
		}
	}
drained:
	if !sawDrop {
		t.Fatal("live saturation did not emit a bus.dropped notice")
	}
	// The notice path may displace one additional live event, leaving a new
	// zero-sequence drop window open. With the buffer drained, one more live
	// event lets that notice land without displacement and closes the window;
	// this isolates the following durable assertion from the subscriber's
	// backpressure bookkeeping.
	if err := publishLive(bus, context.Background(), liveWarning(id, 99)); err != nil {
		t.Fatalf("PublishLive(window close): %v", err)
	}
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.EventTypeBusDropped && ev.Sequence != 0 {
				t.Fatalf("live drop notice Sequence = %d, want 0", ev.Sequence)
			}
		default:
			goto windowClosed
		}
	}
windowClosed:
	if err := bus.Publish(context.Background(), liveWarning(id, 100)); err != nil {
		t.Fatalf("Publish durable after live burst: %v", err)
	}
	durable := drainN(t, sub, 1, time.Second)
	if len(durable) != 1 || durable[0].Sequence != 1 {
		t.Fatalf("durable event after live drop notice = %#v, want Sequence 1", durable)
	}
	rp := bus.(events.Replayer)
	replay, err := rp.Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replay) != 1 || replay[0].Sequence != 1 || replay[0].Type != events.EventTypeRuntimeWarning {
		t.Fatalf("replay after live drop notice = %#v, want only durable Sequence 1", replay)
	}
}

func TestPublishLive_MixedDurableBackpressureDoesNotSequenceNotice(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	cfg := defaultCfg()
	cfg.SubscriberBufferSize = 1
	cfg.DropWindow = 10 * time.Millisecond
	cfg.ReplayBufferSize = 8
	bus, err := inmem.New(cfg, auditpatterns.New(), inmem.WithClock(clock))
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(908)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := bus.Publish(context.Background(), liveWarning(id, 1)); err != nil {
		t.Fatalf("durable Publish(seq1): %v", err)
	}
	// Leave seq1 unread, then advance the injected clock past the drop
	// window. PublishLive must displace seq1 without turning its drop notice
	// into a durable sequence.
	clock.Advance(20 * time.Millisecond)
	if err := publishLive(bus, context.Background(), liveWarning(id, 2)); err != nil {
		t.Fatalf("PublishLive mixed saturation: %v", err)
	}
	var notices int
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type != events.EventTypeBusDropped {
				t.Fatalf("mixed saturation retained event = %+v, want transient drop notice", ev)
			}
			notices++
			if ev.Sequence != 0 {
				t.Fatalf("live-triggered drop notice Sequence = %d, want 0", ev.Sequence)
			}
		default:
			if notices != 1 {
				t.Fatalf("mixed saturation notices = %d, want 1", notices)
			}
			goto drained
		}
	}
drained:
	if err := bus.Publish(context.Background(), liveWarning(id, 3)); err != nil {
		t.Fatalf("durable Publish after live saturation: %v", err)
	}
	durable := drainN(t, sub, 1, time.Second)
	if len(durable) != 1 || durable[0].Sequence != 2 {
		t.Fatalf("durable event after mixed live saturation = %#v, want Sequence 2", durable)
	}
	replay, err := bus.(events.Replayer).Replay(context.Background(), events.Cursor{SessionID: id.SessionID}, liveFilter(id))
	if err != nil {
		t.Fatalf("Replay after mixed saturation: %v", err)
	}
	if len(replay) != 2 || replay[0].Sequence != 1 || replay[1].Sequence != 2 {
		t.Fatalf("replay after mixed saturation = %#v, want durable sequences 1,2", replay)
	}
}

func TestPublishLive_FenceCutsOffInFlightRedaction(t *testing.T) {
	redactor := &blockingRedactor{
		inner:   auditpatterns.New(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	bus, err := inmem.New(defaultCfg(), redactor)
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(907)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
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

func TestPublishLive_CancelledAfterFenceUnfenceCannotReachReusedSession(t *testing.T) {
	redactor := &blockingRedactor{
		inner:        auditpatterns.New(),
		started:      make(chan struct{}),
		release:      make(chan struct{}),
		ignoreCancel: true,
	}
	bus, err := inmem.New(defaultCfg(), redactor)
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	id := mkID(909)
	sub, err := bus.Subscribe(context.Background(), liveFilter(id))
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

	if err := publishLive(bus, context.Background(), liveWarning(id, 10)); err != nil {
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
