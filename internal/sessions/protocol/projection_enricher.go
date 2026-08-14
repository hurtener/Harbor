package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// ProjectionEnricher is the projection-backed Enricher adapter: it serves
// the per-session counter rollup from the authoritative durable
// observability rollup projection (internal/observability/rollups) when
// that projection is CURRENT and its retained horizon COVERS the session's
// lifetime, and otherwise delegates explicitly to the existing raw
// CounterEnricher bounded scan — the honest fallback. The projection is
// used for EXACTLY the dimensions the projection is authoritative for
// (COST, TOKENS, and FAILED TASK OUTCOMES); the dimensions the projection
// does not model (total events emitted, total tasks spawned, and pending
// intervention) are read from the raw bounded scan, and the two are merged
// deterministically.
//
// # What the projection backs — and what it does NOT
//
// The rollup projection is an indexed materialization of the canonical
// event log's supported measures. Its measures are SOURCE-BACKED deltas,
// and the adapter never maps a measure onto a public counter whose meaning
// is broader than the measure's:
//
//   - TotalCostCents   ← llm_cost_micros  (exact integer micro-units of USD,
//     converted to the existing whole-cent wire representation with ONE
//     deterministic rounding at the end — sub-cent calls never floor to 0).
//   - TotalTokens      ← llm_tokens_total.
//   - HasFailedTask    ← tasks_failed > 0 (failed TERMINAL OUTCOMES).
//
// The projection does NOT back events_count or tasks_count:
//
//   - llm_completions is the session's `llm.cost.recorded`
//     successful-completion count — a SUBSET of the events the session
//     emitted (task / pause / artifact / lifecycle events are not
//     completions), so presenting it as total events_count would be a
//     believable-but-false undercount.
//   - tasks_completed + tasks_failed + tasks_cancelled counts terminal
//     OUTCOMES, not the tasks the session SPAWNED (running / paused tasks
//     never produce a terminal outcome), so presenting it as total
//     tasks_count would be a believable-but-false undercount too.
//
// The canonical totals for those two counters — total events emitted and
// total spawned tasks — plus has_pending_intervention (the projection does
// not model the pause registry) are read through the existing raw bounded
// CounterEnricher fallback seam on EVERY projection-backed path. The raw
// scan's Partial flag rides along: a truncated event scan, an unreadable
// registry read, or an unreadable pause read makes the aggregate
// CounterStatus=partial, never current.
//
// # Deterministic merge — projection owns its three, raw owns the rest
//
// When the projection is current and covers the session, the adapter
// merges the two sources WITHOUT letting either overwrite the other's
// authoritative dimension: the projection's EXACT cost / tokens /
// failed-task values are never replaced by the raw scan's lower bounds,
// and the raw scan's canonical events / tasks / pending values are never
// replaced by a projection subset. The aggregate Partial is the raw
// scan's Partial — a partial raw dimension makes the whole rollup
// partial (its events / tasks / pending are honest lower bounds), and the
// projection-backed exact values ride along, never fabricated as current
// over an incomplete raw read.
//
// # Honest fallback — never missing data as exact zero
//
// When the projection cannot be trusted for the session — the quality read
// fails, the state is `catching_up` / `unavailable`, the session window
// cannot be resolved, the retained horizon starts after the session opened
// (a retention gap), or the projection query itself fails — the adapter
// delegates to the raw CounterEnricher bounded scan and returns that
// rollup VERBATIM (its own Partial marking rides along: a truncated scan or
// an unreadable registry read stays an honest lower bound). The fallback is
// never silent: every delegation is Warn-logged with the reason, and the
// projection's freshness stays observable through the adapter's Quality
// accessor. If the raw fallback cannot provide a trustworthy result under
// its own contract (an unavailable substrate, an unreadable registry), its
// honest zero-plus-Partial result is preserved — availability is never
// fabricated.
//
// # Freshness is observable
//
// "Current" is current-as-of-the-last-empty-read: a live runtime may
// persist an event a moment after that read (the projection moves to
// `catching_up` on its next advance), the same best-effort read-at-a-moment
// honesty the raw scan has. The adapter exposes the projection's Quality
// (state / watermark / retention) through Quality so wiring and operators
// can observe how fresh the served counters are.
//
// # Concurrent reuse (CLAUDE.md §5)
//
// A constructed *ProjectionEnricher is immutable after
// NewProjectionEnricher: it holds only the store / quality / fallback /
// window / clock / logger references, each itself safe for concurrent
// reuse. Every Counters call's per-run state lives in its
// arguments and locals; the adapter reads nothing from itself for
// run-specific data.
type ProjectionEnricher struct {
	store    rollups.Store
	quality  ProjectionQuality
	fallback Enricher
	window   SessionWindowFunc
	clock    func() time.Time
	logger   *slog.Logger
}

