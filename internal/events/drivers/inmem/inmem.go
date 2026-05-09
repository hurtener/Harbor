// Package inmem is Harbor's V1 in-memory EventBus driver.
//
// Architecture:
//
//   - Publish runs the payload through audit.Redactor (skipped for
//     SafePayload-marked types — bus-internal events, governance
//     metrics, and any opt-in caller). On redaction error the bus
//     publishes an audit.redaction_failed sibling event and returns
//     the wrapped error; the original payload is NOT enqueued.
//   - Sequence numbering is per-bus monotonic and gap-free. Sequence
//     assignment and ring-buffer append happen under one mutex
//     (publishMu) so the ring's contents are guaranteed to be in
//     Sequence order — the load-bearing invariant for Replay's
//     "no gaps within a RunID" guarantee.
//   - Fan-out walks the subscriber map under RLock; each match runs
//     the per-subscriber enqueue path (drop-oldest under saturation).
//   - The reaper goroutine ticks at IdleTimeout/4 and Cancels any
//     subscription whose Events() channel has not been drained for
//     IdleTimeout.
//   - Replay (Phase 06) snapshots the ring under publishMu, applies
//     the same filter rules as Subscribe, returns events strictly
//     newer than the cursor in Sequence order. ReplayBufferSize=0
//     disables the ring entirely; Replay returns
//     events.ErrReplayUnavailable.
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
	if cfg.ReplayBufferSize < 0 {
		return nil, fmt.Errorf("inmem: ReplayBufferSize must be >= 0 (zero disables replay)")
	}
	b := &bus{
		cfg:       cfg,
		redactor:  r,
		clock:     realClock{},
		ringCap:   cfg.ReplayBufferSize,
		subs:      map[uint64]*subscription{},
		closeDone: make(chan struct{}),
	}
	if b.ringCap > 0 {
		b.ringBuf = make([]events.Event, b.ringCap)
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

	// publishMu protects sequence assignment + ring-buffer append. It
	// is the load-bearing invariant for Replay: by serialising the two
	// operations, the ring's contents are guaranteed in Sequence
	// order (because seq is incremented and the event is written to
	// the ring under the same lock acquisition). Two concurrent
	// publishers may interleave on fanOut afterwards — that's fine,
	// subscribers' arrival order is best-effort — but the ring is
	// authoritative for Replay's gap-free guarantee.
	publishMu sync.Mutex
	nextSeq   uint64
	// Ring buffer state. ringBuf[ringHead] is the slot the next event
	// will be written to. ringFull is true once the buffer has wrapped
	// at least once. ringCap is the configured capacity (snapshot of
	// cfg.ReplayBufferSize so a future hot-reload can be staged
	// without re-allocating live).
	ringBuf  []events.Event
	ringHead int
	ringFull bool
	ringCap  int

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
		// Reap ONLY when (a) we haven't seen a clean enqueue (one
		// that fit without displacing) for at least IdleTimeout AND
		// (b) the consumer's channel currently holds queued events.
		// Condition (b) is the load-bearing one — a quiet bus with
		// an empty channel means the subscriber is just waiting; an
		// idle consumer is a non-empty channel that isn't draining.
		lastDrain := time.Unix(0, s.lastDrain.Load())
		if now.Sub(lastDrain) < idle {
			continue
		}
		if len(s.ch) == 0 {
			continue
		}
		candidates = append(candidates, s)
	}
	b.mu.RUnlock()
	for _, s := range candidates {
		idleSeconds := now.Sub(time.Unix(0, s.lastDrain.Load())).Seconds()
		notice := events.Event{
			Type:       events.EventTypeBusSubscriptionIdleClosed,
			Identity:   s.bound,
			OccurredAt: now,
			Payload: events.SubscriptionIdleClosedPayload{
				SubscriberID: s.id,
				IdleSeconds:  idleSeconds,
			},
		}
		b.assignSeqAndStore(&notice)
		// enqueueClosing + close-channel must run under the SAME
		// s.mu lock so the closing notice's send and the channel
		// close don't race (concurrent publishers are also under
		// s.mu in enqueue). cancelInternal does both atomically.
		s.cancelInternalWithNotice(b, &notice)
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
	b.assignSeqAndStore(&ev)

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
	b.assignSeqAndStore(&ev)
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
		b.assignSeqAndStore(&notice)
		b.fanOut(notice)
	}

	return s, nil
}

