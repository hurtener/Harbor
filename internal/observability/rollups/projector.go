package rollups

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// Source yields successfully-persisted canonical events for the projector
// to consume, in global bus-sequence order.
//
// "Successfully persisted" is the contract: the durable event log is the
// canonical source — the runtime's StateStore-backed durable bus driver
// persists every event before it is fanned out, and its per-bus sequence is
// gap-free. A Source implementation SHOULD be backed by that log (for
// example by scanning its persisted entry records via the StateStore
// maintenance surface, in global sequence order) and MUST return each
// sequence at most once across calls, in strictly ascending order, without
// gaps.
//
// Next promises AT MOST limit events: a batch of fewer than limit events
// does NOT prove the source is exhausted — more events may exist beyond the
// returned prefix. Only an empty read (nil slice) reports "caught up". This
// is the load-bearing contract behind the projector's state machine: a
// short non-empty batch leaves the projector in StateCatchingUp until a
// SUBSEQUENT read returns empty.
//
// The projector owns the cursor: Next is called with the projector's current
// checkpoint and must return events strictly newer than it. (nil, nil) means
// the source holds nothing newer (the projector reports StateCurrent).
type Source interface {
	// Next returns at most limit events whose Sequence is strictly greater
	// than after, in ascending Sequence order (oldest first). A short
	// non-empty batch does NOT report exhaustion — only (nil, nil) means
	// "caught up". An error stops the projector with StateUnavailable; the
	// checkpoint is NOT advanced.
	Next(ctx context.Context, after uint64, limit int) ([]events.Event, error)
}

// State is the projector's catch-up quality.
type State string

const (
	// StateCurrent — the last read was EMPTY: nothing newer than the
	// watermark existed at that read, so the rollups are caught up with
	// the log as of it. A live runtime may persist new events a moment
	// later; the next Advance moves back to StateCatchingUp until it
	// drains them. A short non-empty batch NEVER proves current — only an
	// empty read does.
	StateCurrent State = "current"
	// StateCatchingUp — more events remain, or the projector has not yet
	// verified the log head after a construction / rebuild, or the last
	// read returned a non-empty batch (short or full) that did not prove
	// exhaustion. Rollups may trail the live log.
	StateCatchingUp State = "catching_up"
	// StateUnavailable — the source or the store failed; the projector
	// cannot make progress. Quality.Err carries the last failure.
	StateUnavailable State = "unavailable"
)

// Quality is the projector's operational snapshot: catch-up state, the
// watermark (the last applied local durable sequence), and the retention
// horizon of the rows the store holds. It is a READ-ONLY view — it never
// mutates the projector or the store.
type Quality struct {
	// State is current / catching_up / unavailable.
	State State
	// Watermark is the last successfully applied sequence (the existing
	// local durable sequence — read from the Store's checkpoint, so it is
	// the durable truth across restarts).
	Watermark uint64
	// WatermarkAt is the wall-clock instant the watermark last advanced in
	// THIS projector instance (zero before the first advance — e.g. right
	// after a restart, before catch-up ran).
	WatermarkAt time.Time
	// RetentionStart is the oldest retained bucket start (zero when the
	// store holds no rows).
	RetentionStart time.Time
	// RetentionEnd is the newest retained bucket start (zero when the
	// store holds no rows).
	RetentionEnd time.Time
	// Err is the latest ingestion failure, present only when State is
	// StateUnavailable.
	Err error
}

// ProjectorOption configures the projector at construction. The options are
// test/operator seams; production wiring uses the defaults.
type ProjectorOption func(*Projector)

// WithProjectorBatchSize overrides the events per batch (default
// defaultProjectorBatchSize). A non-positive value is ignored.
func WithProjectorBatchSize(n int) ProjectorOption {
	return func(p *Projector) {
		if n > 0 {
			p.batchSize = n
		}
	}
}

// WithProjectorClock injects the clock used for WatermarkAt stamps.
// Tests use a controllable clock; the default realClock is correct for
// production.
func WithProjectorClock(c Clock) ProjectorOption {
	return func(p *Projector) {
		if c != nil {
			p.clock = c
		}
	}
}

// Clock abstracts the projector's "now" so tests do not depend on the wall
// clock. Production passes nil and the real UTC clock is used.
type Clock interface {
	Now() time.Time
}

type realProjectorClock struct{}

func (realProjectorClock) Now() time.Time { return time.Now().UTC() }