// ProjectionQuality reads the rollup projection's operational freshness —
// the read-only surface the rollup projector exposes (Quality on the
// projector: completeness state, watermark, retained horizon). Wired to the
// production projector in assembly; tests supply a controllable fake.
type ProjectionQuality interface {
	Quality(ctx context.Context) (rollups.Quality, error)
}

// SessionWindowFunc resolves a session's lifetime window so the adapter can
// prove the projection's retained horizon covers the session before
// trusting the rollup as exact. Production wiring supplies a
// session-registry-backed resolver (the snapshot's OpenedAt / LastSeen);
// tests supply a fake. ok=false means the window could not be resolved —
// the adapter then CANNOT prove coverage and delegates to the raw fallback
// rather than guessing (unproven coverage is never treated as exact).
type SessionWindowFunc func(ctx context.Context, id identity.Identity, sessionID string) (openedAt, lastActivityAt time.Time, ok bool, err error)

// ProjectionEnricherDeps carries the ProjectionEnricher's mandatory
// dependencies. Every dependency is required — a nil one would silently
// disable a dimension or the coverage proof (the exact silent-absence class
// this adapter closes), so NewProjectionEnricher fails loud rather than
// build a half-blind adapter (CLAUDE.md §5).
type ProjectionEnricherDeps struct {
	// Store is the rollup projection's query surface the adapter reads the
	// session's authoritative cost / tokens / failed-task measures from
	// (identity-scoped by the query filter — the session's own triple,
	// never cross-session bleed).
	Store rollups.Store
	// Quality reads the projection's freshness (state / watermark /
	// retention). The adapter serves exact projection-backed counters only
	// when Quality reports StateCurrent AND the retained horizon covers the
	// session.
	Quality ProjectionQuality
	// Fallback is the existing raw CounterEnricher bounded scan. It is the
	// canonical source of the dimensions the projection does not model —
	// total events emitted, total spawned tasks, and pending intervention —
	// on the projection-backed path, and the adapter delegates to it
	// VERBATIM whenever the projection cannot be trusted for the session.
	// Its result's own honest Partial marking rides along in both cases —
	// never replaced by fabricated zeros or fabricated availability.
	Fallback Enricher
	// Window resolves the session's lifetime for the retention-coverage
	// proof.
	Window SessionWindowFunc
	// Clock supplies the window's "now". Nil routes to time.Now().UTC().
	Clock func() time.Time
	// Logger receives Warn-level diagnostics when the adapter falls back or
	// a read cannot be taken (the degradation is NEVER silent). Nil routes
	// to slog.Default().
	Logger *slog.Logger
}

