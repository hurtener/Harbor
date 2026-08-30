// Package inmem is Harbor's V1 in-memory EventBus driver.
//
// Architecture:
//
//   - Publish runs the payload through audit.Redactor (skipped for
//     SafePayload-marked types — bus-internal events, governance
//     metrics, and any opt-in caller). On redaction error the bus
//     publishes an audit.redaction_failed sibling event and returns
//     the wrapped error; the original payload is NOT enqueued.
//   - PublishLive runs the same validation, audit-boundary policy, identity
//     filtering, and bounded fan-out for present-tense animation, but never
//     assigns a sequence or touches the replay ring / projection watermark.
//     Non-SafePayload values are redacted; SafePayload values retain their
//     existing redactor bypass. Live events carry Sequence == 0 and are
//     intentionally lossy across reconnects.
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
//   - Replay snapshots the ring under publishMu, applies
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
	"log/slog"
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

// WithLogger injects the logger used for rate-limited asynchronous admission
// warnings. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(b *bus) {
		if l != nil {
			b.logger = l
		}
	}
}

// WithAsyncQueueSize sets the bounded admission capacity for best-effort
// asynchronous publications. It is intended for tests and operators tuning
// backpressure; the default is events.DefaultAsyncQueueSize.
func WithAsyncQueueSize(size int) Option {
	return func(b *bus) { b.asyncQueueSize = size }
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
		cfg:            cfg,
		redactor:       r,
		clock:          realClock{},
		logger:         slog.Default(),
		ringCap:        cfg.ReplayBufferSize,
		subs:           map[uint64]*subscription{},
		subsByIdentity: map[identity.Identity]map[uint64]*subscription{},
		adminSubs:      map[uint64]*subscription{},
		closeDone:      make(chan struct{}),
		asyncQueueSize: events.DefaultAsyncQueueSize,
	}
	if b.ringCap > 0 {
		b.ringBuf = make([]events.Event, b.ringCap)
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.asyncQueueSize <= 0 {
		return nil, fmt.Errorf("inmem: async queue size must be > 0")
	}
	b.asyncSignal = events.NewAsyncAdmissionSignal(b.logger, events.DefaultAsyncAdmissionLogInterval)
	ordered, err := events.NewOrderedQueue(b.asyncQueueSize, events.DefaultPublishBatchSize, b.commitBatch, b.reportAsyncFailure)
	if err != nil {
		return nil, fmt.Errorf("inmem: ordered publication queue: %w", err)
	}
	b.ordered = ordered
	b.startReaper()
	return b, nil
}

func init() {
	events.Register("inmem", func(cfg config.EventsConfig, r audit.Redactor) (events.EventBus, error) {
		return New(cfg, r)
	})
}

type bus struct {
	cfg         config.EventsConfig
	redactor    audit.Redactor
	clock       Clock
	logger      *slog.Logger
	asyncSignal *events.AsyncAdmissionSignal

	ordered *events.OrderedQueue
	// asyncQueueSize is the bounded best-effort admission capacity.
	asyncQueueSize int
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
	// evicted is the precise "history became lossy" signal: true once an
	// append has overwritten a previously-occupied slot (distinct from
	// ringFull, which is true at exactly-capacity BEFORE any eviction). It
	// flows to HistoryReplayer.Bounds' truncated return.
	evicted bool
	ringCap int

	mu sync.RWMutex
	// subs is the canonical lifecycle set. The secondary buckets below are
	// maintained under the same lock and only narrow fan-out candidates; every
	// candidate still passes Filter.Matches before enqueue.
	subs           map[uint64]*subscription
	subsByIdentity map[identity.Identity]map[uint64]*subscription
	adminSubs      map[uint64]*subscription
	subID          atomic.Uint64

	closed    atomic.Bool
	closeOnce sync.Once
	closeDone chan struct{}

	reaperWG sync.WaitGroup

	// droppedTotal is the cumulative count of events dropped across all
	// subscribers under buffer backpressure since construction. Each
	// subscription's recordDrop bumps it through the shared pointer it
	// holds, so the count survives a subscriber being reaped. Read via
	// DroppedTotal (the events.DroppedCounter capability) to source the
	// runtime drop gauge. Atomic — no lock needed on the read path.
	droppedTotal atomic.Int64

	// fenced is the set of erased session triples (see events.Fencer). It
	// is guarded by publishMu — the SAME lock that serialises ring
	// append + every history read snapshots under — so the fence, the ring,
	// and the reads never disagree. A fenced triple's events are not
	// appended to the ring (so nothing is retained) and read as empty
	// history; Fence also physically purges any already-retained entries for
	// the triple (the ring has no DeleteScope to lean on, unlike the durable
	// driver).
	fenced map[string]struct{}
	// erasureGeneration is retained after Unfence. It invalidates events
	// admitted before a Fence even if they reach the commit lane after the
	// session is reused.
	erasureGeneration map[string]uint64

	// liveFenceMu linearises the live fence check with live fan-out. It is
	// held only around the short fenced-map check plus bounded fan-out, never
	// during validation or redaction. Fence takes the write lock before its
	// existing publishMu critical section, so once Fence returns no already-
	// in-flight live publication can reach a subscriber.
	liveFenceMu sync.RWMutex

	// wake is the bounded best-effort watermark notification hub backing
	// the events.ProjectionSource seam (see projection.go). Publish
	// notifies it after each successful persistence of a canonical event;
	// the hub's non-blocking sends never couple the publish path to a
	// projector. Zero value is ready to use.
	wake events.ProjectionWakeHub
}

