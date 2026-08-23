// Package projectorworker runs the observability-rollup projection as a
// runtime worker: it consumes the successfully-persisted canonical event
// stream (the events.ProjectionSource seam — the existing local durable
// sequence, never the live fan-out) and applies the supported measure
// deltas to a rollups.Store, advancing the durable watermark after every
// page.
//
// # What this worker is
//
// The rollups domain core (`internal/observability/rollups`) owns the
// pure event→delta extraction (rollups.Extract), the Store interface and
// its drivers, and the query surface. This package owns the WIRING: the
// bounded forward paging over the persisted event source, the atomic
// page apply (deltas + watermark in one Store transaction), the catch-up
// state machine (current / catching_up / unavailable), the background
// run loop (wake notifications from the source's best-effort hub plus a
// lost-wake poll), and the honest quality surface that includes the
// source's observed watermark and retention-gap signal.
//
// The worker deliberately drives the source's own page cursor: the
// ProjectionSource serves CANONICAL events only (bus-internal notices
// and erased-session events are excluded by the drivers), so its page
// sequences are not contiguous with the raw log. The durable checkpoint
// normally is the last applied canonical sequence. After a page proves
// ProjectionCurrent, it may additionally advance through the page's
// observed Watermark when the intervening raw sequences were excluded;
// that is safe only after the returned page has been applied and avoids
// revisiting a permanently excluded tail. Both values remain valid
// resume positions for the source because the source pages strictly
// after the caller-provided sequence.
//
// # Best-effort downstream posture (no publication coupling)
//
// The worker is a DOWNSTREAM consumer of an already-successful
// publication: the durable log persisted and fanned out each event
// BEFORE the worker reads it. A worker failure (recorded in Quality as
// StateUnavailable and retried on the next wake / poll / Advance) can
// therefore never fail the canonical event publication path. There is
// no outbox, no new canonical event id, and no active-active exactly-
// once claim: the Store is the single writer of its rows, replay is
// idempotent through the atomic page apply (a batch whose checkpoint
// does not advance the stored checkpoint is a no-op), and concurrent
// replica application is at-least-once idempotent on the local
// sequence — never claimed exactly-once.
//
// # Honesty rules the worker enforces
//
//   - Only an EMPTY page proves the projection is current. A short
//     non-empty page (even one the source labels Current) never does:
//     more events may exist beyond the returned prefix. The worker
//     stays catching_up until a subsequent read returns no events.
//   - A source that reports ProjectionUnavailable (no retained
//     substrate) is NEVER treated as an empty stream — the worker
//     fails loudly into StateUnavailable.
//   - Erasure fences are permanent: the worker drops events for
//     fenced triples at ingestion (the store refuses them and the
//     source excludes them), and Rebuild never clears the store's
//     fences, so reprojection cannot resurrect an erased session.
//   - Retention quality is preserved: the worker records the source's
//     observed watermark and latches its retention-gap signal, so a
//     truncated substrate is never mistaken for a complete stream.
//
// # Concurrency
//
// A *Worker is a compiled artifact: source + store + options are set at
// construction and never mutated. The state-mutating path (Advance /
// CatchUp / the run loop's drain steps / Rebuild) is serialised by one
// advance mutex held for the FULL step — watermark read, page read,
// fence checks, apply, and the in-memory watermark/state update — so a
// delayed Advance can never overwrite the watermark backwards and a
// Rebuild can never interleave with an in-flight Advance. Quality is a
// concurrent read.
package projectorworker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// Defaults and bounds.
const (
	// defaultBatchSize bounds one Advance page. Pages are applied
	// atomically (deltas + watermark), so a crash never loses the
	// boundary between "applied" and "not yet applied".
	defaultBatchSize = 1000
	// defaultWakeBuffer is the capacity of the source wake sink. The
	// source's hub sends non-blocking and drops on a full sink; a
	// dropped wake is healed by the next forward page and the poll.
	defaultWakeBuffer = 16
	// defaultPollInterval is the lost-wake fallback poll. Wakes are the
	// primary notification; the poll bounds staleness if a wake is ever
	// lost or the source stops notifying. An idle tick compares the cheap
	// source watermark and durable checkpoint first, and only pages when
	// the projection is behind (or retrying a prior failure).
	defaultPollInterval = 30 * time.Second
	// maxCatchUpIterations bounds one CatchUp call so a pathological
	// source (one that never reports an empty page) fails loudly instead
	// of looping forever.
	maxCatchUpIterations = 1_000_000
)