// defaultProjectorBatchSize bounds one Advance batch. Batches are applied
// atomically (deltas + checkpoint), so a crash never loses the boundary
// between "applied" and "not yet applied".
const defaultProjectorBatchSize = 1000

// maxCatchUpIterations bounds one CatchUp call so a pathological source
// (one that never reports caught up) fails loudly instead of looping
// forever.
const maxCatchUpIterations = 1_000_000

// Projector consumes successfully-persisted canonical events from a Source
// and applies their measure deltas to a Store, checkpointing the existing
// local durable sequence (the bus Sequence) after every batch.
//
// The projector is a compiled artifact: source + store + options are set at
// construction and never mutated. Per-call state (the watermark, the catch-up
// state) is guarded by an internal mutex and is safe to read (Quality) and
// advance (Advance / CatchUp) concurrently — concurrent advances are
// serialised and idempotent through the Store's checkpoint guard.
//
// Best-effort posture: the projector is a DOWNSTREAM consumer of an
// already-successful publication. The durable log persisted the event and
// fanned it out BEFORE the projector reads it; the projector's failures
// (StateUnavailable, retried on the next Advance) therefore never fail the
// canonical event publication path. No caller should use projector quality
// to fail an already-persisted event. There is no outbox, no new event id,
// and no exactly-once claim: replay is idempotent through the atomic
// checkpoint, and the projector is the single writer to its Store.
type Projector struct {
	source Source
	store  Store
	clock  Clock

	batchSize int

	mu          sync.RWMutex
	watermark   uint64
	watermarkAt time.Time
	state       State
	lastErr     error
}

// NewProjector builds the projector over source + store. Both are
// mandatory. The constructor reads the Store's checkpoint (the durable
// watermark from a previous run) so a restart resumes exactly where the
// last run stopped — the restart catch-up path. The initial State is
// StateCatchingUp until the first empty read verifies the log head.
func NewProjector(source Source, store Store, opts ...ProjectorOption) (*Projector, error) {
	if source == nil {
		return nil, fmt.Errorf("rollups: NewProjector: source is nil")
	}
	if store == nil {
		return nil, fmt.Errorf("rollups: NewProjector: store is nil")
	}
	p := &Projector{
		source:    source,
		store:     store,
		clock:     realProjectorClock{},
		batchSize: defaultProjectorBatchSize,
		state:     StateCatchingUp,
	}
	for _, opt := range opts {
		opt(p)
	}
	ckpt, err := store.Checkpoint(context.Background())
	if err != nil {
		return nil, fmt.Errorf("rollups: NewProjector: read checkpoint: %w", err)
	}
	p.watermark = ckpt
	return p, nil
}

// Advance processes one batch: it reads the next batch of events from the
// Source, drops events for fenced (erased) sessions, extracts the deltas of
// the survivors, and applies them atomically with the checkpoint — then
// reports whether the Source proved caught up.
//
// The catch-up proof is an EMPTY read: a short non-empty batch does NOT
// mark the projector current, because Source.Next promises at most limit
// events, not "the rest" — more may exist beyond the returned prefix. Only
// a subsequent read that returns no events (or an explicit source-head
// contract, which none of the shipped sources implements) marks
// StateCurrent.
//
// A gap or reorder in the Source's sequences fails loudly (the checkpoint
// would jump over events — a permanent undercount). A batch rejected by the
// Store (e.g. ErrSessionFenced from a fence that landed between the
// pre-check and the apply) leaves the checkpoint untouched; the next call
// drops the now-fenced event and progresses.
//
// Advance is safe for concurrent use: concurrent calls are idempotent
// through the Store's checkpoint guard (a call whose batch does not advance
// the stored checkpoint applies nothing).
func (p *Projector) Advance(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	p.mu.RLock()
	after := p.watermark
	batchSize := p.batchSize
	p.mu.RUnlock()

	batch, err := p.source.Next(ctx, after, batchSize)
	if err != nil {
		p.fail(err)
		return false, fmt.Errorf("rollups: projector source: %w", err)
	}
	if len(batch) == 0 {
		// Only an empty read proves the source holds nothing newer — the
		// log head has been verified.
		p.setState(StateCurrent, nil)
		return true, nil
	}

	// Strict contiguity guard: the durable log's global sequences are
	// gap-free, so the first event must be exactly after+1 and every
	// following event exactly one more. A violation means the Source
	// skipped or duplicated sequences — the checkpoint would jump over
	// events (permanent undercount) or re-apply them (double-count).
	expected := after + 1
	deltas := make([]Delta, 0, len(batch))
	var lastSeq uint64
	for _, ev := range batch {
		if ev.Sequence != expected {
			gapErr := fmt.Errorf("rollups: projector: source sequence gap: got seq=%d want seq=%d (a durable-log source must be gap-free)", ev.Sequence, expected)
			p.fail(gapErr)
			return false, gapErr
		}
		expected++
		lastSeq = ev.Sequence

		fenced, err := p.store.IsFenced(ctx, ev.Identity.Identity)
		if err != nil {
			p.fail(err)
			return false, fmt.Errorf("rollups: projector fence check: %w", err)
		}
		if fenced {
			// A late event for an erased session: drop it (the erasure
			// cascade fenced the triple; the store would reject the row).
			continue
		}
		ds, err := Extract(ev)
		if err != nil {
			p.fail(err)
			return false, err
		}
		deltas = append(deltas, ds...)
	}

	// Every consumed event advances the checkpoint — including fenced
	// (dropped) and unsupported-type events, which contribute no deltas but
	// must never be re-read. Applying an empty-delta batch is exactly the
	// cursor advance.
	if err := p.store.ApplyBatch(ctx, Batch{Checkpoint: lastSeq, Deltas: deltas}); err != nil {
		p.fail(err)
		return false, fmt.Errorf("rollups: projector apply: %w", err)
	}
	p.mu.Lock()
	p.watermark = lastSeq
	p.watermarkAt = p.clock.Now()
	p.lastErr = nil
	p.mu.Unlock()

	// The batch was non-empty, so it did not prove exhaustion: remain
	// catching_up until a subsequent read returns empty.
	p.setState(StateCatchingUp, nil)
	return false, nil
}

