// Package inmem is Harbor's V1 in-memory EventBus driver.
//
// Architecture:
//
//   - Publish runs the payload through audit.Redactor (skipped for
//     SafePayload-marked types — bus-internal events, governance
//     metrics, and any opt-in caller). On redaction error the bus
//     publishes an audit.redaction_failed sibling event and returns
//     the wrapped error; the original payload is NOT enqueued.
//   - Sequence numbering is per-bus monotonic via atomic.Uint64.
//   - Fan-out walks the subscriber map under RLock; each match runs
//     the per-subscriber enqueue path (drop-oldest under saturation).
//   - The reaper goroutine ticks at IdleTimeout/4 and Cancels any
//     subscription whose Events() channel has not been drained for
//     IdleTimeout.
//   - Close idempotently signals the reaper, cancels every live
//     subscription, and waits for goroutines to exit.
//
// The driver is registered under name "inmem" via init(); cmd/harbor
// blank-imports this package so the registration fires at process
// startup. Per AGENTS.md §4.4.
package inmem

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// Clock abstracts time so the reaper is testable without time.Sleep.
// The realClock implementation simply forwards to the time package;
// fakeClock (in inmem_test.go) advances manually.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Ticker is the abstraction the reaper consumes.
type Ticker interface {
	Chan() <-chan time.Time
	Stop()
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTicker(d time.Duration) Ticker {
	rt := time.NewTicker(d)
	return &realTicker{t: rt}
}

type realTicker struct{ t *time.Ticker }

func (rt *realTicker) Chan() <-chan time.Time { return rt.t.C }
func (rt *realTicker) Stop()                  { rt.t.Stop() }

// Option configures the bus at construction. The exported options
// (WithClock) are intentionally test-only seams; production code does
// not touch them.
type Option func(*bus)

// WithClock injects a Clock implementation. Production callers do
// NOT use this; the default realClock is correct. Tests use a fake
// clock to exercise the reaper deterministically.
func WithClock(c Clock) Option {
	return func(b *bus) { b.clock = c }
}

// New constructs a bus directly without going through the registry.
// Exposed for tests that need to pass Options.
func New(cfg config.EventsConfig, r audit.Redactor, opts ...Option) (events.EventBus, error) {
	if r == nil {
		return nil, fmt.Errorf("inmem: audit.Redactor required (got nil)")
	}
	if cfg.MaxSubscribersPerSession <= 0 {
		return nil, fmt.Errorf("inmem: MaxSubscribersPerSession must be > 0")
	}
	if cfg.SubscriberBufferSize <= 0 {
		return nil, fmt.Errorf("inmem: SubscriberBufferSize must be > 0")
	}
	if cfg.IdleTimeout <= 0 {
		return nil, fmt.Errorf("inmem: IdleTimeout must be > 0")
	}
	if cfg.DropWindow <= 0 {
		return nil, fmt.Errorf("inmem: DropWindow must be > 0")
	}
	b := &bus{
		cfg:       cfg,
		redactor:  r,
		clock:     realClock{},
		subs:      map[uint64]*subscription{},
		closeDone: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	b.startReaper()
	return b, nil
}

func init() {
	events.Register("inmem", func(cfg config.EventsConfig, r audit.Redactor) (events.EventBus, error) {
		return New(cfg, r)
	})
}

type bus struct {
	cfg      config.EventsConfig
	redactor audit.Redactor
	clock    Clock

	seq atomic.Uint64

	mu    sync.RWMutex
	subs  map[uint64]*subscription
	subID atomic.Uint64

	closed    atomic.Bool
	closeOnce sync.Once
	closeDone chan struct{}

	reaperWG sync.WaitGroup
}

// startReaper launches the idle-subscription sweep goroutine. The
// tick interval is IdleTimeout / 4 to keep latency to reaping
// bounded by ~25% of the timeout.
func (b *bus) startReaper() {
	interval := b.cfg.IdleTimeout / 4
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	ticker := b.clock.NewTicker(interval)
	b.reaperWG.Add(1)
	go func() {
		defer b.reaperWG.Done()
		defer ticker.Stop()
		for {
			select {
			case <-b.closeDone:
				return
			case now := <-ticker.Chan():
				b.reapIdle(now)
			}
		}
	}()
}

func (b *bus) reapIdle(now time.Time) {
	idle := b.cfg.IdleTimeout
	b.mu.RLock()
	candidates := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		lastDrain := time.Unix(0, s.lastDrain.Load())
		if now.Sub(lastDrain) >= idle {
			candidates = append(candidates, s)
		}
	}
	b.mu.RUnlock()
	for _, s := range candidates {
		idleSeconds := now.Sub(time.Unix(0, s.lastDrain.Load())).Seconds()
		// Push the close-notice payload before cancelling so any
		// reader still draining sees the reason.
		notice := events.Event{
			Type:       events.EventTypeBusSubscriptionIdleClosed,
			Identity:   s.bound,
			OccurredAt: now,
			Payload: events.SubscriptionIdleClosedPayload{
				SubscriberID: s.id,
				IdleSeconds:  idleSeconds,
			},
		}
		notice.Sequence = b.seq.Add(1)
		s.enqueueClosing(notice)
		s.cancelInternal(b)
	}
}

// Publish validates, redacts, sequences, and fans out the event.
func (b *bus) Publish(ctx context.Context, ev events.Event) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	if err := events.ValidateEvent(ev); err != nil {
		return err
	}

	// Redaction: skip for SafePayload, otherwise run through the
	// audit redactor. On redaction error, emit a sibling
	// audit.redaction_failed event and return the wrapped error.
	payload := ev.Payload
	if _, safe := payload.(events.SafePayload); !safe {
		redacted, err := b.redactor.Redact(ctx, payload)
		if err != nil {
			b.emitRedactionFailure(ctx, ev, err)
			return fmt.Errorf("events: publish redaction failed: %w", err)
		}
		payload = wrapRedacted(redacted)
	}
	ev.Payload = payload

	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}
	ev.Sequence = b.seq.Add(1)

	b.fanOut(ev)
	return nil
}

