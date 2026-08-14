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
//     the source's best-effort wake notifications to avoid polling;
//     the optional bounded poll — WithPollInterval — is the lost-wake
//     safety net, never the fast path);
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
// # The answer source (honest unavailable, converging via the seam)
//
// (continued from the previous section:)
//
// No canonical EVENT carries the final answer content — the runtime
// persists answers on the task RECORD, which a projection never reads.
// The materializer therefore exposes the injected read-only
// TaskSnapshotReader seam (WithTaskSnapshotReader): while projecting a
// successfully persisted task.spawned / task.completed / task.failed,
// it reads the already-redacted canonical task record under the EVENT
// identity and converges the query / agent / input-attachment /
// answer / output-attachment / failure components from it. A runtime
// that wires the reader gets the bounded Harbor answer envelope
// (inline, empty, or artifact-reference shaped) onto a COMPLETE seal;
// a runtime that does not (or a legacy record that lacks the fields)
// keeps the honest Unavailable posture below. The seam is invoked only
// during event projection, never on Protocol reads, and never with a
// widened identity; a transient snapshot error fails the projection
// WITHOUT advancing the checkpoint. Every read is BOUND to the exact
// requested identity and event-derived task id: the record's nonempty
// TaskID must equal the requested task id (it can never replace the
// event's canonical task id), and its RunID must agree with the
// event-derived / already-established turn run — a snapshot may fill
// the run only when the event genuinely lacks one, and once a turn/run
// binding exists no later snapshot or event can move it. A binding
// mismatch fails the projection loudly without advancing the
// checkpoint or mutating a turn.
//
// Without a wired reader, the answer component materializes as
// Unavailable, and a task.completed whose answer has not converged
// defers the complete seal: the row honestly stays MUTABLE (running)
// while its sources have not converged. The deferred seal is NOT
// re-applied blindly — it is converged: every convergence pass
// REREADS the exact task snapshot under the original event identity
// and task id, re-runs the accepted TaskID / RunID and component
// agreement checks, attaches the newly available bounded answer /
// output / input data, and seals only after convergence, using the
// projector's explicit NO-NEW-EVENT semantics (EventSeq 0 preserves
// the row's LastAppliedEventSeq — convergence never fabricates a newer
// canonical event sequence). The deferred turns live in a BOUNDED
// pending-work queue served with a bounded per-pass budget in stable
// FIFO / round-robin order, so a late-converging answer source (a task
// record that was missing at completion and appeared later, or a
// record whose answer landed after the terminal event) seals the row
// without any manual rebuild and without a new event — the lost-wake
// poll serves the queue even when the source watermark is unchanged.
// After a durable restart, replaying a terminal event at or below the
// checkpoint reconstructs the deferred state ONLY after reading the
// existing exact turn and proving it is the same unsealed incomplete
// row (a sealed row is never touched, an evicted row is never
// resurrected, and ordinary history is not re-applied). A complete row
// is never fabricated; a failed/cancelled seal needs no answer and
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
	"time"

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

// defaultConvergenceBudget bounds how many deferred-complete turns ONE
// convergence pass attempts. A large pending queue is served in stable
// FIFO / round-robin order across passes — one pass never turns into a
// full scan of every pending turn, and a caught-up poll tick is one
// cheap bounded pass, never a busy loop.
const defaultConvergenceBudget = 64

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
	// snap is the injected READ-ONLY seam over the runtime's canonical
	// task records (see TaskSnapshotReader); nil means the runtime
	// declared none — an honest availability gap: the query / agent /
	// input / answer / output / failure-message components stay
	// unavailable and a complete seal defers.
	snap TaskSnapshotReader
	// pageLimit bounds one forward source page.
	pageLimit int
	// pollInterval is the bounded lost-wake recovery poll cadence (0 =
	// wake-only, the default). When set, Run re-checks the source
	// watermark on this interval so a dropped best-effort wake
	// notification converges without a restart; wake notifications stay
	// primary.
	pollInterval time.Duration
	// convergenceBudget bounds how many deferred-complete entries one
	// convergence pass attempts (see defaultConvergenceBudget).
	convergenceBudget int

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
	// pending is the BOUNDED deferred-complete work queue: one entry
	// per turn whose complete seal is deferred because its answer
	// source has not converged. Entries are served in stable FIFO /
	// round-robin order (an un-converged entry is re-enqueued at the
	// tail) with a bounded per-pass budget, so the queue converges
	// without any new canonical event and no single deferred seal
	// starves. Guarded by mu.
	pending []pendingWork
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

// WithPollInterval enables the bounded lost-wake recovery poll: when
// set (a strictly positive interval), Run re-checks the source
// watermark on that cadence so a DROPPED best-effort wake notification
// converges without a restart. Wake notifications stay PRIMARY — a
// wake triggers an immediate catch-up; the poll is the safety net,
// never the fast path, and it never fires while the loop is already
// catching up. Even when the source watermark is UNCHANGED, the poll
// still serves the bounded deferred-complete work queue: a pending
// turn's answer source (the task record) may have converged without
// any new canonical event. A non-positive value is ignored (wake-only,
// the default). The poll is bounded by the caller-chosen interval and
// is stopped on every Run exit path, so cancellation stops the watcher
// AND the timer with no goroutine leaks.
func WithPollInterval(d time.Duration) Option {
	return func(m *Materializer) {
		if d > 0 {
			m.pollInterval = d
		}
	}
}

