// Package materializer owns the runtime event → turn-projection
// observation mapping: the durable conversation-turn materializer.
//
// # What it is
//
// The materializer consumes ONLY the runtime's internal
// successfully-persisted canonical event source
// (events.ProjectionSource — the narrow forward-paging seam over the
// durable event log / retained ring, carrying the bus's own local
// monotonic sequence) and incrementally materializes the durable
// conversation-turn projection (turns.Projector) from it. It is the
// "runtime event → observation mapping" the turns package deliberately
// leaves out of scope (turns package doc) — the glue between the
// canonical task/result/planner/tool/MCP-App/pause/usage events and
// the projector's mutation DTOs.
//
// It is NOT:
//
//   - a generic projection framework — one dedicated projection, one
//     bounded schema;
//   - a projection warehouse or a second durable store — the projector
//     Store is the authoritative row source; the materializer's
//     in-memory per-session state holds only the accumulators the
//     bounded rows cannot retain (the FULL cumulative activity /
//     reasoning / usage / input feeds that keep positions, totals, and
//     snapshots stable) and the run/task routing index, all rebuilt
//     from the event source after a restart;
//   - coupled to publication — it never publishes, never subscribes to
//     the live fan-out, and never fails the publish path (it consumes
//     the source's best-effort wake notifications only to avoid
//     polling);
//   - a synchronous rebuild path — chat open reads the projection
//     (turns.Projector.List / Get) directly; the materializer runs in
//     the background and never rebuilds history on the query path.
//
// # Root foreground run selection and child folding
//
// A TURN is one root foreground run: a task.spawned event whose kind
// is foreground and whose parent task id is empty. Child / background
// tasks never become user messages ("background/child tasks fold into
// the root turn's Activity or are omitted by an explicit relationship
// rule"). Every event is routed by identity: task
// lifecycle events by their payload TaskID, run-scoped events (tool /
// planner / app / pause / usage / input-disposition) by the envelope's
// RunID, both walking the task→parent chain to the root foreground
// turn — so a child run's tool dispatches, planner decisions, usage,
// App refs, and pause episodes fold into its parent turn.
//
// # What each canonical family materializes
//
//   - task.spawned → row creation (root foreground) / routing index
//     (child); task.started / task.resumed → running (a resume clears
//     the pause episode); task.completed → a COMPLETE seal whose
//     answer source must have converged (see below); task.failed →
//     a FAILED seal with the closed content-free error class;
//     task.cancelled → a CANCELLED seal. Only the ROOT foreground
//     task's OWN terminal lifecycle seals its turn: a child /
//     background task's terminal events fold bounded activity (through
//     their run-scoped events) but NEVER seal the root. task.paused is
//     not the live pause path and is omitted by the explicit
//     relationship rule.
//   - planner.decision → one DERIVED consumer-safe reasoning step
//     (closed kind + chronological index). Raw provider thinking is
//     structurally absent from the projection.
//   - tool.invoked / completed / failed / policy_exhausted → ordered
//     content-free activity rows (never arguments/results) with the
//     exact turn-level totals; tool.completed/failed match the most
//     recent in-flight row for the tool (LIFO — the canonical events
//     carry no invocation id to match exactly; an unmatched completion
//     is omitted honestly).
//   - task.input_disposition.resolved → input attachment METADATA
//     (artifact id, MIME, effective disposition; never bytes).
//   - mcp.app_available → an ORDERED App reference (replacement
//     identity exactly (effective agent id, server id, resource uri));
//     the payload's Binding callback capability is structurally absent
//     and tool_call_id rides as correlation metadata only.
//   - pause.requested / pause.resumed → the durable token-free pause
//     episode (class derived from the canonical planner reason;
//     lifecycle; availability) and the running↔paused status
//     transitions. No pause/resume/approval token is ever stored and
//     actionability is not materialized.
//   - llm.cost.recorded → the cumulative per-measure usage rollup
//     (tokens exact when a positive cumulative amount exists; cost
//     derived from the float64 USD source as exact integer
//     micro-dollars, honestly estimated; latency in integer
//     nanoseconds).
//
// # The answer source (honest unavailable)
//
// (continued from the previous section:)
//
// No canonical event currently carries the final answer content (the
// runtime persists answers on the task record, which a projection
// never reads). The answer component therefore materializes as
// Unavailable, and a task.completed whose answer has not converged
// defers the complete seal: the row honestly stays MUTABLE (running)
// while its sources have not converged and the materializer retries
// the seal at the end of every pass, so a late-converging answer
// source seals the row without any manual rebuild. A complete row is
// never fabricated; a failed/cancelled seal needs no answer and
// converges immediately. The answer-union mapping itself is
// implemented and pinned; a future answer-carrying canonical event
// plugs into the same path.
//
// # Monotonic sequence, checkpoint, restart/catch-up, erasure
//
// Every observation carries the event's bus sequence, and the
// projector's row-level monotonic guards make an observation at or
// below a row's last-applied sequence a NO-OP (response-loss replay
// and out-of-order feeds never mutate a row, never bump a version, and
// never need a lucky expected version). The materializer advances the
// projector Store's per-session checkpoint monotonically after every
// applied event. After a process restart the materializer's in-memory
// state is empty, so the first pass re-pages the source from sequence
// zero: row mutations at or below the checkpoint are no-ops (cheap
// reads), the in-memory accumulators rebuild deterministically from
// the events, and the pass continues past the checkpoint — restart
// catch-up is idempotent and converging. Within one instance the
// session state also tracks the sequence of the last event fully
// incorporated into its accumulators, so a page retry after a
// mid-page failure never re-derives already-applied events (event
// application is transactional: the accumulators commit only after the
// durable write succeeds). A turn whose durable row was EVICTED past
// retention is an honest per-turn terminal projection gap: its routing
// state is retired and the pass keeps advancing without resurrecting
// it. An ERASED session is skipped permanently: the store-local
// durable erasure fence refuses every write (ErrErasureFenced), the
// durable event source itself excludes fenced sessions, and the
// optional runtime ErasureProbe (wired like the projector's own)
// refuses to re-materialize an erased session from sequence zero on a
// restarted in-memory store.
//
// # Source honesty
//
// A source retention gap (a wrapped ring evicting older events) is
// surfaced on the materialize Result, never silently treated as a
// complete stream — a projector can never mistake a truncated ring for
// a gapless log. A source without a retained substrate reports
// ProjectionUnavailable and the materializer fails loud
// (ErrSourceUnavailable) instead of serving a silent empty stream.
package materializer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// ErrSourceUnavailable — the projection source has no retained
// substrate (ProjectionUnavailable) or the source itself failed loud.
// The materializer cannot run on this deployment; it never silently
// treats the stream as empty.
var ErrSourceUnavailable = errors.New("materializer: projection source unavailable")