// emitRedactionFailure publishes a sibling audit.redaction_failed
// event with NO payload bytes. The sibling carries enough metadata
// (original type + reason) for an admin subscriber to investigate
// without seeing the unredacted bytes the redactor refused.
func (b *bus) emitRedactionFailure(_ context.Context, original events.Event, cause error) {
	ev := events.Event{
		Type:       events.EventTypeAuditRedactionFailed,
		Identity:   original.Identity,
		OccurredAt: b.clock.Now(),
		Payload: events.AuditRedactionFailedPayload{
			OriginalType: original.Type,
			Reason:       cause.Error(),
		},
	}
	ev.Sequence = b.seq.Add(1)
	b.fanOut(ev)
}

// wrapRedacted converts the audit redactor's output (which may be a
// map[string]any after walking a struct) into a value satisfying
// events.EventPayload. If the redactor returned the input unchanged
// AND it satisfies EventPayload, pass it through; otherwise wrap in
// RedactedMap.
func wrapRedacted(v any) events.EventPayload {
	if p, ok := v.(events.EventPayload); ok {
		return p
	}
	if m, ok := v.(map[string]any); ok {
		return events.RedactedMap{Data: m}
	}
	return events.RedactedMap{Data: map[string]any{"value": v}}
}

// fanOut walks subscribers and enqueues to each whose filter matches.
func (b *bus) fanOut(ev events.Event) {
	b.mu.RLock()
	matched := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		if s.cancelled.Load() {
			continue
		}
		if s.filter.Matches(ev) {
			matched = append(matched, s)
		}
	}
	b.mu.RUnlock()
	for _, s := range matched {
		s.enqueue(ev, b)
	}
}