// fenceKey renders a session triple as the fenced-set key. The NUL
// separator can never appear in an id, so distinct triples never collide.
func fenceKey(id identity.Identity) string {
	return id.TenantID + "\x00" + id.UserID + "\x00" + id.SessionID
}

// fencedLocked reports whether the event's session triple is fenced.
// Caller must hold publishMu.
func (b *bus) fencedLocked(id identity.Quadruple) bool {
	if b.fenced == nil {
		return false
	}
	_, ok := b.fenced[fenceKey(id.Identity)]
	return ok
}

// isFenced is the lock-acquiring sibling of fencedLocked (the Publish-edge
// fast check).
func (b *bus) isFenced(id identity.Quadruple) bool {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	return b.fencedLocked(id)
}

// generationForAdmission captures the identity's erasure generation under
// publishMu at queue admission. The generation remains valid only until a
// later Fence increments it; the commit path rechecks it before sequencing.
func (b *bus) generationForAdmission(id identity.Identity) (uint64, bool) {
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	key := fenceKey(id)
	if _, fenced := b.fenced[key]; fenced {
		return 0, false
	}
	return b.erasureGeneration[key], true
}

// generationMatchesLocked reports whether a queued request still belongs to
// the current identity generation. Caller must hold publishMu.
func (b *bus) generationMatchesLocked(id identity.Quadruple, generation uint64) bool {
	return b.erasureGeneration[fenceKey(id.Identity)] == generation
}

// Fence implements events.Fencer. It marks the triple erased — future
// events for it are not retained and its history reads empty — and
// physically purges any already-retained ring entries for the triple, so a
// later session reusing the same id (after Unfence) can never observe the
// erased session's events.
func (b *bus) Fence(_ context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	b.liveFenceMu.Lock()
	defer b.liveFenceMu.Unlock()
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	if b.fenced == nil {
		b.fenced = map[string]struct{}{}
	}
	if b.erasureGeneration == nil {
		b.erasureGeneration = map[string]uint64{}
	}
	key := fenceKey(id)
	b.erasureGeneration[key]++
	b.fenced[key] = struct{}{}
	b.purgeFencedLocked(identity.Quadruple{Identity: id})
	return nil
}

// Unfence implements events.Fencer. Idempotent.
func (b *bus) Unfence(_ context.Context, id identity.Identity) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	b.liveFenceMu.Lock()
	defer b.liveFenceMu.Unlock()
	b.publishMu.Lock()
	defer b.publishMu.Unlock()
	delete(b.fenced, fenceKey(id))
	return nil
}

// purgeFencedLocked rebuilds the ring with every event for the given
// triple removed, preserving sequence order for the survivors. Caller must
// hold publishMu. The monotonic sequence counter (nextSeq) is left
// untouched — sequences stay globally monotonic; the purge only drops
// retained slots.
func (b *bus) purgeFencedLocked(id identity.Quadruple) {
	if b.ringCap == 0 {
		return
	}
	snapshot := b.ringSnapshotLocked()
	kept := make([]events.Event, 0, len(snapshot))
	for _, ev := range snapshot {
		if fenceKey(ev.Identity.Identity) == fenceKey(id.Identity) {
			continue
		}
		kept = append(kept, ev)
	}
	if len(kept) == len(snapshot) {
		return // nothing for this triple was retained
	}
	// Rewrite the ring from the survivors (oldest-first). Removal can only
	// shrink the live set, so the ring is no longer full unless it happened
	// to stay at exactly capacity. evicted is preserved: if history was
	// already lossy it stays flagged.
	for i := range b.ringBuf {
		b.ringBuf[i] = events.Event{}
	}
	copy(b.ringBuf, kept)
	b.ringHead = len(kept) % b.ringCap
	b.ringFull = len(kept) == b.ringCap
}

