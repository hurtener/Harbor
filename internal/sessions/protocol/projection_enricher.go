package protocol

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// ProjectionEnricher is the projection-backed Enricher adapter: it serves
// the per-session counter rollup from the authoritative durable
// observability rollup projection (internal/observability/rollups) when
// that projection is CURRENT and its retained horizon COVERS the session's
// lifetime, and otherwise delegates explicitly to the existing raw
// CounterEnricher bounded scan — the honest fallback. It composes the
// projection for exactly the dimensions the projection is authoritative
// for (successful-completion EVENTS, COST, TOKENS, and TASK OUTCOMES) and
// reads the one dimension the projection does not model (pending
// intervention) from the pause registry, mirroring the raw enricher's
// read so a missing pause read is surfaced as Partial, never as a
// fabricated "no intervention".
//
// # When the projection is current and covers the session
//
// The rollup projection is an indexed materialization of the canonical
// event log's supported measures. When its completeness state is `current`
// (the last source read was empty — nothing newer than the watermark
// existed at that read) and its retained horizon starts at or before the
// session's opening bucket, the projection has observed every supported
// measure the session emitted: the adapter returns EXACT counters — no
// `CountersPartial`, no read-time event scan. The mapping is the
// projection's source-backed measures only:
//
//   - EventsCount      ← llm_completions  (the session's `llm.cost.recorded`
//     successful-completion events — the projection's event measure).
//   - TotalCostCents   ← llm_cost_micros  (exact integer micro-units of USD,
//     converted to the existing whole-cent wire representation with ONE
//     deterministic rounding at the end — sub-cent calls never floor to 0).
//   - TotalTokens      ← llm_tokens_total.
//   - TasksCount       ← tasks_completed + tasks_failed + tasks_cancelled
//     (task OUTCOMES — the projection counts terminal transitions).
//   - HasFailedTask    ← tasks_failed > 0.
//
// The projection does NOT model the pause registry, so
// HasPendingIntervention is read from the pause coordinator exactly as the
// raw enricher reads it; a read that cannot be taken marks the rollup
// Partial (a zero that means "we could not look" is never reported as a
// zero that means "we looked and there were none").
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
// accessor.
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
// pause-coordinator / window / clock / logger references, each itself safe
// for concurrent reuse. Every Counters call's per-run state lives in its
// arguments and locals; the adapter reads nothing from itself for
// run-specific data.
type ProjectionEnricher struct {
	store    rollups.Store
	quality  ProjectionQuality
	fallback Enricher
	pauses   pauseresume.Coordinator
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
	// session's authoritative measures from (identity-scoped by the query
	// filter — the session's own triple, never cross-session bleed).
	Store rollups.Store
	// Quality reads the projection's freshness (state / watermark /
	// retention). The adapter serves exact projection-backed counters only
	// when Quality reports StateCurrent AND the retained horizon covers the
	// session.
	Quality ProjectionQuality
	// Fallback is the existing raw CounterEnricher bounded scan the adapter
	// delegates to whenever the projection cannot be trusted for the
	// session. Its result is returned verbatim — including its own honest
	// Partial marking — never replaced by fabricated zeros.
	Fallback Enricher
	// Pauses is the pause coordinator the has_pending_intervention counter
	// reads from, scoped to the session (the projection does not model
	// pauses; a read that cannot be taken marks the rollup Partial).
	Pauses pauseresume.Coordinator
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
// dependency is mandatory — a nil Store / Quality / Fallback / Pauses /
// Window fails loud with ErrMisconfigured rather than building an adapter
// that reports believable-but-false counters on one dimension (CLAUDE.md
// §5). The returned *ProjectionEnricher is immutable and safe for
// concurrent reuse.
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
	if deps.Pauses == nil {
		return nil, fmt.Errorf("%w: ProjectionEnricher Pauses is nil", ErrMisconfigured)
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
		pauses:   deps.Pauses,
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
// the projection when the projection is current and covers the session, and
// otherwise delegates to the raw bounded scan. It never turns missing
// projection data into an exact zero.
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

	counters, err := e.projectionCounters(ctx, id, openedAt)
	if err != nil {
		// A projection query failure is an unavailable projection: the raw
		// scan is the honest fallback, never a partial zero.
		return e.delegateToFallback(ctx, id, "rollup projection query failed", err)
	}

	// The projection does not model the pause registry: read
	// has_pending_intervention from the coordinator, mirroring the raw
	// enricher's session-scoped read. A read that cannot be taken (an
	// unentitled foreign row) marks the rollup Partial — a zero that means
	// "we could not look" is never reported as "we looked and there were
	// none".
	pending, read := e.pendingIntervention(ctx, id)
	counters.HasPendingIntervention = pending
	counters.Partial = !read
	return counters
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
// the projection-backed counter dimensions. The window ends at the
// adapter's "now" (a `current` projection is caught up to the log head, so
// every supported event up to now is reflected). The bucket size is chosen
// adaptively — minute, hour, day — so a multi-week session fits the
// query's MaxBuckets budget while day coarsening keeps the SUM exact
// (coarsening never changes the totals). A session with no projection rows
// returns measured zeros (the projection observed none of the session's
// supported measures) — Partial stays false, so the zeros are exact.
func (e *ProjectionEnricher) projectionCounters(ctx context.Context, id identity.Identity, openedAt time.Time) (SessionCounters, error) {
	now := e.clock()
	size, from, to, err := sessionRollupWindow(openedAt, now)
	if err != nil {
		return SessionCounters{}, err
	}

	var (
		completions, costMicros, tokens int64
		tasksDone, tasksFailed          int64
		tasksCancelled                  int64
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
			// successful-completion events, cost, total tokens, and task
			// outcomes. No measure the projection does not back is read.
			Measures: []rollups.Measure{
				rollups.MeasureLLMCompletions,
				rollups.MeasureLLMCostMicros,
				rollups.MeasureLLMTokensTotal,
				rollups.MeasureTasksCompleted,
				rollups.MeasureTasksFailed,
				rollups.MeasureTasksCancelled,
			},
			Sort:   rollups.SortKeyBucketAsc,
			Limit:  rollups.MaxRowsPerQuery,
			Cursor: cursor,
		})
		if err != nil {
			return SessionCounters{}, fmt.Errorf("sessions/protocol: projection counter query: %w", err)
		}
		for _, r := range res.Rows {
			completions += r.Measures[rollups.MeasureLLMCompletions].N
			costMicros += r.Measures[rollups.MeasureLLMCostMicros].N
			tokens += r.Measures[rollups.MeasureLLMTokensTotal].N
			tasksDone += r.Measures[rollups.MeasureTasksCompleted].N
			tasksFailed += r.Measures[rollups.MeasureTasksFailed].N
			tasksCancelled += r.Measures[rollups.MeasureTasksCancelled].N
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
		EventsCount:    int(completions),
		TotalCostCents: microsToCents(costMicros),
		TotalTokens:    tokens,
		TasksCount:     int(tasksDone + tasksFailed + tasksCancelled),
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

// pendingIntervention reports whether the session has a paused pause record
// awaiting resume / approval, mirroring the raw enricher's session-scoped
// pause read. A read that cannot be taken (an unentitled foreign row, a
// coordinator failure) returns read=false — the caller must mark the rollup
// Partial, never report a fabricated "no intervention".
func (e *ProjectionEnricher) pendingIntervention(ctx context.Context, id identity.Identity) (pending bool, read bool) {
	idCtx, err := e.rowScopedCtx(ctx, id)
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: projection enricher intervention row scope refused",
			slog.String("session_id", id.SessionID), slog.Any("error", err))
		return false, false
	}
	resp, err := e.pauses.List(idCtx, pauseresume.ListRequest{
		Identity: id,
		Filter: pauseresume.ListFilter{
			States: []pauseresume.State{pauseresume.StatusPaused},
			// Scope by the full triple (tenant is implied by Identity;
			// UserIDs + SessionIDs mirror the events / tasks reads and
			// CLAUDE.md §6 defence-in-depth — never scope by session alone).
			UserIDs:    []string{id.UserID},
			SessionIDs: []string{id.SessionID},
		},
		Page:     1,
		PageSize: 1,
	})
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: projection enricher intervention list failed",
			slog.String("session_id", id.SessionID), slog.Any("error", err))
		return false, false
	}
	return resp.TotalRows > 0, true
}