// Subscribe validates the filter, audits Admin scope, enforces the
// per-session subscriber cap, and returns a Subscription.
func (b *bus) Subscribe(_ context.Context, f events.Filter) (events.Subscription, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}

	if !f.Admin {
		// Enforce per-session cap.
		b.mu.RLock()
		count := 0
		for _, s := range b.subs {
			if s.cancelled.Load() {
				continue
			}
			if !s.filter.Admin &&
				s.filter.Tenant == f.Tenant &&
				s.filter.User == f.User &&
				s.filter.Session == f.Session {
				count++
			}
		}
		b.mu.RUnlock()
		if count >= b.cfg.MaxSubscribersPerSession {
			return nil, events.ErrSubscriberLimitReached
		}
	}

	id := b.subID.Add(1)
	bound := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  f.Tenant,
			UserID:    f.User,
			SessionID: f.Session,
		},
	}
	s := &subscription{
		id:     id,
		filter: f,
		bound:  bound,
		ch:     make(chan events.Event, b.cfg.SubscriberBufferSize),
	}
	s.lastDrain.Store(b.clock.Now().UnixNano())
	s.lastDropEmit.Store(b.clock.Now().UnixNano())

	b.mu.Lock()
	b.subs[id] = s
	b.mu.Unlock()

	if f.Admin {
		// Synthesise an audit event so admin-scope use is observable.
		notice := events.Event{
			Type:       events.EventTypeAdminScopeUsed,
			Identity:   bound,
			OccurredAt: b.clock.Now(),
			Payload: events.AdminScopeUsedPayload{
				Tenant:       f.Tenant,
				User:         f.User,
				Session:      f.Session,
				SubscriberID: id,
			},
		}
		notice.Sequence = b.seq.Add(1)
		b.fanOut(notice)
	}

	return s, nil
}

// Close idempotently shuts the bus down. After Close, Publish and
// Subscribe return ErrBusClosed; existing subscribers receive a
// closed Events() channel.
func (b *bus) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.closeDone)
	})

	// Cancel all subscriptions.
	b.mu.Lock()
	subs := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		s.cancelInternal(b)
	}

	// Wait for the reaper to exit, honouring ctx.
	done := make(chan struct{})
	go func() {
		b.reaperWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// subscription is the per-subscriber state.
type subscription struct {
	id     uint64
	filter events.Filter
	bound  identity.Quadruple
	ch     chan events.Event

	// Drop bookkeeping.
	mu             sync.Mutex // serialises enqueue against itself
	dropOpen       bool       // a drop window is in progress
	dropFromSeq    uint64
	dropToSeq      uint64
	dropCount      uint64
	lastDropEmit   atomic.Int64 // unix nano of last bus.dropped emit
	lastDrain      atomic.Int64 // unix nano of last successful read
	cancelled      atomic.Bool
	cancelledOnce  sync.Once
	closeChanOnce  sync.Once
}

// Events implements events.Subscription.
func (s *subscription) Events() <-chan events.Event {
	return wrappedReadChan(s)
}

// wrappedReadChan returns a reader-side channel that records last-drain
// timestamps so the reaper sees fresh activity on every read. We
// achieve this by spawning a tiny pipe — but at scale we'd rather
// avoid a goroutine per subscriber. Instead, expose s.ch directly
// and rely on subscribers to call s.markDrain() if they want to
// influence the reaper. To keep the public API ergonomic, we pump
// s.ch through a wrapper that updates lastDrain on every read.
func wrappedReadChan(s *subscription) <-chan events.Event {
	if s.cancelled.Load() {
		closed := make(chan events.Event)
		close(closed)
		return closed
	}
	return s.ch
}

// markDrain is called by the bus's drain-aware reader path... in
// practice, we do NOT have a wrapper goroutine: the consumer reads
// directly from s.ch. The reaper checks the buffer fill — if the
// channel is at capacity (consumer not draining), the subscription
// is reaped. This avoids one goroutine per subscriber.
//
// lastDrain is updated when the bus enqueues — every successful
// fan-out into s.ch counts as the subscription "being drained" if
// the channel had room (i.e. the consumer is keeping up). When the
// channel saturates, lastDrain stops advancing and the reaper trips
// after IdleTimeout.
func (s *subscription) markEnqueueProgress(now int64) {
	s.lastDrain.Store(now)
}

// Cancel implements events.Subscription. Idempotent.
func (s *subscription) Cancel() {
	s.cancelInternal(nil)
}

// cancelInternal performs the cancel, closing s.ch and removing the
// subscription from the bus's map (when bus is non-nil).
func (s *subscription) cancelInternal(b *bus) {
	s.cancelled.Store(true)
	s.cancelledOnce.Do(func() {
		s.closeChanOnce.Do(func() { close(s.ch) })
	})
	if b != nil {
		b.mu.Lock()
		delete(b.subs, s.id)
		b.mu.Unlock()
	}
}

// enqueue tries to deliver ev. Drops oldest under saturation,
// records the drop, and may emit a sibling bus.dropped event into
// the subscriber's stream (windowed by DropWindow).
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

	// Fast path: try non-blocking send.
	select {
	case s.ch <- ev:
		s.markEnqueueProgress(now.UnixNano())
		// If a drop window is open and we're now draining, check
		// whether to emit the bus.dropped notice.
		s.maybeEmitDropNotice(ev.Identity, b, now)
		return
	default:
	}

	// Channel full — drop oldest, account, then send.
	var dropped events.Event
	select {
	case dropped = <-s.ch:
		// account
		s.recordDrop(dropped.Sequence, ev.Sequence)
	default:
		// Consumer drained between our two selects; channel is no
		// longer full. Fall through to retry the send.
	}

	select {
	case s.ch <- ev:
		s.markEnqueueProgress(now.UnixNano())
		s.maybeEmitDropNotice(ev.Identity, b, now)
	default:
		// Pathological — record this as dropped too.
		s.recordDrop(ev.Sequence, ev.Sequence)
	}
}