// WithConvergenceBudget bounds how many deferred-complete turns ONE
// convergence pass attempts (default 64). A huge pending queue is
// served in stable FIFO / round-robin order across passes — one pass
// never turns into a full scan of every pending turn, and a caught-up
// poll tick is one cheap bounded pass, never a busy loop. A
// non-positive value is ignored (the default stands).
func WithConvergenceBudget(n int) Option {
	return func(m *Materializer) {
		if n > 0 {
			m.convergenceBudget = n
		}
	}
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
		src:               src,
		proj:              proj,
		pageLimit:         defaultPageLimit,
		convergenceBudget: defaultConvergenceBudget,
		sessions:          map[identity.Identity]*sessionState{},
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
	// stay MUTABLE (running) and are reread on the next convergence
	// pass: each pending turn rereads its exact task snapshot under the
	// original event identity / task id and seals once the answer
	// source converges — without any new canonical event.
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
// cancels). Deferred complete seals are converged at the end of the
// pass: each pending turn REREADS its exact task snapshot under the
// original event identity / task id, re-runs the accepted TaskID /
// RunID / component agreement checks, attaches newly available bounded
// data, and seals only after convergence (a bounded per-pass budget,
// stable FIFO / round-robin order). The pass is idempotent: a
// concurrent or restarted materializer re-applying the same events
// converges to identical rows.
//
// Hard failures abort the pass with a wrapped error (fail loud — a
// store failure, a transient snapshot error, or a binding mismatch is
// never silently swallowed); per-event skips that are expected
// (erasure fences, sealed turns, unclassifiable steps) are counted,
// not raised.
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

	// Converge deferred complete seals now that the whole page has been
	// applied: a completion observed in this pass (or reconstructed
	// during restart catch-up) whose answer source converged later in
	// the same pass seals without waiting for the next pass. Each
	// pending turn REREADS its exact task snapshot under the session
	// identity / task id, re-runs the identity/task/run and component
	// agreement checks, attaches newly available bounded data, and
	// seals only after convergence — a deferred seal whose answer
	// source is STILL unavailable stays honestly mutable (running) and
	// is reread on the next pass. The pass is bounded
	// (convergenceBudget entries) and served in stable FIFO /
	// round-robin order.
	if err := m.convergePending(ctx, &res); err != nil {
		return res, err
	}
	res.PendingComplete = len(m.pending)
	res.Cursor = m.cursor
	return res, nil
}

// Run drives the background materialization loop: catch up, then wait
// for the source's best-effort wake notifications (or, when
// WithPollInterval is set, its own bounded watermark poll) and catch up
// again, until ctx is cancelled. Wake notifications are the PRIMARY
// fast path; the poll is the bounded lost-wake safety net — a dropped
// best-effort wake (a full or absent sink) converges without a restart
// because the poll re-checks the source watermark on a bounded cadence.
// Even when the source watermark is UNCHANGED, the loop still serves
// the bounded deferred-complete work queue (the poll's
// no-new-event convergence; also checked on a spurious wake): a
// pending turn's answer source — the task record — may have converged
// without any canonical event, and the queue is served with a bounded
// per-pass budget in stable FIFO / round-robin order, never a full
// scan and never a busy loop. The wake sink is caller-owned bounded and
// unsubscribed on exit, and the poll timer is stopped on exit, so a
// cancelled Run stops the watcher AND the timer goroutines with no
// leaks. Run returns nil on clean cancellation, ErrSourceUnavailable
// when the source cannot serve a projection, or the pass error
// otherwise.
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

	// The bounded lost-wake recovery poll. A nil pollC never fires, so
	// the wake-only default is unchanged. The ticker is stopped on
	// every exit path (defer) — no timer goroutine outlives Run.
	var poll *time.Ticker
	var pollC <-chan time.Time
	if m.pollInterval > 0 {
		poll = time.NewTicker(m.pollInterval)
		pollC = poll.C
		defer poll.Stop()
	}

	// catchUp runs one forward pass. A cancellation that lands mid-pass
	// is a clean exit (nil), never reported as a pass failure.
	catchUp := func() error {
		_, err := m.Materialize(ctx)
		if err != nil && ctx.Err() != nil {
			return nil
		}
		return err
	}

	if err := catchUp(); err != nil {
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
				// A spurious wake (or a wake racing the poll): no new
				// events behind the cursor, but a deferred-complete
				// turn's answer source may have converged since the last
				// pass — serve the bounded pending-work queue. Wakes
				// stay the FAST PATH for new events; this is only the
				// no-new-event convergence.
				if err := m.convergeQueued(ctx); err != nil {
					return err
				}
				continue
			}
			if err := catchUp(); err != nil {
				return err
			}
		case <-pollC:
			// The poll tick: compare the source watermark against the
			// cursor and catch up only when genuinely behind — a
			// caught-up tick is one cheap watermark read, never a full
			// page. Even when the watermark is UNCHANGED the bounded
			// pending-work queue is still served: a deferred-complete
			// turn's answer source (the task record) may have converged
			// without any new canonical event, so the lost-wake poll
			// must converge it.
			wm, err := m.src.Watermark(ctx)
			if err != nil {
				if errors.Is(err, events.ErrProjectionUnavailable) {
					return ErrSourceUnavailable
				}
				return fmt.Errorf("materializer: poll source watermark: %w", err)
			}
			m.mu.Lock()
			behind := wm > m.cursor
			m.mu.Unlock()
			if !behind {
				if err := m.convergeQueued(ctx); err != nil {
					return err
				}
				continue
			}
			if err := catchUp(); err != nil {
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
