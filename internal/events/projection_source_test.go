package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// ---------------------------------------------------------------------------
// ProjectionWakeHub — the bounded best-effort watermark notification
// registry the drivers embed
// ---------------------------------------------------------------------------

func TestProjectionWakeHub_NotifiesRegisteredSinks(t *testing.T) {
	var hub events.ProjectionWakeHub
	sinkA := make(chan uint64, 4)
	sinkB := make(chan uint64, 4)
	unsubA := hub.Register(sinkA)
	unsubB := hub.Register(sinkB)
	defer unsubA()
	defer unsubB()

	hub.NotifyWatermark(7)

	for name, sink := range map[string]chan uint64{"A": sinkA, "B": sinkB} {
		select {
		case wm := <-sink:
			if wm != 7 {
				t.Errorf("sink %s got watermark %d, want 7", name, wm)
			}
		case <-time.After(time.Second):
			t.Errorf("sink %s was not notified", name)
		}
	}
}

func TestProjectionWakeHub_UnsubscribeStopsDelivery(t *testing.T) {
	var hub events.ProjectionWakeHub
	sink := make(chan uint64, 4)
	unsub := hub.Register(sink)
	unsub()
	// Idempotent.
	unsub()

	hub.NotifyWatermark(3)
	select {
	case wm := <-sink:
		t.Fatalf("unsubscribed sink received watermark %d", wm)
	case <-time.After(50 * time.Millisecond):
		// Expected: no delivery.
	}
}

func TestProjectionWakeHub_FullSinkDropsBestEffort(t *testing.T) {
	var hub events.ProjectionWakeHub
	sink := make(chan uint64, 1) // bounded: one slot
	unsub := hub.Register(sink)
	defer unsub()

	// First notify fills the sink; the second is dropped (non-blocking).
	hub.NotifyWatermark(1)
	hub.NotifyWatermark(2)

	select {
	case wm := <-sink:
		if wm != 1 {
			t.Errorf("sink holds watermark %d, want 1 (the second was dropped)", wm)
		}
	case <-time.After(time.Second):
		t.Fatal("sink never received the first watermark")
	}
	// Nothing else queued — the second notification was dropped.
	select {
	case wm := <-sink:
		t.Fatalf("expected the second notification to be dropped, got watermark %d", wm)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestProjectionWakeHub_NotifyWithNoSinksIsNoOp(t *testing.T) {
	var hub events.ProjectionWakeHub
	hub.NotifyWatermark(9) // must not panic, must not block
}

// TestProjectionWakeHub_ConcurrentRegisterNotifyUnsubscribe runs the hub
// under the race detector with concurrent registrations, notifications,
// and unsubscriptions, asserting no goroutine is blocked on the hub and
// a slow (never-draining) sink cannot stall the notifiers.
func TestProjectionWakeHub_ConcurrentRegisterNotifyUnsubscribe(t *testing.T) {
	var hub events.ProjectionWakeHub
	const notifiers = 8
	const perNotifier = 200

	var notifyWG sync.WaitGroup
	for range notifiers {
		notifyWG.Add(1)
		go func() {
			defer notifyWG.Done()
			for i := 1; i <= perNotifier; i++ {
				hub.NotifyWatermark(uint64(i))
			}
		}()
	}
	notifyDone := make(chan struct{})
	go func() {
		notifyWG.Wait()
		close(notifyDone)
	}()

	// Churn of tiny (cap-1) sinks registered for the whole notify storm
	// and never drained: the notifiers' sends into them must drop
	// non-blocking — a slow projector can never block or fail the
	// publish path.
	var churnWG sync.WaitGroup
	for range 8 {
		churnWG.Add(1)
		go func() {
			defer churnWG.Done()
			sink := make(chan uint64, 1)
			unsub := hub.Register(sink)
			defer unsub()
			<-notifyDone
		}()
	}

	done := make(chan struct{})
	go func() {
		churnWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("hub concurrency test hung — a notification blocked a goroutine")
	}
}

// ---------------------------------------------------------------------------
// OpenProjectionSource — the capability discovery helper
// ---------------------------------------------------------------------------

// stubBus is a minimal events.EventBus that does NOT implement
// LivePublisher or ProjectionSource — used to prove both optional
// capabilities remain separate from the durable EventBus core.
type stubBus struct{}

func (stubBus) Publish(context.Context, events.Event) error { return nil }
func (stubBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, events.ErrIdentityScopeRequired
}
func (stubBus) Close(context.Context) error { return nil }

func TestOpenProjectionSource_NilBusFailsLoud(t *testing.T) {
	if _, err := events.OpenProjectionSource(nil); !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("OpenProjectionSource(nil) err=%v, want ErrProjectionUnavailable", err)
	}
}

func TestOpenProjectionSource_NonCapableBusFailsLoud(t *testing.T) {
	if _, err := events.OpenProjectionSource(stubBus{}); !errors.Is(err, events.ErrProjectionUnavailable) {
		t.Fatalf("OpenProjectionSource(stubBus) err=%v, want ErrProjectionUnavailable", err)
	}
}

func TestProjectionQuality_String(t *testing.T) {
	for want, q := range map[string]events.ProjectionQuality{
		"current":               events.ProjectionCurrent,
		"catching_up":           events.ProjectionCatchingUp,
		"unavailable":           events.ProjectionUnavailable,
		"ProjectionQuality(99)": events.ProjectionQuality(99),
	} {
		if got := q.String(); got != want {
			t.Errorf("ProjectionQuality(%d).String()=%q, want %q", int(q), got, want)
		}
	}
}