// defaultPageLimit is the materializer's default forward page size.
// Paging is bounded; Materialize loops until the source reports
// ProjectionCurrent.
const defaultPageLimit = 256

// Materializer is the compiled artifact that drives the turn
// projection from the persisted canonical event source.
//
// Concurrent reuse (the mandatory concurrent-reuse contract): a
// constructed *Materializer is immutable after New — it holds only the
// ProjectionSource, the *turns.Projector (both themselves safe for N
// concurrent goroutines), and immutable options. The per-session
// in-memory state is guarded by an internal mutex, so Materialize (and
// therefore Run) is safe to call from N goroutines; concurrent
// invocations serialize on the state, and concurrent readers of the
// projection (turns.Projector.List / Get) run against the store
// without touching the materializer at all.
type Materializer struct {
	src  events.ProjectionSource
	proj *turns.Projector
	// probe is the runtime's durable erasure authority consulted when a
	// session is first touched after a restart; nil means the runtime
	// declared none (an honest availability gap — see the projector's
	// own ErasureProbe contract).
	probe turns.ErasureProbe
	// pageLimit bounds one forward source page.
	pageLimit int

	mu sync.Mutex
	// cursor is the materializer's in-memory global forward cursor:
	// the sequence of the last event processed. A fresh instance starts
	// at 0 and re-pages the retained log from the beginning (restart
	// catch-up is idempotent; the projector's monotonic guards make
	// re-application a no-op).
	cursor uint64
	// sessions holds the per-session working state, keyed by the
	// isolation triple.
	sessions map[identity.Identity]*sessionState
	// passTouched counts sessions created or updated during the current
	// Materialize pass (guarded by mu, which Materialize holds for the
	// whole pass).
	passTouched int
}