// assignSeqAndStore is the load-bearing helper that assigns the next
// monotonic sequence to ev AND appends a copy to the ring buffer
// under one lock acquisition. The ring is therefore guaranteed to
// contain events in strict Sequence order — no two ring slots will
// ever hold events whose Sequence ordering disagrees with their slot
// ordering.
//
// Caller must NOT pre-fill ev.Sequence (Publish enforces this; the
// internal callers — emitRedactionFailure, the reaper closing
// notice, the Subscribe AdminScopeUsed notice, the maybeEmitDropNotice
// path — all construct fresh Events with Sequence=0 by convention).
//
// When ringCap is 0, only the sequence is assigned; no ring storage
// happens (replay is configured-off).
func (b *bus) assignSeqAndStore(ev *events.Event) {
	b.publishMu.Lock()
	b.nextSeq++
	ev.Sequence = b.nextSeq
	if b.ringCap > 0 {
		b.ringAppendLocked(*ev)
	}
	b.publishMu.Unlock()
}

// ringAppendLocked writes ev to the next ring slot and advances the
// head. Caller must hold publishMu. Called only when ringCap > 0.
func (b *bus) ringAppendLocked(ev events.Event) {
	b.ringBuf[b.ringHead] = ev
	b.ringHead++
	if b.ringHead >= b.ringCap {
		b.ringHead = 0
		b.ringFull = true
	}
}

// ringSnapshotLocked returns the events currently retained in the
// ring, in Sequence order (oldest first). Caller must hold publishMu.
//
// When the ring has not wrapped (ringFull=false), the live entries
// are buf[0:ringHead]. When it has wrapped, the live entries are
// buf[ringHead:cap] followed by buf[0:ringHead] — concatenation
// preserves Sequence order because the appender writes monotonically.
//
// The returned slice is a fresh copy; the caller owns it.
func (b *bus) ringSnapshotLocked() []events.Event {
	if b.ringCap == 0 {
		return nil
	}
	if !b.ringFull {
		out := make([]events.Event, b.ringHead)
		copy(out, b.ringBuf[:b.ringHead])
		return out
	}
	out := make([]events.Event, b.ringCap)
	copy(out, b.ringBuf[b.ringHead:])
	copy(out[b.ringCap-b.ringHead:], b.ringBuf[:b.ringHead])
	return out
}