// Option configures the worker at construction. The options are
// test/operator seams; production wiring uses the defaults.
type Option func(*Worker)

// WithBatchSize overrides the events per page (default
// defaultBatchSize). A non-positive value is ignored.
func WithBatchSize(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.batchSize = n
		}
	}
}

// WithClock injects the clock used for WatermarkAt stamps. Tests use a
// controllable clock; the default real UTC clock is correct for
// production.
func WithClock(c rollups.Clock) Option {
	return func(w *Worker) {
		if c != nil {
			w.clock = c
		}
	}
}

// WithWakeBuffer overrides the source wake-sink capacity (default
// defaultWakeBuffer). A non-positive value is ignored.
func WithWakeBuffer(n int) Option {
	return func(w *Worker) {
		if n > 0 {
			w.wakeBuffer = n
		}
	}
}

// WithPollInterval configures the lost-wake fallback poll interval
// (default defaultPollInterval). A non-positive value disables polling
// entirely — the worker then advances only on wake notifications and
// explicit Advance / CatchUp calls, which is the right choice for
// deterministic tests and for callers that drive the worker
// explicitly.
func WithPollInterval(d time.Duration) Option {
	return func(w *Worker) {
		w.pollInterval = d
	}
}

// realClock is the production clock (UTC).
type realClock struct{}

// Now implements rollups.Clock.
func (realClock) Now() time.Time { return time.Now().UTC() }

// Quality is the worker's operational snapshot: the catch-up state, the
// durable watermark, the retention horizon of the stored rows, and the
// source-side signals the worker observed (the source's high-water and
// its retention-gap flag). It is a READ-ONLY view — it never mutates
// the worker or the store.
type Quality struct {
	// Quality carries the shared rollup quality block: State
	// (current / catching_up / unavailable), Watermark (the durable
	// applied-through source checkpoint read from the Store — normally
	// the last canonical event, but possibly including excluded source
	// sequences proven current; this is the truth across restarts),
	// WatermarkAt (this instance's last advance stamp), RetentionStart /
	// RetentionEnd (the oldest /
	// newest retained bucket), and Err (the latest ingestion failure,
	// present only when State is StateUnavailable).
	rollups.Quality
	// SourceWatermark is the source's observed high-water from the
	// last page: the highest sequence that had completed persistence
	// when the page was assembled. It may exceed Watermark when the
	// intervening sequences belong to excluded bus-internal notices
	// or fenced sessions.
	SourceWatermark uint64
	// SourceRetentionGap latches whether the source ever reported
	// that events between the worker's cursor and the page head may
	// be missing (a wrapped ring evicted older events). It is cleared
	// by Rebuild and re-evaluated on the next page. A latched gap
	// means the projection's history is potentially incomplete and
	// must never be presented as complete.
	SourceRetentionGap bool
}