// rowScopedCtx seats the ROW's own identity for the row-scoped pause read.
//
// It mirrors CounterEnricher.rowScopedCtx (enricher.go) exactly: a row
// inside the caller's verified tenant is ordinary narrowing; a row from
// ANOTHER tenant is a crossing the adapter re-checks against the admin-tier
// claim the fleet listing gated on, seating it as an audited re-scope so
// the crossing stays visible instead of being an unexplained widening. The
// rollup store read itself needs no elevation — its query filter names the
// session triple explicitly — but the pause coordinator scopes reads by the
// ctx identity, so the crossing is minted here.
func (e *ProjectionEnricher) rowScopedCtx(ctx context.Context, id identity.Identity) (context.Context, error) {
	verified, anchored := identity.FromVerified(ctx)
	if !anchored || id.TenantID == verified.TenantID {
		return identity.With(ctx, id)
	}
	if !auth.HasScope(ctx, auth.ScopeAdmin) && !auth.HasScope(ctx, auth.ScopeConsoleFleet) {
		return nil, fmt.Errorf("sessions projection enricher intervention: %w", errRowScopeUnentitled)
	}
	return identity.WithElevated(ctx, id,
		"sessions projection enricher intervention: reading a row returned by an authorized fleet listing under that row's own identity")
}

// Compile-time assertion: *ProjectionEnricher satisfies Enricher.
var _ Enricher = (*ProjectionEnricher)(nil)