// Replay implements events.Replayer. Returns events strictly newer
// than from.Sequence that match f, in Sequence order. See the
// Replayer godoc for the semantics; the call also enforces the same
// filter rules as Subscribe (empty-triple non-admin filters are
// rejected; Admin emits an audit.admin_scope_used sibling event).
//
// Concurrency: snapshotting the ring under publishMu prevents torn
// reads against an in-progress Publish. Filtering and copying happen
// outside the lock so a long Replay does not stall publishers.
func (b *bus) Replay(_ context.Context, from events.Cursor, f events.Filter) ([]events.Event, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return nil, events.ErrReplayUnavailable
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}

	if f.Admin {
		// Mirror Subscribe: surface admin-scope use on the bus so
		// abuse is retroactively detectable. Phase 61 (Protocol auth)
		// will add cryptographic verification; Phase 06 trusts the
		// boolean exactly as Phase 05's Subscribe does.
		notice := events.Event{
			Type:       events.EventTypeAdminScopeUsed,
			Identity:   identity.Quadruple{Identity: identity.Identity{TenantID: f.Tenant, UserID: f.User, SessionID: f.Session}},
			OccurredAt: b.clock.Now(),
			Payload: events.AdminScopeUsedPayload{
				Tenant:  f.Tenant,
				User:    f.User,
				Session: f.Session,
			},
		}
		b.assignSeqAndStore(&notice)
		b.fanOut(notice)
	}

	// Snapshot the ring + record the head sequence under one lock
	// acquisition so the head boundary is consistent with the
	// snapshot's contents.
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	headSeq := b.nextSeq
	b.publishMu.Unlock()

	if len(snapshot) == 0 {
		return nil, nil
	}

	oldestSeq := snapshot[0].Sequence

	// Cursor at or past the head — nothing newer.
	if from.Sequence >= headSeq {
		return nil, nil
	}

	// Cursor older than the ring tail. The next event the caller
	// needs is from.Sequence+1; if that is older than oldestSeq,
	// events have been evicted and we cannot serve a gap-free
	// snapshot. The cursor=0 ("from beginning") case bypasses this
	// check by definition — it means "give me whatever the ring
	// retains" and accepts ring eviction implicitly.
	if from.Sequence > 0 && from.Sequence+1 < oldestSeq {
		return nil, fmt.Errorf("%w: oldest=%d requested=%d",
			events.ErrCursorTooOld, oldestSeq, from.Sequence)
	}

	// Filter and slice. Snapshot is already in Sequence order
	// (assignSeqAndStore guarantees this), so iterating preserves
	// the strictly-increasing-Sequence requirement.
	out := make([]events.Event, 0, len(snapshot))
	for _, ev := range snapshot {
		if ev.Sequence <= from.Sequence {
			continue
		}
		if !f.Matches(ev) {
			continue
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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

// Events implements events.Subscription. Returns s.ch directly so
// any buffered events (including a closing notice the reaper added)
// remain readable after cancel — closed Go channels still surface
// their buffered values before the receive returns ok=false.
//
// An earlier version of this method returned a freshly-closed
// stand-in channel when s.cancelled was true; that broke the
// reaper-emit contract because the buffered SubscriptionIdleClosed
// notice (and any saturating events the consumer was supposed to
// receive) became unreachable.
func (s *subscription) Events() <-chan events.Event {
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
//
// Lock order: s.mu before b.mu. Taking s.mu before closing s.ch
// serialises the close against any in-flight enqueue (which holds
// s.mu while doing the non-blocking sends). Without this, Close
// racing with an active Publish triggered "send on closed channel"
// under -race; pinned by TestBus_CloseDuringActivePublish.
func (s *subscription) cancelInternal(b *bus) {
	s.cancelInternalWithNotice(b, nil)
}

// cancelInternalWithNotice is the lock-coordinated cancel used by
// the reaper: under s.mu it (a) optionally enqueues a closing
// notice (displacing one event if the buffer is full — consumers
// would rather see the close reason than one more saturating
// event) AND (b) closes s.ch. Combining both under one acquisition
// avoids the race between the notice's send and the close in
// cancelInternal.
func (s *subscription) cancelInternalWithNotice(b *bus, notice *events.Event) {
	s.mu.Lock()
	if !s.cancelled.Load() {
		if notice != nil {
			select {
			case s.ch <- *notice:
			default:
				// Buffer full — displace one to make room for the
				// closing notice. The closing reason is more
				// useful than one more saturating event the
				// consumer wasn't going to read anyway.
				select {
				case <-s.ch:
				default:
				}
				select {
				case s.ch <- *notice:
				default:
				}
			}
		}
		s.cancelled.Store(true)
		s.cancelledOnce.Do(func() {
			s.closeChanOnce.Do(func() { close(s.ch) })
		})
	}
	s.mu.Unlock()
	if b != nil {
		b.mu.Lock()
		delete(b.subs, s.id)
		b.mu.Unlock()
	}
}

// enqueue tries to deliver ev. Drops oldest under saturation,
// records the drop, and may emit a sibling bus.dropped event into
// the subscriber's stream (windowed by DropWindow).
//
// lastDrain advances ONLY on the fast-path send (channel had room
// without displacement). The reaper uses lastDrain + non-empty
// buffer as the saturation signal — a saturated buffer where the
// only way the bus could enqueue was via displacement is exactly
// the "consumer not keeping up" condition.
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

	// Fast path: try non-blocking send. Only this path advances
	// lastDrain — it's the signal that the buffer had room.
	select {
	case s.ch <- ev:
		s.markEnqueueProgress(now.UnixNano())
		s.maybeEmitDropNotice(ev.Identity, b, now)
		return
	default:
	}

	// Channel full — drop oldest, account, then send. lastDrain
	// stays where it was; the reaper's "buffer non-empty + lastDrain
	// stale" condition fires after IdleTimeout in this state.
	var dropped events.Event
	select {
	case dropped = <-s.ch:
		s.recordDrop(dropped.Sequence, ev.Sequence)
	default:
		// Consumer drained between our two selects; channel is no
		// longer full. Fall through to retry the send.
	}

	select {
	case s.ch <- ev:
		s.maybeEmitDropNotice(ev.Identity, b, now)
	default:
		// Pathological — record this as dropped too.
		s.recordDrop(ev.Sequence, ev.Sequence)
	}
}

// (enqueueClosing was inlined into cancelInternalWithNotice — the
// closing notice and the channel close MUST happen under the same
// s.mu acquisition or they race against concurrent publishers.)

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
	b.assignSeqAndStore(&notice)

	// Try to land the notice without displacing.
	select {
	case s.ch <- notice:
		s.resetDropWindow(now)
		return
	default:
	}
	// Channel full — displace one event so the notice can land. The
	// displaced event becomes part of the NEXT window's drop tally
	// (we book it via recordDrop AFTER resetting the current window
	// because the just-emitted notice already covers events up to
	// dropToSeq — the displaced one is news for the next window).
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
		if displaced {
			s.recordDrop(displacedSeq, displacedSeq)
		}
	default:
		// Still couldn't land — pathological. Will retry next enqueue.
		if displaced {
			s.recordDrop(displacedSeq, displacedSeq)
		}
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

// Compile-time assertion that bus implements events.EventBus AND
// events.Replayer (Phase 06).
var (
	_ events.EventBus = (*bus)(nil)
	_ events.Replayer = (*bus)(nil)
)

// Compile-time assertion: subscription.Cancel is exported via the
// interface; satisfying both Events() and Cancel() suffices.
var _ events.Subscription = (*subscription)(nil)

// errors.Is helper for bus-closed checks (avoids package-level cycle).
var _ = errors.Is