// Worker runs the observability-rollup projection over a persisted
// canonical event source and a rollups.Store.
//
// # Watermark semantics
//
// The durable watermark is the Store's checkpoint — normally the last
// applied canonical sequence, or a source watermark that a current page
// proved safe to skip across excluded records. The worker reads it from
// the Store at the START of every advance step (never a cached copy),
// pages the source strictly after it, and applies the page atomically
// with the checkpoint. Reading the durable checkpoint per step is what makes
// concurrent replica application converge instead of double-counting:
// two workers over one Store read the SAME cursor, produce IDENTICAL
// pages (same cursor + same batch size + a stable source), and the
// Store's non-advancing-batch gate turns the loser's apply into a
// no-op. Replay of the same page is idempotent; restart catch-up, a
// crash between source persistence and projection application, and
// concurrent identical-configuration replica application neither lose
// nor double-count values (at-least-once on the local sequence, never
// an active-active exactly-once claim).
//
// StateCurrent is reported only after an EMPTY page read: a short
// non-empty page (even one the source labels Current) never proves the
// source holds nothing newer.
type Worker struct {
	source events.ProjectionSource
	store  rollups.Store
	clock  rollups.Clock

	batchSize    int
	wakeBuffer   int
	pollInterval time.Duration

	// advanceMu serialises the entire state-mutating step: Advance /
	// CatchUp / the run loop's drain hold it for the FULL step
	// (checkpoint read, page read, fence checks, ApplyBatch, in-memory
	// state update) and Rebuild takes the same mutex, so no two advances
	// overlap and no rebuild interleaves with an in-flight advance.
	// Quality does NOT take it — reads stay concurrent.
	advanceMu sync.Mutex

	mu                 sync.RWMutex
	watermarkAt        time.Time
	state              rollups.State
	lastErr            error
	sourceWatermark    uint64
	sourceRetentionGap bool
}

// New builds the worker over source + store. Both are mandatory. The
// constructor verifies the Store is readable (the checkpoint read fails
// loudly on a closed / broken store); the durable watermark itself is
// read at the start of every advance step, so a restart resumes exactly
// where the last run stopped. The initial State is StateCatchingUp
// until the first empty page verifies the source head.
func New(source events.ProjectionSource, store rollups.Store, opts ...Option) (*Worker, error) {
	if source == nil {
		return nil, fmt.Errorf("projectorworker: New: source is nil")
	}
	if store == nil {
		return nil, fmt.Errorf("projectorworker: New: store is nil")
	}
	w := &Worker{
		source:       source,
		store:        store,
		clock:        realClock{},
		batchSize:    defaultBatchSize,
		wakeBuffer:   defaultWakeBuffer,
		pollInterval: defaultPollInterval,
		state:        rollups.StateCatchingUp,
	}
	for _, opt := range opts {
		opt(w)
	}
	// Construction-time checkpoint read uses a fresh background context:
	// New has no caller context to honour, and the read both verifies the
	// store and pins the durable watermark a restart resumes from.
	if _, err := store.Checkpoint(context.Background()); err != nil {
		return nil, fmt.Errorf("projectorworker: New: read checkpoint: %w", err)
	}
	return w, nil
}

// Advance processes one bounded page: it reads the next page of
// canonical persisted events from the Source, drops events for fenced
// (erased) sessions, extracts the deltas of the survivors, and applies
// them atomically with the watermark — then reports whether the Source
// proved caught up.
//
// The catch-up proof is an EMPTY page: a short non-empty page does NOT
// mark the worker current (the ProjectionSource promises at most limit
// events, not "the rest"), and a page the source itself labels Current
// is still non-empty until a SUBSEQUENT read returns no events. Only an
// empty read flips the state to StateCurrent.
//
// A page whose events violate the source's cursor contract (a sequence
// at or below the cursor, or a non-ascending sequence) fails loudly —
// the watermark would otherwise jump backwards or double-apply. A page
// whose Next does not equal its last event's sequence (a source bug)
// fails loudly too: the checkpoint would silently skip or under-count.
// A batch rejected by the Store (e.g. ErrSessionFenced from a fence
// that landed between the page and the apply) leaves the watermark
// untouched; the next call drops the now-fenced event and progresses.
//
// Advance is safe for concurrent use: the advance mutex is held for the
// FULL step, so concurrent advances are serialised end to end and a
// delayed advance can never apply a newer/no-op batch and then
// overwrite the watermark backwards. Rebuild takes the same mutex.
func (w *Worker) Advance(ctx context.Context) (bool, error) {
	w.advanceMu.Lock()
	defer w.advanceMu.Unlock()
	return w.advanceLocked(ctx)
}

