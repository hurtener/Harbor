package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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

	if err := bus.PublishLive(context.Background(), liveWarning(id, 1)); err != nil {
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
		if err := bus.PublishLive(context.Background(), ev); err != nil {
			t.Fatalf("PublishLive(%s): %v", ev.Type, err)
		}
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("filtered live event was delivered: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}

	if err := bus.PublishLive(context.Background(), liveWarning(target, 3)); err != nil {
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
			if err := bus.PublishLive(context.Background(), liveWarning(ids[i], uint64(i))); err != nil {
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
	if err := bus.PublishLive(context.Background(), bad); !errors.Is(err, events.ErrSequenceProvided) {
		t.Fatalf("PublishLive(sequence=99) = %v, want ErrSequenceProvided", err)
	}
	missing := liveWarning(id, 2)
	missing.Identity.SessionID = ""
	if err := bus.PublishLive(context.Background(), missing); !errors.Is(err, events.ErrIdentityRequired) {
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
	if err := bus.PublishLive(ctx, liveWarning(id, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishLive(cancelled) = %v, want context.Canceled", err)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("cancelled live event reached subscriber: %+v", ev)
	case <-time.After(25 * time.Millisecond):
	}
}