// DroppedTotal reports the cumulative number of events dropped under
// subscriber-buffer backpressure since the bus was constructed.
// Satisfies events.DroppedCounter so the runtime observability wiring
// can source the harbor_runtime_events_dropped gauge from the live bus.
func (b *bus) DroppedTotal() int64 {
	return b.droppedTotal.Load()
}

// OldestRetainedAt implements events.RetentionReporter. It returns the
// OccurredAt of the oldest event still in the replay ring — the observed
// retention horizon — advancing as the ring evicts. present is false
// when the ring holds no retained (non-notice) event. Snapshots the ring
// under publishMu so the read never tears against an in-flight Publish.
func (b *bus) OldestRetainedAt(_ context.Context) (time.Time, bool, error) {
	if b.closed.Load() {
		return time.Time{}, false, events.ErrBusClosed
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	b.publishMu.Unlock()
	// The snapshot is oldest-first; the first non-notice event is the
	// horizon. Bus-internal notices are excluded so the horizon matches
	// the shape of the history a session read would surface.
	for _, ev := range snapshot {
		if events.IsBusInternalNotice(ev.Type) {
			continue
		}
		return ev.OccurredAt, true, nil
	}
	return time.Time{}, false, nil
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
// Publish validates and admits one durable event onto the ordered publication
// lane. The queue worker assigns its sequence and fans it out after commit.
func (b *bus) Publish(ctx context.Context, ev events.Event) error {
	prepared, accepted, err := b.preparePublish(ctx, ev)
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	generation, admitted := b.generationForAdmission(prepared.Identity.Identity)
	if !admitted {
		b.logFencedDrop(ctx, prepared)
		return nil
	}
	return b.ordered.PublishWithGeneration(ctx, prepared, generation)
}

// PublishBatch validates and admits one atomic durable batch. All events must
// name one identity/session so the batch has one ordering authority and one
// contiguous sequence range. Validation and redaction complete before queue
// admission, so a rejected event cannot leave a partial durable batch.
func (b *bus) PublishBatch(ctx context.Context, batch []events.Event) error {
	if len(batch) == 0 {
		return fmt.Errorf("inmem: publish batch is empty")
	}
	if len(batch) > events.DefaultPublishBatchSize {
		return fmt.Errorf("inmem: publish batch length %d exceeds max %d", len(batch), events.DefaultPublishBatchSize)
	}
	if !sameBatchSession(batch) {
		return fmt.Errorf("inmem: publish batch must use one identity/session")
	}
	prepared := make([]events.Event, 0, len(batch))
	for _, ev := range batch {
		preparedEvent, accepted, err := b.preparePublish(ctx, ev)
		if err != nil {
			return err
		}
		if accepted {
			prepared = append(prepared, preparedEvent)
		}
	}
	if len(prepared) == 0 {
		return nil
	}
	generation, admitted := b.generationForAdmission(prepared[0].Identity.Identity)
	if !admitted {
		for _, ev := range prepared {
			b.logFencedDrop(ctx, ev)
		}
		return nil
	}
	return b.ordered.PublishBatchWithGeneration(ctx, prepared, generation)
}

// PublishAsync admits a best-effort observability event without waiting for
// the store. ErrAsyncQueueFull is returned immediately when the bounded lane
// is saturated; an accepted event is committed by the same FIFO worker as
// ordinary Publish.
func (b *bus) PublishAsync(ctx context.Context, ev events.Event) error {
	prepared, accepted, err := b.preparePublish(ctx, ev)
	if err != nil {
		return err
	}
	if !accepted {
		return nil
	}
	generation, admitted := b.generationForAdmission(prepared.Identity.Identity)
	if !admitted {
		b.logFencedDrop(ctx, prepared)
		return nil
	}
	return b.ordered.PublishAsyncWithGeneration(ctx, prepared, generation)
}

// ObserveAsyncAdmissionFailure records a bounded-lane admission failure
// without changing the producer's business result.
func (b *bus) ObserveAsyncAdmissionFailure(ctx context.Context, eventType events.EventType, err error) {
	b.asyncSignal.Observe(ctx, eventType, err)
}

// AsyncAdmissionFailures reports the cumulative bounded-lane admission
// failures observed since this bus was constructed.
func (b *bus) AsyncAdmissionFailures() int64 {
	return b.asyncSignal.Total()
}

// Flush waits for all earlier accepted durable and asynchronous publications
// to commit or report their first failure. Terminal runtime paths call this
// before materializing successful completion.
func (b *bus) Flush(ctx context.Context) error {
	return b.ordered.Flush(ctx)
}

func (b *bus) preparePublish(ctx context.Context, ev events.Event) (events.Event, bool, error) {
	if ctx == nil {
		return events.Event{}, false, fmt.Errorf("inmem: publish context is nil")
	}
	if b.closed.Load() {
		return events.Event{}, false, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.Event{}, false, fmt.Errorf("inmem: publish cancelled: %w", err)
	}
	if err := events.ValidateEvent(ev); err != nil {
		return events.Event{}, false, err
	}
	if b.isFenced(ev.Identity) {
		b.logFencedDrop(ctx, ev)
		return events.Event{}, false, nil
	}
	payload := ev.Payload
	if _, safe := payload.(events.SafePayload); !safe {
		redacted, err := b.redactor.Redact(ctx, payload)
		if err != nil {
			b.emitRedactionFailure(ctx, ev, err)
			return events.Event{}, false, fmt.Errorf("events: publish redaction failed: %w", err)
		}
		payload = wrapRedacted(redacted)
	}
	ev.Payload = payload
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}
	return ev, true, nil
}

func (b *bus) commitBatch(ctx context.Context, batch []events.Event, generation uint64, _ string) error {
	if len(batch) == 0 {
		return fmt.Errorf("inmem: empty publication batch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	b.publishMu.Lock()
	if !b.generationMatchesLocked(batch[0].Identity, generation) || b.fencedLocked(batch[0].Identity) {
		b.publishMu.Unlock()
		for _, ev := range batch {
			b.logFencedDrop(ctx, ev)
		}
		return nil
	}
	if uint64(len(batch)) > ^uint64(0)-b.nextSeq {
		b.publishMu.Unlock()
		return fmt.Errorf("inmem: global sequence exhausted")
	}
	accepted := b.assignSeqAndStoreBatchLocked(batch)
	b.publishMu.Unlock()
	for _, ev := range accepted {
		b.notifyProjectionWatermark(ev)
		b.fanOut(ev, false)
	}
	return nil
}

func (b *bus) reportAsyncFailure(ctx context.Context, batch []events.Event, err error) {
	slog.ErrorContext(ctx, "events(inmem): asynchronous event batch failed",
		slog.String("driver", "inmem"),
		slog.Int("events", len(batch)),
		slog.String("error", err.Error()))
}

func sameBatchSession(batch []events.Event) bool {
	if len(batch) < 2 {
		return true
	}
	first := batch[0].Identity.Identity
	for _, ev := range batch[1:] {
		if ev.Identity.Identity != first {
			return false
		}
	}
	return true
}

// PublishLive validates, redacts, and bounded-fan-outs a present-tense event.
// It does not assign a sequence, append to the replay ring, or notify
// projection watchers: live events are best-effort animation and carry the
// non-replayable Sequence == 0 sentinel. Validation and redaction happen
// before the live fence cutoff lock; the final check and fan-out are
// linearized with Fence so erased-session output cannot leak after Fence
// returns. SafePayload retains its existing redactor bypass. Durable
// semantic and lifecycle events must continue through Publish.
func (b *bus) PublishLive(ctx context.Context, ev events.Event) error {
	if b.closed.Load() {
		return events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("inmem: publish live cancelled: %w", err)
	}
	if err := events.ValidateEvent(ev); err != nil {
		return err
	}

	payload := ev.Payload
	if _, safe := payload.(events.SafePayload); !safe {
		redacted, err := b.redactor.Redact(ctx, payload)
		if err != nil {
			b.emitLiveRedactionFailure(ctx, ev, err)
			return fmt.Errorf("inmem: live publish redaction failed: %w", err)
		}
		payload = wrapRedacted(redacted)
	}
	ev.Payload = payload
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = b.clock.Now()
	}
	// ValidateEvent requires caller-provided Sequence to be zero. Keep the
	// explicit assignment here as a guard if Event construction changes later.
	ev.Sequence = 0
	b.fanOutLive(ctx, ev)
	return nil
}

// emitLiveRedactionFailure reports a live redaction failure without routing
// the notice through the durable Publish path. The notice itself is
// non-replayable and has no effect on the ring, sequence counter, or
// projection watermark.
func (b *bus) emitLiveRedactionFailure(ctx context.Context, original events.Event, cause error) {
	ev := events.Event{
		Type:       events.EventTypeAuditRedactionFailed,
		Identity:   original.Identity,
		OccurredAt: b.clock.Now(),
		Payload: events.AuditRedactionFailedPayload{
			OriginalType: original.Type,
			Reason:       cause.Error(),
		},
		Sequence: 0,
	}
	b.fanOutLive(ctx, ev)
}

// fanOutLive performs the final fenced-session check and the complete
// bounded fan-out under one read-side fence lock. Fence's write-side lock
// therefore waits for an already-admitted live fan-out to finish and blocks
// later ones before marking the session erased. No StateStore or replay work
// is performed here.
func (b *bus) fanOutLive(ctx context.Context, ev events.Event) {
	b.liveFenceMu.RLock()
	defer b.liveFenceMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return
	}
	b.publishMu.Lock()
	fenced := b.fencedLocked(ev.Identity)
	b.publishMu.Unlock()
	if fenced {
		b.logFencedDrop(ctx, ev)
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	b.fanOut(ev, true)
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
	b.fanOut(ev, false)
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

// fanOut selects subscribers from the exact identity bucket plus the
// explicit admin bucket, then retains Filter.Matches as the final predicate.
// Non-admin Subscribe requires a complete identity triple, so no tenant/user
// wildcard bucket is valid here. The bucket lookup makes unrelated connected
// sessions invisible to the hot path while preserving every existing filter
// and delivery rule.
func (b *bus) fanOut(ev events.Event, live bool) {
	key := ev.Identity.Identity
	b.mu.RLock()
	exact := b.subsByIdentity[key]
	matched := make([]*subscription, 0, len(exact)+len(b.adminSubs))
	for _, s := range exact {
		if s.cancelled.Load() {
			continue
		}
		if s.filter.Matches(ev) {
			matched = append(matched, s)
		}
	}
	for _, s := range b.adminSubs {
		if s.cancelled.Load() {
			continue
		}
		if s.filter.Matches(ev) {
			matched = append(matched, s)
		}
	}
	b.mu.RUnlock()
	for _, s := range matched {
		s.enqueue(ev, b, live)
	}
}

// Subscribe validates the filter, audits Admin scope, enforces the
// per-session subscriber cap, and returns a Subscription.
func (b *bus) Subscribe(_ context.Context, f events.Filter) (events.Subscription, error) {
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}

	key := identity.Identity{TenantID: f.Tenant, UserID: f.User, SessionID: f.Session}
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return nil, events.ErrBusClosed
	}
	if !f.Admin {
		// The exact identity bucket is the only non-admin scope that can
		// match this filter. Count active entries while holding the same
		// lock used for insertion so concurrent Subscribe calls cannot
		// oversubscribe the per-session cap.
		bucket := b.subsByIdentity[key]
		count := 0
		for _, existing := range bucket {
			if !existing.cancelled.Load() {
				count++
			}
		}
		if count >= b.cfg.MaxSubscribersPerSession {
			b.mu.Unlock()
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
		id:         id,
		filter:     f,
		bound:      bound,
		ch:         make(chan events.Event, b.cfg.SubscriberBufferSize),
		busDropped: &b.droppedTotal,
		bus:        b,
	}
	s.lastDrain.Store(b.clock.Now().UnixNano())
	s.lastDropEmit.Store(b.clock.Now().UnixNano())

	b.subs[id] = s
	if f.Admin {
		b.adminSubs[id] = s
	} else {
		bucket := b.subsByIdentity[key]
		if bucket == nil {
			bucket = make(map[uint64]*subscription)
			b.subsByIdentity[key] = bucket
		}
		bucket[id] = s
	}
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
		b.fanOut(notice, false)
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
	accepted := b.assignSeqAndStoreBatch([]events.Event{*ev})
	if len(accepted) > 0 {
		*ev = accepted[0]
	}
}

// assignSeqAndStoreBatch assigns one contiguous sequence range and appends
// every event under one lock. In-memory assignment cannot fail after
// validation, so the returned slice contains every input event; fenced
// events retain their sequence but are not retained in the ring, matching the
// legacy single-event behavior.
func (b *bus) assignSeqAndStoreBatch(batch []events.Event) []events.Event {
	if len(batch) == 0 {
		return nil
	}
	b.publishMu.Lock()
	accepted := b.assignSeqAndStoreBatchLocked(batch)
	b.publishMu.Unlock()
	return accepted
}

func (b *bus) assignSeqAndStoreBatchLocked(batch []events.Event) []events.Event {
	accepted := make([]events.Event, 0, len(batch))
	for _, ev := range batch {
		b.nextSeq++
		ev.Sequence = b.nextSeq
		if b.ringCap > 0 && !b.fencedLocked(ev.Identity) {
			b.ringAppendLocked(ev)
		}
		accepted = append(accepted, ev)
	}
	return accepted
}

// logFencedDrop records a dropped event for an erased (fenced) session at
// Info — the drop is a correct outcome, surfaced for observability rather
// than hidden (CLAUDE.md §13).
func (b *bus) logFencedDrop(ctx context.Context, ev events.Event) {
	slog.InfoContext(ctx, "events(inmem): dropped event for erased (fenced) session",
		slog.String("driver", "inmem"),
		slog.String("event_type", string(ev.Type)),
		slog.String("tenant_id", ev.Identity.TenantID),
		slog.String("user_id", ev.Identity.UserID),
		slog.String("session_id", ev.Identity.SessionID))
}

// ringAppendLocked writes ev to the next ring slot and advances the
// head. Caller must hold publishMu. Called only when ringCap > 0.
func (b *bus) ringAppendLocked(ev events.Event) {
	if b.ringFull {
		// The ring is already at capacity, so this append overwrites the
		// oldest retained event — the first actual eviction.
		b.evicted = true
	}
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
		// abuse is retroactively detectable. The Protocol auth layer
		// will add cryptographic verification; Harbor trusts the
		// boolean exactly as the Subscribe does.
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
		b.fanOut(notice, false)
	}

	// Snapshot the ring + record the head sequence under one lock
	// acquisition so the head boundary is consistent with the
	// snapshot's contents.
	b.publishMu.Lock()
	fenced := b.fencedLocked(identity.Quadruple{Identity: identity.Identity{TenantID: f.Tenant, UserID: f.User, SessionID: f.Session}})
	snapshot := b.ringSnapshotLocked()
	headSeq := b.nextSeq
	b.publishMu.Unlock()
	if fenced {
		// Erased session — no history to replay (events.Fencer).
		return nil, nil
	}

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

// Bounds implements events.HistoryReplayer. It reports the lowest and
// highest retained sequence among the ring events matching f, or
// events.ErrNoHistory when none match. Identity-scoped exactly like
// Subscribe / Replay. Best-effort: the ring is a bounded retention
// window, so the reported head reflects what the ring still holds.
func (b *bus) Bounds(_ context.Context, f events.Filter) (head, tail uint64, truncated bool, err error) {
	if b.closed.Load() {
		return 0, 0, false, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return 0, 0, false, events.ErrReplayUnavailable
	}
	if !f.Admin && !f.HasFullTriple() {
		return 0, 0, false, events.ErrIdentityScopeRequired
	}
	if f.Admin {
		b.emitAdminScopeUsedAndFanOut(f)
	}
	b.publishMu.Lock()
	fenced := b.fencedLocked(identity.Quadruple{Identity: identity.Identity{TenantID: f.Tenant, UserID: f.User, SessionID: f.Session}})
	snapshot := b.ringSnapshotLocked()
	// evicted is the honest "older events were dropped" signal: once an
	// append has overwritten an occupied slot, the oldest retained sequence
	// is NOT the session's first. Read under the same lock that mutates it.
	evicted := b.evicted
	b.publishMu.Unlock()
	if fenced {
		// Erased session — reads as no retained history (events.Fencer).
		return 0, 0, false, events.ErrNoHistory
	}

	var lo, hi uint64
	found := false
	for _, ev := range snapshot {
		// MatchesScoped, never Matches: this is a by-id read scoped to the
		// named session even under Admin — Admin must not fan the whole
		// ring across sessions/tenants.
		if events.IsBusInternalNotice(ev.Type) || !f.MatchesScoped(ev) {
			continue
		}
		if !found || ev.Sequence < lo {
			lo = ev.Sequence
		}
		if ev.Sequence > hi {
			hi = ev.Sequence
		}
		found = true
	}
	if !found {
		return 0, 0, false, events.ErrNoHistory
	}
	return lo, hi, evicted, nil
}

// Window implements events.HistoryReplayer. It returns at most limit
// events matching f whose Sequence < before (before==0 ⇒ from the tail),
// the most-recent K, returned oldest-first within the window. Scoped to
// the named session (MatchesScoped) even under Admin — a by-id read.
//
// Window does NOT emit audit.admin_scope_used: the handler always calls
// Bounds first, which emits it once per state.history request (a paired
// Bounds+Window must not double-audit a single read).
func (b *bus) Window(_ context.Context, before uint64, limit int, f events.Filter) ([]events.Event, error) {
	if b.closed.Load() {
		return nil, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return nil, events.ErrReplayUnavailable
	}
	if !f.Admin && !f.HasFullTriple() {
		return nil, events.ErrIdentityScopeRequired
	}
	if limit <= 0 {
		return nil, nil
	}
	b.publishMu.Lock()
	fenced := b.fencedLocked(identity.Quadruple{Identity: identity.Identity{TenantID: f.Tenant, UserID: f.User, SessionID: f.Session}})
	snapshot := b.ringSnapshotLocked()
	b.publishMu.Unlock()
	if fenced {
		// Erased session — no retained history window (events.Fencer).
		return nil, nil
	}

	if len(snapshot) == 0 {
		return nil, nil
	}
	out := make([]events.Event, 0, limit)
	for i := len(snapshot) - 1; i >= 0; i-- {
		ev := snapshot[i]
		if before != 0 && ev.Sequence >= before {
			continue
		}
		if events.IsBusInternalNotice(ev.Type) || !f.MatchesScoped(ev) {
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ListWindow implements events.HistoryReplayer. It serves the cross-
// session, time-ranged, cursor-paged `events.list` read from the in-memory
// ring: a bounded backward page of the most-recent matching events with
// Sequence < q.Before, returned oldest-first. Unlike Window (by-id,
// single session via MatchesScoped), ListWindow filters with MatchWire —
// the wire filter's multi-valued identity sets + since/until bounds — and,
// when q.Admin is set, fans in across every retained session. A non-admin
// query whose filter elides an identity axis is rejected with
// ErrIdentityScopeRequired (fail closed — the handler folds the caller's
// triple, but the driver does not trust that it did). A widened (Admin)
// query emits one audit.admin_scope_used before returning.
//
// truncated is the ring's honest retention signal (b.evicted): once an
// append has overwritten an occupied slot, older matching events may have
// been dropped and the page's oldest row is not guaranteed to be the true
// first match (CLAUDE.md §13 "never silently lossy").
func (b *bus) ListWindow(_ context.Context, q events.EventListQuery) (events.EventListPage, error) {
	if b.closed.Load() {
		return events.EventListPage{}, events.ErrBusClosed
	}
	if b.ringCap == 0 {
		return events.EventListPage{}, events.ErrReplayUnavailable
	}
	if !q.Admin && !events.WireFilterHasFullTriple(q.Filter) {
		return events.EventListPage{}, events.ErrIdentityScopeRequired
	}
	if q.Admin {
		b.emitAdminScopeUsedAndFanOut(events.Filter{
			Tenant:  events.WireFilterFirst(q.Filter.TenantIDs),
			User:    events.WireFilterFirst(q.Filter.UserIDs),
			Session: events.WireFilterFirst(q.Filter.SessionIDs),
		})
	}
	limit := q.Limit
	if limit <= 0 {
		return events.EventListPage{}, nil
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	evicted := b.evicted
	b.publishMu.Unlock()
	page := events.ListWindowFromSnapshot(snapshot, q.Before, limit, q.Filter)
	page.Truncated = evicted
	return page, nil
}

// ListWindowMetadata serves the same bounded page using the typed metadata
// projection. In-memory events already reside in process memory, but keeping
// this capability aligned with the durable driver makes aggregate and session
// counter consumers use one contract across all replay-capable drivers.
func (b *bus) ListWindowMetadata(ctx context.Context, q events.EventListQuery) (events.MetadataListPage, error) {
	if b.closed.Load() {
		return events.MetadataListPage{}, events.ErrBusClosed
	}
	if err := ctx.Err(); err != nil {
		return events.MetadataListPage{}, err
	}
	if b.ringCap == 0 {
		return events.MetadataListPage{}, events.ErrReplayUnavailable
	}
	if !q.Admin && !events.WireFilterHasFullTriple(q.Filter) {
		return events.MetadataListPage{}, events.ErrIdentityScopeRequired
	}
	if q.Admin {
		b.emitAdminScopeUsedAndFanOut(events.Filter{
			Tenant:  events.WireFilterFirst(q.Filter.TenantIDs),
			User:    events.WireFilterFirst(q.Filter.UserIDs),
			Session: events.WireFilterFirst(q.Filter.SessionIDs),
		})
	}
	b.publishMu.Lock()
	snapshot := b.ringSnapshotLocked()
	evicted := b.evicted
	b.publishMu.Unlock()
	page, err := events.MetadataListWindowFromSnapshot(snapshot, q.Before, q.Limit, q.Filter)
	if err != nil {
		return events.MetadataListPage{}, err
	}
	page.Truncated = evicted
	return page, nil
}

// emitAdminScopeUsedAndFanOut surfaces admin-scope use on the bus so
// abuse is retroactively detectable — mirrors the Replay admin path.
func (b *bus) emitAdminScopeUsedAndFanOut(f events.Filter) {
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
	b.fanOut(notice, false)
}

// Close idempotently shuts the bus down. After Close, Publish and
// Subscribe return ErrBusClosed; existing subscribers receive a
// closed Events() channel.
func (b *bus) Close(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("inmem: close context is nil")
	}
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.closeDone)
	})
	queueErr := b.ordered.Close(ctx)

	// Cancel all subscriptions.
	b.mu.Lock()
	subs := make([]*subscription, 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[uint64]*subscription{}
	b.subsByIdentity = map[identity.Identity]map[uint64]*subscription{}
	b.adminSubs = map[uint64]*subscription{}
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
		return queueErr
	case <-ctx.Done():
		return errors.Join(queueErr, ctx.Err())
	}
}

// subscription is the per-subscriber state.
type subscription struct {
	id     uint64
	filter events.Filter
	bound  identity.Quadruple
	ch     chan events.Event
	bus    *bus

	// Drop bookkeeping.
	mu            sync.Mutex // serialises enqueue against itself
	dropOpen      bool       // a drop window is in progress
	dropFromSeq   uint64
	dropToSeq     uint64
	dropCount     uint64
	lastDropEmit  atomic.Int64 // unix nano of last bus.dropped emit
	lastDrain     atomic.Int64 // unix nano of last successful read
	cancelled     atomic.Bool
	cancelledOnce sync.Once
	closeChanOnce sync.Once

	// busDropped points at the owning bus's cumulative drop counter so
	// recordDrop can bump a process-wide total that outlives this
	// subscription (the per-window dropCount above resets each window).
	busDropped *atomic.Int64
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
	s.cancelInternal(s.bus)
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
		b.removeSubscription(s)
	}
}

// removeSubscription removes a subscriber from the canonical lifecycle map
// and exactly one secondary bucket. It is idempotent so Cancel may race with
// Close or the idle reaper without leaving stale fan-out candidates behind.
// Caller must not hold s.mu; this method acquires only b.mu.
func (b *bus) removeSubscription(s *subscription) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, s.id)
	if s.filter.Admin {
		delete(b.adminSubs, s.id)
		return
	}
	key := s.bound.Identity
	bucket := b.subsByIdentity[key]
	if bucket == nil {
		return
	}
	delete(bucket, s.id)
	if len(bucket) == 0 {
		delete(b.subsByIdentity, key)
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
func (s *subscription) enqueue(ev events.Event, b *bus, live bool) {
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
		s.maybeEmitDropNotice(ev.Identity, b, now, live)
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
		s.maybeEmitDropNotice(ev.Identity, b, now, live)
	default:
		// Pathological — record this as dropped too.
		s.recordDrop(ev.Sequence, ev.Sequence)
	}
}