// advanceLocked is the full state-mutating step; the caller holds
// advanceMu.
func (w *Worker) advanceLocked(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	// The DURABLE watermark is the resume cursor, read from the Store at
	// the start of every step — never a cached copy. This is what makes
	// concurrent replica application converge on one shared checkpoint
	// (identical cursors → identical pages → the Store's non-advancing
	// gate no-ops the loser) instead of overlapping advancing batches
	// that would double-count the intersection.
	after, err := w.store.Checkpoint(ctx)
	if err != nil {
		w.fail(err)
		return false, fmt.Errorf("projectorworker: checkpoint read: %w", err)
	}
	batchSize := w.batchSize

	page, err := w.source.Page(ctx, after, batchSize)
	if err != nil {
		w.fail(err)
		return false, fmt.Errorf("projectorworker: source page: %w", err)
	}
	if page.Quality == events.ProjectionUnavailable {
		// No retained substrate — NEVER an empty stream. Fail loudly so
		// an operator never mistakes "cannot run" for "caught up".
		err := fmt.Errorf("%w: page reports projection unavailable (no retained substrate)", events.ErrProjectionUnavailable)
		w.fail(err)
		return false, err
	}
	w.mu.Lock()
	w.sourceWatermark = page.Watermark
	if page.RetentionGap {
		// Latch the source's historical-incompleteness signal: once a
		// gap is observed, the projection's history may be incomplete
		// and stays honestly marked until a Rebuild re-evaluates.
		w.sourceRetentionGap = true
	}
	w.mu.Unlock()

	checkpoint := page.Next
	if page.Quality == events.ProjectionCurrent && page.Watermark > checkpoint {
		// ProjectionPage.Next is intentionally the last returned
		// canonical sequence. A current page has nevertheless examined
		// the source through Watermark, which may be ahead when the
		// intervening sequences are internal notices or fenced sessions.
		// Persist that skipped range only after the page has been fully
		// validated, otherwise every lost-wake poll repeats the global
		// head scan forever.
		checkpoint = page.Watermark
	}

	if len(page.Events) == 0 {
		// Only an empty page proves the source holds nothing newer — the
		// log head has been verified. Persist a current watermark that
		// advanced across excluded sequences so the next idle poll is a
		// cheap checkpoint/watermark read rather than another Page scan.
		if checkpoint > after {
			if err := w.store.ApplyBatch(ctx, rollups.Batch{Checkpoint: checkpoint}); err != nil {
				w.fail(err)
				return false, fmt.Errorf("projectorworker: advance excluded checkpoint: %w", err)
			}
			w.mu.Lock()
			w.watermarkAt = w.clock.Now()
			w.mu.Unlock()
		}
		w.setState(rollups.StateCurrent, nil)
		return true, nil
	}

	// Cursor-contract guards: the page must consist of strictly
	// ascending sequences all greater than the cursor, and Next must
	// equal the last returned event's sequence. A violation means the
	// source skipped or duplicated a sequence the watermark would jump
	// over (permanent undercount or double-count).
	if page.Next <= after {
		err := fmt.Errorf("projectorworker: source page cursor violation: Next=%d <= after=%d on a non-empty page", page.Next, after)
		w.fail(err)
		return false, err
	}
	deltas := make([]rollups.Delta, 0, len(page.Events))
	var prevSeq = after
	for _, ev := range page.Events {
		if ev.Sequence <= prevSeq {
			err := fmt.Errorf("projectorworker: source page violates the strictly-ascending cursor contract: seq=%d after seq=%d", ev.Sequence, prevSeq)
			w.fail(err)
			return false, err
		}
		prevSeq = ev.Sequence

		fenced, err := w.store.IsFenced(ctx, ev.Identity.Identity)
		if err != nil {
			w.fail(err)
			return false, fmt.Errorf("projectorworker: fence check: %w", err)
		}
		if fenced {
			// A late event for an erased session: drop it (the erasure
			// cascade fenced the triple; the store would reject the row).
			continue
		}
		ds, err := rollups.Extract(ev)
		if err != nil {
			w.fail(err)
			return false, err
		}
		deltas = append(deltas, ds...)
	}
	if page.Next != prevSeq {
		err := fmt.Errorf("projectorworker: source page cursor violation: Next=%d != last event seq=%d", page.Next, prevSeq)
		w.fail(err)
		return false, err
	}

	// Every consumed event advances the durable watermark — including
	// fenced (dropped) and unsupported-type events, which contribute no
	// deltas but must never be re-read. A current page may also advance
	// across excluded internal/fenced sequences through Watermark (see
	// checkpoint above). A concurrent replica may have already applied
	// this identical page; the Store's non-advancing gate turns that into
	// a no-op (no double-count).
	if err := w.store.ApplyBatch(ctx, rollups.Batch{Checkpoint: checkpoint, Deltas: deltas}); err != nil {
		w.fail(err)
		return false, fmt.Errorf("projectorworker: apply: %w", err)
	}
	w.mu.Lock()
	w.watermarkAt = w.clock.Now()
	w.lastErr = nil
	w.mu.Unlock()

	// The page was non-empty, so it did not prove exhaustion: remain
	// catching_up until a subsequent page returns empty.
	w.setState(rollups.StateCatchingUp, nil)
	return false, nil
}