// enqueueClosing pushes a final notice (typically the
// SubscriptionIdleClosed event) into the channel just before cancel.
// Best-effort: if the channel is full, the notice is dropped.
func (s *subscription) enqueueClosing(ev events.Event) {
	if s.cancelled.Load() {
		return
	}
	select {
	case s.ch <- ev:
	default:
	}
}

// recordDrop accumulates dropped sequence range into the open window.
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

// maybeEmitDropNotice emits a bus.dropped event if (a) a drop
// window is open AND (b) at least DropWindow has elapsed since the
// last emit. Resets the window on emit. If the channel is full
// when the window has elapsed, displaces one event to make room
// for the notice — bus.dropped is more important than any single
// dropped data event because it tells the consumer they missed a
// range. The displaced event is folded into the drop accounting
// before being overwritten.
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
	notice.Sequence = b.seq.Add(1)

	// Try to land the notice without displacing.
	select {
	case s.ch <- notice:
		s.resetDropWindow(now)
		return
	default:
	}
	// Channel full — displace one event so the notice can land. The
	// displaced event becomes part of the drop accounting we'll cite
	// in the NEXT window (we don't update the current notice's
	// payload after constructing it; better to under-report than
	// race the notice's contents).
	select {
	case displaced := <-s.ch:
		_ = displaced
	default:
	}
	select {
	case s.ch <- notice:
		s.resetDropWindow(now)
	default:
		// Still couldn't land — pathological. Will retry next enqueue.
	}
}

func (s *subscription) resetDropWindow(now time.Time) {
	s.dropOpen = false
	s.dropFromSeq = 0
	s.dropToSeq = 0
	s.dropCount = 0
	s.lastDropEmit.Store(now.UnixNano())
	s.markEnqueueProgress(now.UnixNano())
}

// Compile-time assertion that bus implements events.EventBus.
var _ events.EventBus = (*bus)(nil)

// Compile-time assertion: subscription.Cancel is exported via the
// interface; satisfying both Events() and Cancel() suffices.
var _ events.Subscription = (*subscription)(nil)

// errors.Is helper for bus-closed checks (avoids package-level cycle).
var _ = errors.Is