// CatchUp advances in batches until the Source proves caught up with an
// empty read, honouring ctx. It is a convenience loop over Advance for the
// operator paths that want "drain the backlog now". Bounded by
// maxCatchUpIterations so a pathological source fails loudly rather than
// looping forever.
func (p *Projector) CatchUp(ctx context.Context) error {
	for i := 0; i < maxCatchUpIterations; i++ {
		caughtUp, err := p.Advance(ctx)
		if err != nil {
			return err
		}
		if caughtUp {
			return nil
		}
	}
	return fmt.Errorf("rollups: projector: catch-up exceeded %d iterations without reaching the log head", maxCatchUpIterations)
}

// Quality returns the projector's operational snapshot. The watermark is
// read from the Store's checkpoint (the durable truth, correct across
// restarts); the state is this instance's last advance result; retention
// comes from the Store's rows.
func (p *Projector) Quality(ctx context.Context) (Quality, error) {
	p.mu.RLock()
	state := p.state
	wmAt := p.watermarkAt
	lastErr := p.lastErr
	p.mu.RUnlock()

	ckpt, err := p.store.Checkpoint(ctx)
	if err != nil {
		return Quality{}, fmt.Errorf("rollups: projector quality: checkpoint: %w", err)
	}
	oldest, newest, err := p.store.Retention(ctx)
	if err != nil {
		return Quality{}, fmt.Errorf("rollups: projector quality: retention: %w", err)
	}
	q := Quality{
		State:          state,
		Watermark:      ckpt,
		WatermarkAt:    wmAt,
		RetentionStart: oldest,
		RetentionEnd:   newest,
	}
	if state == StateUnavailable {
		q.Err = lastErr
	}
	return q, nil
}

// Rebuild resets the store's projection rows and checkpoint so the
// projector reprocesses the full log from the beginning — the rebuild path
// for a corrupted projection or a changed extractor. Erasure fences are
// PERMANENT and are never cleared (the Store's Rebuild preserves them), so
// an erased session stays erased through reprojection: rebuilding rows or
// the checkpoint cannot authorize resurrection. The State returns to
// StateCatchingUp.
func (p *Projector) Rebuild(ctx context.Context) error {
	if err := p.store.Rebuild(ctx); err != nil {
		return fmt.Errorf("rollups: projector rebuild: %w", err)
	}
	p.mu.Lock()
	p.watermark = 0
	p.watermarkAt = time.Time{}
	p.state = StateCatchingUp
	p.lastErr = nil
	p.mu.Unlock()
	return nil
}

// setState records a new catch-up state (and clears the last error on
// success paths).
func (p *Projector) setState(s State, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = s
	if err == nil {
		p.lastErr = nil
	}
}

// fail records the StateUnavailable state and the failure.
func (p *Projector) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = StateUnavailable
	p.lastErr = err
}