// CatchUp advances in pages until the Source proves caught up with an
// empty read, honouring ctx. It is a convenience loop over Advance for
// the operator paths that want "drain the backlog now". Bounded by
// maxCatchUpIterations so a pathological source fails loudly rather
// than looping forever.
func (w *Worker) CatchUp(ctx context.Context) error {
	for range maxCatchUpIterations {
		caughtUp, err := w.Advance(ctx)
		if err != nil {
			return err
		}
		if caughtUp {
			return nil
		}
	}
	return fmt.Errorf("projectorworker: catch-up exceeded %d iterations without reaching the source head", maxCatchUpIterations)
}

// Run drives the projection in the background until ctx is cancelled.
// It registers a wake sink on the source (the source seeds the sink
// with its current watermark, then notifies after every successful
// persistence of a canonical event), drains until the first empty read,
// then waits for a wake notification or the lost-wake fallback poll
// before draining again.
//
// Run NEVER returns for a transient source/store failure: the failure
// is recorded in Quality as StateUnavailable and retried on the next
// wake or poll, because a rollup projection failure must never be able
// to fail the canonical event publication path (best-effort downstream
// posture). Run returns nil on graceful ctx cancellation, and an error
// only when the source refuses to register the sink at startup — a
// source with no retained substrate (events.ErrProjectionUnavailable)
// or a closed bus (events.ErrBusClosed) — after recording the failure
// in Quality.
//
// Run is safe to call alongside explicit Advance / CatchUp / Rebuild
// calls: all state-mutating steps share the advance mutex.
func (w *Worker) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wake := make(chan uint64, w.wakeBuffer)
	watch, err := w.source.Watch(ctx, wake)
	if err != nil {
		w.fail(err)
		return fmt.Errorf("projectorworker: run: register wake sink: %w", err)
	}
	defer watch.Unsubscribe()

	var (
		ticker *time.Ticker
		tick   <-chan time.Time
	)
	if w.pollInterval > 0 {
		ticker = time.NewTicker(w.pollInterval)
		defer ticker.Stop()
		tick = ticker.C
	}

	if err := w.CatchUp(ctx); err != nil {
		if ctx.Err() != nil {
			// Cancelled mid-drain: graceful shutdown, the drained
			// pages are already atomically applied.
			return nil
		}
		// Transient failure — already recorded as StateUnavailable
		// by Advance; retry on the next wake or poll.
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			// New events persisted (or the seed watermark): re-drain.
		case <-tick:
			// Lost-wake healer: first compare the cheap source watermark
			// with the durable projection checkpoint. A current projection
			// with no new source sequence does not need a global Page scan;
			// this is the idle path. A prior failure or a still-catching-up
			// pass deliberately retries even when the watermark is equal.
			wm, wmErr := w.source.Watermark(ctx)
			if wmErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				w.fail(wmErr)
				continue
			}
			checkpoint, checkpointErr := w.store.Checkpoint(ctx)
			if checkpointErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				w.fail(checkpointErr)
				continue
			}
			w.mu.RLock()
			current := w.state == rollups.StateCurrent
			w.mu.RUnlock()
			if current && wm <= checkpoint {
				continue
			}
		}
		if err := w.CatchUp(ctx); err != nil {
			if ctx.Err() != nil {
				// Cancelled mid-drain: graceful shutdown, the drained
				// pages are already atomically applied.
				return nil
			}
			// Transient failure — already recorded as StateUnavailable
			// by Advance; retry on the next wake or poll.
		}
	}
}