// Option configures New.
type Option func(*Materializer)

// WithPageLimit bounds one forward source page (default 256). A
// non-positive value is ignored (the default stands).
func WithPageLimit(n int) Option {
	return func(m *Materializer) {
		if n > 0 {
			m.pageLimit = n
		}
	}
}

// WithErasureProbe wires the runtime's durable erasure authority the
// materializer consults when a session is first touched (the restart
// gate mirroring turns.Projector.WithErasureProbe): an erased session
// is never re-materialized from sequence zero merely because the
// in-memory store restarted. Runtimes with a durable erasure cascade
// MUST wire it; a nil probe is an honest availability gap.
func WithErasureProbe(pb turns.ErasureProbe) Option {
	return func(m *Materializer) { m.probe = pb }
}

// New constructs a Materializer over a mandatory source and projector.
// A nil source or projector fails loud at construction — never a
// nil-panicking materializer.
func New(src events.ProjectionSource, proj *turns.Projector, opts ...Option) (*Materializer, error) {
	if src == nil {
		return nil, fmt.Errorf("materializer: New requires a non-nil ProjectionSource")
	}
	if proj == nil {
		return nil, fmt.Errorf("materializer: New requires a non-nil *turns.Projector")
	}
	m := &Materializer{
		src:       src,
		proj:      proj,
		pageLimit: defaultPageLimit,
		sessions:  map[identity.Identity]*sessionState{},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Result reports one materialize pass.
type Result struct {
	// EventsApplied is the count of events routed to a session and
	// processed (state and possibly the projection).
	EventsApplied int
	// EventsSkipped is the count of events deliberately not processed
	// (unrecognised type, incomplete identity, unknown session, fenced
	// session, sealed turn, unroutable run).
	EventsSkipped int
	// SessionsTouched is the number of sessions whose state was
	// created or updated during the pass.
	SessionsTouched int
	// PendingComplete is the number of turns whose complete seal is
	// deferred because the answer source has not converged. Such rows
	// stay MUTABLE (running) and are retried on the next pass.
	PendingComplete int
	// FencedSessions is the number of sessions skipped because an
	// erasure fence is in force.
	FencedSessions int
	// Cursor is the materializer's forward cursor after the pass.
	Cursor uint64
	// Watermark is the source's high-water mark at pass end.
	Watermark uint64
	// Quality is the source quality at pass end (Current when the
	// materializer caught up).
	Quality events.ProjectionQuality
	// RetentionGap reports whether the source signalled that canonical
	// events between the cursor and the page head may be missing (a
	// wrapped ring). A projector must treat its history as potentially
	// incomplete, never as a silent gap-free stream.
	RetentionGap bool
}

// Materialize runs one forward catch-up pass: page the source from the
// materializer's cursor, apply every canonical event, and continue
// until the source reports ProjectionCurrent (or the caller's ctx
// cancels). Deferred complete seals are retried at the end of the
// pass. The pass is idempotent: a concurrent or restarted materializer
// re-applying the same events converges to identical rows.
//
// Hard failures abort the pass with a wrapped error (fail loud — a
// store failure or a source failure is never silently swallowed);
// per-event skips that are expected (erasure fences, sealed turns,
// unclassifiable steps) are counted, not raised.
func (m *Materializer) Materialize(ctx context.Context) (Result, error) {
	var res Result
	m.mu.Lock()
	defer m.mu.Unlock()
	m.passTouched = 0

	if err := ctx.Err(); err != nil {
		return res, err
	}

	for {
		page, err := m.src.Page(ctx, m.cursor, m.pageLimit)
		if err != nil {
			if errors.Is(err, events.ErrProjectionUnavailable) {
				return res, ErrSourceUnavailable
			}
			return res, fmt.Errorf("materializer: source page @%d: %w", m.cursor, err)
		}
		if page.Quality == events.ProjectionUnavailable {
			return res, ErrSourceUnavailable
		}
		res.Watermark = page.Watermark
		res.Quality = page.Quality
		if page.RetentionGap {
			res.RetentionGap = true
		}

		for _, ev := range page.Events {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			applied, skipped, err := m.applyEvent(ctx, ev)
			if err != nil {
				return res, err
			}
			if applied {
				res.EventsApplied++
			} else if skipped {
				res.EventsSkipped++
			}
		}
		m.cursor = page.Next

		if len(page.Events) > 0 {
			res.SessionsTouched = m.passTouched
		}
		if page.Quality == events.ProjectionCurrent {
			break
		}
		if len(page.Events) == 0 {
			// A catching-up page with no events is a source anomaly;
			// break rather than loop forever (the cursor did not
			// advance).
			break
		}
	}

	// Retry deferred complete seals now that the whole page has been
	// applied: a task.completed observed in this pass whose answer
	// source converged later in the same pass seals without waiting for
	// the next pass.
	for _, sess := range m.sessions {
		if sess.fenced {
			res.FencedSessions++
			continue
		}
		for _, ts := range sess.turns {
			if !ts.pendingComplete || ts.terminal() {
				continue
			}
			row, err := m.sealTurn(ctx, sess, ts, turns.Seal{
				Status:       turns.StatusComplete,
				FinishReason: turns.FinishGoal,
				EventSeq:     sess.checkpoint,
			})
			if err != nil {
				if errors.Is(err, turns.ErrSealIncomplete) {
					res.PendingComplete++
					continue
				}
				if errors.Is(err, turns.ErrTurnSealed) || errors.Is(err, turns.ErrTurnNotFound) {
					ts.pendingComplete = false
					continue
				}
				return res, fmt.Errorf("materializer: retry complete seal %s: %w", ts.taskID, err)
			}
			if ts.retired {
				// The row was evicted past retention: the deferred seal
				// is an honest terminal projection gap — retire the
				// routing state, never resurrect.
				ts.pendingComplete = false
				continue
			}
			if !row.Sealed {
				// The seal observation was a sequence no-op (its
				// sequence is at or below the row's last-applied
				// sequence): the DURABLE row is demonstrably NOT sealed.
				// Never equate a no-op with a successful seal — the
				// local state stays unsealed and the retry continues on
				// the next pass.
				res.PendingComplete++
				continue
			}
			ts.sealed = true
			ts.pendingComplete = false
		}
	}
	res.Cursor = m.cursor
	return res, nil
}

// Run drives the background materialization loop: catch up, then wait
// for the source's best-effort wake notifications (or its own poll
// fallback) and catch up again, until ctx is cancelled. The wake sink
// is caller-owned bounded and unsubscribed on exit so a dead
// materializer never accumulates dropped sends. Run returns nil on
// clean cancellation, ErrSourceUnavailable when the source cannot
// serve a projection, or the pass error otherwise.
func (m *Materializer) Run(ctx context.Context) error {
	wake := make(chan uint64, 8)
	watch, err := m.src.Watch(ctx, wake)
	if err != nil {
		if errors.Is(err, events.ErrProjectionUnavailable) {
			return ErrSourceUnavailable
		}
		return fmt.Errorf("materializer: watch source: %w", err)
	}
	defer watch.Unsubscribe()

	if _, err := m.Materialize(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case wm := <-wake:
			m.mu.Lock()
			behind := wm > m.cursor
			m.mu.Unlock()
			if !behind {
				continue
			}
			if _, err := m.Materialize(ctx); err != nil {
				return err
			}
		}
	}
}

// Cursor returns the materializer's current forward cursor (the
// sequence of the last processed event; 0 before any pass). Reads are
// safe concurrently with Materialize.
func (m *Materializer) Cursor() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursor
}

// Source returns the underlying projection source. Exposed so a
// runtime can discover the watermark independently of a pass.
func (m *Materializer) Source() events.ProjectionSource { return m.src }

// Projector returns the underlying turn projector. Exposed so a
// runtime can serve the read surface (sessions.turns.list / get) over
// the same projection the materializer writes.
func (m *Materializer) Projector() *turns.Projector { return m.proj }