// NewProjectionEnricher builds the projection-backed Enricher adapter. Every
// dependency is mandatory — a nil Store / Quality / Fallback / Window
// fails loud with ErrMisconfigured rather than building an adapter that
// reports believable-but-false counters on one dimension (CLAUDE.md §5).
// The returned *ProjectionEnricher is immutable and safe for concurrent
// reuse.
func NewProjectionEnricher(deps ProjectionEnricherDeps) (*ProjectionEnricher, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: ProjectionEnricher Store is nil", ErrMisconfigured)
	}
	if deps.Quality == nil {
		return nil, fmt.Errorf("%w: ProjectionEnricher Quality is nil", ErrMisconfigured)
	}
	if deps.Fallback == nil {
		return nil, fmt.Errorf("%w: ProjectionEnricher Fallback is nil", ErrMisconfigured)
	}
	if deps.Window == nil {
		return nil, fmt.Errorf("%w: ProjectionEnricher Window is nil", ErrMisconfigured)
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := deps.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &ProjectionEnricher{
		store:    deps.Store,
		quality:  deps.Quality,
		fallback: deps.Fallback,
		window:   deps.Window,
		clock:    clock,
		logger:   logger,
	}, nil
}

// Quality exposes the rollup projection's operational freshness — the
// completeness state (`current` / `catching_up` / `unavailable`), the
// watermark, and the retained horizon — so the freshness of the counters
// the adapter serves is observable (a catching_up / unavailable projection
// is exactly when the adapter delegates to the raw fallback). Read-only and
// safe for concurrent use.
func (e *ProjectionEnricher) Quality(ctx context.Context) (rollups.Quality, error) {
	return e.quality.Quality(ctx)
}

// Counters implements Enricher. It serves the session's counter rollup from
// the projection when the projection is current and covers the session
// (projection-backed cost / tokens / failed-task merged with the raw scan's
// canonical events / tasks / pending), and otherwise delegates to the raw
// bounded scan. It never turns missing projection data into an exact zero
// and never maps a projection subset onto a broader public counter.
func (e *ProjectionEnricher) Counters(ctx context.Context, id identity.Identity, sessionID string) SessionCounters {
	if sessionID != "" {
		id.SessionID = sessionID
	}

	// The projection must PROVE current before any projection-backed value
	// is served: a quality read failure or a catching_up / unavailable
	// state means the projection may trail the log — delegating to the raw
	// scan is the honest answer, never a projection-backed number that
	// could silently undercount.
	q, err := e.quality.Quality(ctx)
	if err != nil {
		return e.delegateToFallback(ctx, id, "rollup projection quality read failed", err)
	}
	if q.State != rollups.StateCurrent {
		return e.delegateToFallback(ctx, id, "rollup projection is not current ("+string(q.State)+")", q.Err)
	}

	// The session's lifetime must be resolvable AND inside the retained
	// horizon: an unresolvable window cannot prove coverage, and a
	// retention gap (the session opened before the oldest retained bucket)
	// means early projection-covered events may be missing — both delegate
	// to the raw scan rather than report a projection-backed undercount as
	// exact.
	openedAt, _, ok, err := e.window(ctx, id, sessionID)
	if err != nil || !ok || openedAt.IsZero() {
		return e.delegateToFallback(ctx, id, "session window unresolvable", err)
	}
	if !q.RetentionStart.IsZero() &&
		rollups.BucketStart(openedAt, rollups.BucketMinute).Before(q.RetentionStart) {
		return e.delegateToFallback(ctx, id, "rollup projection retention gap (session predates the retained horizon)", nil)
	}

	projection, err := e.projectionCounters(ctx, id, openedAt)
	if err != nil {
		// A projection query failure is an unavailable projection: the raw
		// scan is the honest fallback, never a partial zero.
		return e.delegateToFallback(ctx, id, "rollup projection query failed", err)
	}

	// The dimensions the projection does not model — total events emitted,
	// total spawned tasks, and pending intervention — come from the raw
	// bounded scan, the same canonical seam the delegation paths use. Its
	// Partial flag rides along: a truncated / unreadable raw dimension
	// makes the aggregate CounterStatus=partial, never current.
	raw := e.fallback.Counters(ctx, id, id.SessionID)

	// Merge deterministically: the projection owns the three exact outputs
	// it authoritatively backs (cost, tokens, failed-task) and the raw scan
	// owns events / tasks / pending. The projection's exact values are
	// NEVER overwritten by the raw lower bounds, and the raw's canonical
	// values are never replaced by a projection subset. The aggregate is
	// partial exactly when the raw result is partial.
	projection.EventsCount = raw.EventsCount
	projection.TasksCount = raw.TasksCount
	projection.HasPendingIntervention = raw.HasPendingIntervention
	projection.Partial = raw.Partial
	return projection
}

