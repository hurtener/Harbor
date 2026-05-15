package durable

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// subscription is the durable driver's per-subscriber state. It is a
// lean port of the inmem driver's subscription: a bounded channel,
// drop-oldest on saturation, and a windowed bus.dropped notice. The
// durable driver has no idle reaper goroutine — durability is the
// driver's job; idle-subscriber reaping is the inmem driver's
// concern. Subscribers that stop draining simply accumulate drops.
type subscription struct {
	id     uint64
	filter events.Filter
	bound  identity.Quadruple
	ch     chan events.Event

	mu           sync.Mutex // serialises enqueue + cancel against each other
	dropOpen     bool
	dropFromSeq  uint64
	dropToSeq    uint64
	dropCount    uint64
	lastDropEmit atomic.Int64 // unix nano of last bus.dropped emit
	cancelled    atomic.Bool
	cancelOnce   sync.Once
}

// Events implements events.Subscription. Returns s.ch directly so
// buffered events remain readable after Cancel — a closed Go channel
// still surfaces its buffered values.
func (s *subscription) Events() <-chan events.Event { return s.ch }

// Cancel implements events.Subscription. Idempotent and safe from any
// goroutine.
func (s *subscription) Cancel() { s.cancel() }

// cancel closes s.ch under s.mu so the close is serialised against any
// in-flight enqueue (which also holds s.mu). Without the lock, Close
// racing an active Publish triggers "send on closed channel".
func (s *subscription) cancel() {
	s.mu.Lock()
	if !s.cancelled.Load() {
		s.cancelled.Store(true)
		s.cancelOnce.Do(func() { close(s.ch) })
	}
	s.mu.Unlock()
}

// enqueue delivers ev to the subscriber. Drops the oldest event under
// saturation, accounts the drop, and emits a windowed bus.dropped
// sibling event into the subscriber's own stream.
func (s *subscription) enqueue(ev events.Event, b *bus) {
	if s.cancelled.Load() {
		return
	}
	now := b.clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled.Load() {
		return
	}

	select {
	case s.ch <- ev:
		s.maybeEmitDropNotice(ev.Identity, b, now)
		return
	default:
	}

	// Channel full — drop oldest, account, retry.
	select {
	case dropped := <-s.ch:
		s.recordDrop(dropped.Sequence, ev.Sequence)
	default:
		// Consumer drained between the two selects.
	}
	select {
	case s.ch <- ev:
		s.maybeEmitDropNotice(ev.Identity, b, now)
	default:
		s.recordDrop(ev.Sequence, ev.Sequence)
	}
}

// recordDrop accumulates a dropped sequence range into the open
// window.
func (s *subscription) recordDrop(fromSeq, toSeq uint64) {
	if !s.dropOpen {
		s.dropOpen = true
		s.dropFromSeq = fromSeq
		s.dropToSeq = toSeq
		s.dropCount = 1
		return
	}
	s.dropToSeq = toSeq
	s.dropCount++
}

// maybeEmitDropNotice emits a bus.dropped event into the subscriber's
// stream when a drop window is open and DropWindow has elapsed since
// the last emit. The notice carries the dropped sequence range so the
// consumer learns exactly what it missed.
func (s *subscription) maybeEmitDropNotice(forIdentity identity.Quadruple, b *bus, now time.Time) {
	if !s.dropOpen {
		return
	}
	last := s.lastDropEmit.Load()
	if now.UnixNano()-last < int64(b.cfg.DropWindow) {
		return
	}
	notice := events.Event{
		Type:       events.EventTypeBusDropped,
		Identity:   forIdentity,
		OccurredAt: now,
		Payload: events.BusDroppedPayload{
			FromSeq:      s.dropFromSeq,
			ToSeq:        s.dropToSeq,
			DroppedCount: s.dropCount,
			SubscriberID: s.id,
		},
	}
	// The bus.dropped notice is bus-internal bookkeeping: it is NOT
	// sequenced through the durable log (it carries no run state to
	// persist and a per-subscriber notice is not part of the session
	// history). Try to land it without displacing.
	select {
	case s.ch <- notice:
		s.resetDropWindow(now)
		return
	default:
	}
	// Channel full — displace one event so the notice can land; the
	// displaced event is folded into the NEXT window's tally.
	var displacedSeq uint64
	displaced := false
	select {
	case ev := <-s.ch:
		displacedSeq = ev.Sequence
		displaced = true
	default:
	}
	select {
	case s.ch <- notice:
		s.resetDropWindow(now)
	default:
	}
	if displaced {
		s.recordDrop(displacedSeq, displacedSeq)
	}
}

func (s *subscription) resetDropWindow(now time.Time) {
	s.dropOpen = false
	s.dropFromSeq = 0
	s.dropToSeq = 0
	s.dropCount = 0
	s.lastDropEmit.Store(now.UnixNano())
}

// Compile-time assertion that subscription satisfies the interface.
var _ events.Subscription = (*subscription)(nil)