// (enqueueClosing was inlined into cancelInternalWithNotice — the
// closing notice and the channel close MUST happen under the same
// s.mu acquisition or they race against concurrent publishers.)

// recordDrop accumulates dropped sequence range into the open window.
// It also bumps the bus-level cumulative drop total (which outlives this
// subscription's per-window accounting) so the runtime drop gauge sees
// every drop, exactly once.
func (s *subscription) recordDrop(fromSeq, toSeq uint64) {
	if s.busDropped != nil {
		s.busDropped.Add(1)
	}
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
func (s *subscription) maybeEmitDropNotice(forIdentity identity.Quadruple, b *bus, now time.Time, live bool) {
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
	// A live invocation must remain completely transient even when it
	// displaces a previously published durable event. A durable invocation
	// retains the existing sequenced notice semantics.
	if !live {
		b.assignSeqAndStore(&notice)
	}

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

// Compile-time assertion that bus implements events.EventBus,
// events.Replayer AND events.HistoryReplayer.
var (
	_ events.EventBus                      = (*bus)(nil)
	_ events.LivePublisher                 = (*bus)(nil)
	_ events.Replayer                      = (*bus)(nil)
	_ events.HistoryReplayer               = (*bus)(nil)
	_ events.EventMetadataReplayer         = (*bus)(nil)
	_ events.Fencer                        = (*bus)(nil)
	_ events.ProjectionSource              = (*bus)(nil)
	_ events.AsyncAdmissionFailureObserver = (*bus)(nil)
	_ events.AsyncAdmissionCounter         = (*bus)(nil)
)

// Compile-time assertion: subscription.Cancel is exported via the
// interface; satisfying both Events() and Cancel() suffices.
var _ events.Subscription = (*subscription)(nil)

// errors.Is helper for bus-closed checks (avoids package-level cycle).
var _ = errors.Is