// delegateToFallback delegates to the raw bounded-scan enricher, logging the
// reason (the degradation is NEVER silent). The raw result is returned
// verbatim: its own Partial marking (a truncated scan, an unreadable
// registry read) is the honest representation of the fallback's
// completeness.
func (e *ProjectionEnricher) delegateToFallback(ctx context.Context, id identity.Identity, reason string, err error) SessionCounters {
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: projection enricher delegating to the raw bounded scan",
			slog.String("session_id", id.SessionID), slog.String("reason", reason), slog.Any("error", err))
	} else {
		e.logger.WarnContext(ctx, "sessions/protocol: projection enricher delegating to the raw bounded scan",
			slog.String("session_id", id.SessionID), slog.String("reason", reason))
	}
	return e.fallback.Counters(ctx, id, id.SessionID)
}

// centScaleMicros is the number of exact cost micro-units in one whole US
// cent — derived from the rollup domain's fixed micro scale so the two
// scales can never drift apart (1 USD = rollups.CostScaleMicros micros =
// 100 cents).
const centScaleMicros = int64(rollups.CostScaleMicros / 100)

// maxProjectionQueryPages bounds the projection-query pagination loop so a
// pathological driver (one that returns a next cursor forever) fails loud
// instead of looping. A validated query spans at most rollups.MaxBuckets
// buckets and one page holds rollups.MaxRowsPerQuery rows, so the shipped
// drivers always exhaust in one page; the bound is defence in depth.
const maxProjectionQueryPages = 8

// projectionCounters runs the authoritative session-scoped rollup query over
// the session's lifetime window and sums the exact integer measures into
// the projection-backed counter dimensions — EXACTLY the three the
// projection authoritatively backs: cost (micros, converted to cents once
// at the end), total tokens, and HasFailedTask (from failed terminal
// outcomes). Events count and spawned-task count are deliberately NOT read
// here: llm_completions is a subset of emitted events and terminal outcomes
// are not spawned tasks, so both would be believable-but-false undercounts
// (the raw bounded scan supplies the canonical totals). The window ends at
// the adapter's "now" (a `current` projection is caught up to the log head,
// so every supported event up to now is reflected). The bucket size is
// chosen adaptively — minute, hour, day — so a multi-week session fits the
// query's MaxBuckets budget while day coarsening keeps the SUM exact
// (coarsening never changes the totals). A session with no projection rows
// returns measured zeros (the projection observed none of the session's
// supported measures) — the raw scan's Partial marking, not this query,
// decides the aggregate's partiality.
func (e *ProjectionEnricher) projectionCounters(ctx context.Context, id identity.Identity, openedAt time.Time) (SessionCounters, error) {
	now := e.clock()
	size, from, to, err := sessionRollupWindow(openedAt, now)
	if err != nil {
		return SessionCounters{}, err
	}

	var (
		costMicros, tokens int64
		tasksFailed        int64
	)
	cursor := ""
	for page := 0; ; page++ {
		res, err := e.store.Query(ctx, rollups.Query{
			From:   from,
			To:     to,
			Bucket: size,
			// Scope by the session's OWN full triple — never a broader
			// window, never cross-session bleed. Models are deliberately
			// NOT restricted: the session's rollup spans every model it
			// used.
			Filter: rollups.Filter{
				TenantIDs:  []string{id.TenantID},
				UserIDs:    []string{id.UserID},
				SessionIDs: []string{id.SessionID},
			},
			// The projection-backed dimensions' source measures only:
			// cost, total tokens, and failed task outcomes. No measure the
			// projection does not back is read — in particular neither
			// llm_completions (a completion is not the session's total
			// events) nor tasks_completed / tasks_cancelled (a terminal
			// outcome is not a spawned task).
			Measures: []rollups.Measure{
				rollups.MeasureLLMCostMicros,
				rollups.MeasureLLMTokensTotal,
				rollups.MeasureTasksFailed,
			},
			Sort:   rollups.SortKeyBucketAsc,
			Limit:  rollups.MaxRowsPerQuery,
			Cursor: cursor,
		})
		if err != nil {
			return SessionCounters{}, fmt.Errorf("sessions/protocol: projection counter query: %w", err)
		}
		for _, r := range res.Rows {
			costMicros += r.Measures[rollups.MeasureLLMCostMicros].N
			tokens += r.Measures[rollups.MeasureLLMTokensTotal].N
			tasksFailed += r.Measures[rollups.MeasureTasksFailed].N
		}
		if res.NextCursor == "" {
			break
		}
		if page >= maxProjectionQueryPages-1 {
			return SessionCounters{}, fmt.Errorf("sessions/protocol: projection counter query exceeded %d pages", maxProjectionQueryPages)
		}
		cursor = res.NextCursor
	}

	return SessionCounters{
		TotalCostCents: microsToCents(costMicros),
		TotalTokens:    tokens,
		HasFailedTask:  tasksFailed > 0,
	}, nil
}