// Quality returns the worker's operational snapshot. The watermark is
// read from the Store's checkpoint (the durable truth, correct across
// restarts); the state is this instance's last page result; retention
// comes from the Store's rows; the source signals come from the last
// page read.
func (w *Worker) Quality(ctx context.Context) (Quality, error) {
	w.mu.RLock()
	state := w.state
	wmAt := w.watermarkAt
	lastErr := w.lastErr
	srcWm := w.sourceWatermark
	srcGap := w.sourceRetentionGap
	w.mu.RUnlock()

	ckpt, err := w.store.Checkpoint(ctx)
	if err != nil {
		return Quality{}, fmt.Errorf("projectorworker: quality: checkpoint: %w", err)
	}
	oldest, newest, err := w.store.Retention(ctx)
	if err != nil {
		return Quality{}, fmt.Errorf("projectorworker: quality: retention: %w", err)
	}
	q := Quality{
		Quality: rollups.Quality{
			State:          state,
			Watermark:      ckpt,
			WatermarkAt:    wmAt,
			RetentionStart: oldest,
			RetentionEnd:   newest,
		},
		SourceWatermark:    srcWm,
		SourceRetentionGap: srcGap,
	}
	if state == rollups.StateUnavailable {
		q.Err = lastErr
	}
	return q, nil
}

// Rebuild resets the store's projection rows and watermark so the
// worker reprocesses the full available source from the beginning —
// the rebuild path for a corrupted projection or a changed extractor.
// Erasure fences are PERMANENT and are never cleared (the Store's
// Rebuild preserves them), so an erased session stays erased through
// reprojection: rebuilding rows or the watermark cannot authorize
// resurrection. The retention-gap latch is cleared and re-evaluated
// from the next page. The State returns to StateCatchingUp.
//
// Rebuild takes the SAME advance mutex as Advance, so it coordinates
// with in-flight advances: a rebuild waits for any delayed advance to
// finish (the advance's page lands BEFORE the reset — never after it,
// which would jump the fresh watermark over the pre-rebuild events),
// and an advance waits for a rebuild to finish.
func (w *Worker) Rebuild(ctx context.Context) error {
	w.advanceMu.Lock()
	defer w.advanceMu.Unlock()

	if err := w.store.Rebuild(ctx); err != nil {
		return fmt.Errorf("projectorworker: rebuild: %w", err)
	}
	w.mu.Lock()
	w.watermarkAt = time.Time{}
	w.state = rollups.StateCatchingUp
	w.lastErr = nil
	w.sourceWatermark = 0
	w.sourceRetentionGap = false
	w.mu.Unlock()
	return nil
}

// setState records a new catch-up state (and clears the last error on
// success paths).
func (w *Worker) setState(s rollups.State, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = s
	if err == nil {
		w.lastErr = nil
	}
}

// fail records the StateUnavailable state and the failure.
func (w *Worker) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.state = rollups.StateUnavailable
	w.lastErr = err
}