// sessionRollupWindow returns the bucket size and the aligned half-open
// window [from, to) that covers the session's lifetime [openedAt, now] on
// that size's fixed UTC grid. The size is the FINEST closed bucket whose
// bucket count fits the query budget: minute while the span fits
// rollups.MaxBuckets minutes, then hour, then day (which covers any
// realistic session lifetime). Coarsening never changes the summed totals —
// it only changes how many bucket rows the query returns — so a long-lived
// session stays queryable. An empty or reversed window (a session opened at
// or after the projection's "now" — clock skew or a corrupted record) fails
// loud rather than querying a nonsense window.
func sessionRollupWindow(openedAt, now time.Time) (rollups.BucketSize, time.Time, time.Time, error) {
	fromMinute := rollups.BucketStart(openedAt, rollups.BucketMinute)
	toMinute := rollups.BucketStart(now, rollups.BucketMinute).Add(time.Minute)
	if !toMinute.After(fromMinute) {
		return "", time.Time{}, time.Time{}, fmt.Errorf("sessions/protocol: session window [%s, %s) is empty or reversed (clock skew or a corrupted session record)",
			fromMinute.Format(time.RFC3339), toMinute.Format(time.RFC3339))
	}
	for _, size := range [...]rollups.BucketSize{rollups.BucketMinute, rollups.BucketHour, rollups.BucketDay} {
		from := rollups.BucketStart(openedAt, size)
		to := rollups.BucketStart(now, size).Add(size.Duration())
		if buckets := int(to.Sub(from) / size.Duration()); buckets <= rollups.MaxBuckets {
			return size, from, to, nil
		}
	}
	return "", time.Time{}, time.Time{}, fmt.Errorf("sessions/protocol: session window [%s, %s) exceeds the rollup query budget at every bucket size",
		fromMinute.Format(time.RFC3339), toMinute.Format(time.RFC3339))
}

// microsToCents converts the projection's exact integer cost micro-units
// (1 USD = 1,000,000 micros) to the existing whole-US-cents wire
// representation (1 USD = 100 cents), deterministically: ONE round-half-up
// rounding on the exact integer micro total. The rollup sums exact micros
// before this single conversion, so sub-cent per-call costs never floor to
// zero (the false-absence the raw enricher also closes) and the cent total
// is the honest round of the exact spend. Micros are non-negative in the
// rollup domain; a non-positive total converts to zero.
func microsToCents(micros int64) int64 {
	if micros <= 0 {
		return 0
	}
	return (micros + centScaleMicros/2) / centScaleMicros
}

// Compile-time assertion: *ProjectionEnricher satisfies Enricher.
var _ Enricher = (*ProjectionEnricher)(nil)
