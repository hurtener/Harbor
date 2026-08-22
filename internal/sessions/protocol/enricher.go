package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Enricher is the optional read-time counter-rollup backend the
// ListerProjector overlays onto a SessionRow. It mirrors the shipped
// tasks.Projector enricher SEAM — only the seam is inherited; the
// aggregation below is net-new (the tasks serve enricher returns a ZERO
// cost rollup, deferring cost to the event stream). Production wiring
// supplies a CounterEnricher backed by the event substrate + task registry
// + pause registry; tests and partial builds run without one.
//
// A projector with no Enricher wired reports honest ZERO counters — "we
// don't have this data", not a silent degradation of a known value — and
// the Service loud-rejects a facet/sort over those counters (WARN-3, see
// protocol.go) so an unwired build can never reproduce the false-absence
// defect this phase closes.
type Enricher interface {
	// Counters returns the read-time counter rollup for one session,
	// aggregated from the cost-event stream, the task registry, the event
	// substrate, and the pause registry — all identity-scoped to `id` (the
	// session's own full triple). A zero-valued return is honest ("we don't
	// have this data"), never silent degradation. When the bounded
	// per-session scan hits its bound (or a retention gap), the returned
	// SessionCounters.Partial is set and the cost / tokens / events counts
	// are an HONEST LOWER BOUND, never a plausible exact number.
	Counters(ctx context.Context, id identity.Identity, sessionID string) SessionCounters
}

// SessionCounters is the read-time counter rollup for one session — the
// six false-absence SessionRow counters plus the honest-partial marker.
type SessionCounters struct {
	// TasksCount is the number of tasks the session has spawned.
	TasksCount int
	// EventsCount is the number of events the session has emitted (a lower
	// bound when Partial is set).
	EventsCount int
	// TotalCostCents is the session's accumulated LLM cost in US cents (a
	// lower bound when Partial is set).
	TotalCostCents int64
	// TotalTokens is the session's accumulated LLM token count (a lower
	// bound when Partial is set).
	TotalTokens int64
	// HasPendingIntervention reports whether the session has a pause
	// awaiting resume / approval.
	HasPendingIntervention bool
	// HasFailedTask reports whether the session has at least one failed
	// task.
	HasFailedTask bool
	// Partial is true when ANY of the reads behind this rollup could not
	// be taken in full: the bounded per-session event scan hit its bound
	// (ListWindow HasMore / Truncated), the windowed-read substrate was
	// unavailable, or a registry read (tasks / pauses) failed or could not
	// be scoped to the row. Every populated count is then an HONEST LOWER
	// BOUND, not exact, and the facet/sort layer MUST NOT treat a Partial
	// key as authoritative.
	//
	// The marker covers the registry reads deliberately: a failed
	// registry read leaves TasksCount / HasFailedTask /
	// HasPendingIntervention at their zero values, and a zero that means
	// "we could not look" must never be reported as a zero that means
	// "we looked and there were none".
	Partial bool
}

// perSessionScanBound is the named upper bound on the per-session event
// scan the CounterEnricher performs against HistoryReplayer.ListWindow.
// A single session's event count is bounded by its own lifetime (unlike a
// fleet-wide aggregate), so hitting this bound is practically rare — and
// when it happens the enricher surfaces SessionCounters.Partial (an honest
// lower bound) rather than a silently-undercounted exact number. It is
// deliberately far below the fleet-aggregate bound: the rollup runs once
// per visible row (<= page limit, default 50 / max 200), so a smaller
// per-row bound keeps the read cheap while staying well above a normal
// session's lifetime event count.
const perSessionScanBound = 10_000

// CounterEnricher is the V1 production Enricher. It aggregates the
// per-session counters from raw data owned by subsystems one package over —
// SUMMING, not reading a shadow store:
//
//   - total_cost_cents / total_tokens: summed from `llm.cost.recorded`
//     events scoped to the session (via the event substrate).
//   - events_count: the count of the session's events from the durable
//     event substrate (HistoryReplayer.ListWindow, bounded).
//   - tasks_count / has_failed_task: the session's tasks from the task
//     registry.
//   - has_pending_intervention: a paused pause record from the pause
//     registry scoped to the session.
//
// # Concurrent reuse (CLAUDE.md §5)
//
// A constructed *CounterEnricher is immutable after NewCounterEnricher: it
// holds only the bus / registry / coordinator references (each itself safe
// for concurrent reuse) plus a logger. Every Counters call's per-run state
// lives in its arguments and locals; the enricher reads nothing from
// itself for run-specific data.
type CounterEnricher struct {
	bus       events.EventBus
	tasks     tasks.TaskRegistry
	pauses    pauseresume.Coordinator
	logger    *slog.Logger
	scanBound int
}

// CounterEnricherDeps carries the CounterEnricher's mandatory dependencies.
// All three are required — a nil dependency would silently drop a counter
// dimension (a value that reads zero on a busy session), the exact
// silent-absence class this phase closes, so NewCounterEnricher fails loud
// rather than build a half-blind enricher (CLAUDE.md §5).
type CounterEnricherDeps struct {
	// Bus is the event substrate the cost / tokens / events counters read
	// from. It MUST implement events.HistoryReplayer to serve the windowed
	// per-session scan; a bus without it yields honest-partial counters
	// (Partial=true, zero bus-derived counts), never a silent zero.
	Bus events.EventBus
	// Tasks is the task registry the tasks_count / has_failed_task counters
	// read from, scoped to the session.
	Tasks tasks.TaskRegistry
	// Pauses is the pause coordinator the has_pending_intervention counter
	// reads from, scoped to the session.
	Pauses pauseresume.Coordinator
	// Logger receives Warn-level diagnostics when a bounded scan truncates
	// or the substrate is unavailable (the degradation is NEVER silent —
	// it is both logged and surfaced as SessionCounters.Partial). Nil
	// routes to slog.Default().
	Logger *slog.Logger
}

// NewCounterEnricher builds the V1 production Enricher. Every dependency is
// mandatory — a nil Bus / Tasks / Pauses fails loud with ErrMisconfigured
// rather than building an enricher that reports believable-but-false zeros
// on one dimension (CLAUDE.md §5). The returned *CounterEnricher is
// immutable and safe for concurrent reuse.
func NewCounterEnricher(deps CounterEnricherDeps) (*CounterEnricher, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: CounterEnricher Bus is nil", ErrMisconfigured)
	}
	if deps.Tasks == nil {
		return nil, fmt.Errorf("%w: CounterEnricher Tasks is nil", ErrMisconfigured)
	}
	if deps.Pauses == nil {
		return nil, fmt.Errorf("%w: CounterEnricher Pauses is nil", ErrMisconfigured)
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &CounterEnricher{
		bus:       deps.Bus,
		tasks:     deps.Tasks,
		pauses:    deps.Pauses,
		logger:    logger,
		scanBound: perSessionScanBound,
	}, nil
}

// Counters implements Enricher for the production backend. It reads the
// session's own full triple (id) to scope every source — no cross-session
// bleed: the authorisation to see this session already happened at the
// projector's identity-scope predicate; the enricher re-scopes each read to
// the ROW's own identity so an admin listing another tenant's session reads
// exactly that session's tasks / events / pauses.
func (e *CounterEnricher) Counters(ctx context.Context, id identity.Identity, sessionID string) SessionCounters {
	if sessionID != "" {
		id.SessionID = sessionID
	}
	var out SessionCounters

	// tasks_count / has_failed_task — from the session-scoped task
	// registry. A read that could not be taken marks the rollup partial
	// rather than reporting a zero nobody measured.
	var tasksRead, pausesRead bool
	out.TasksCount, out.HasFailedTask, tasksRead = e.taskCounters(ctx, id)

	// has_pending_intervention — from the session-scoped pause registry,
	// same partial contract.
	out.HasPendingIntervention, pausesRead = e.pendingIntervention(ctx, id)

	// total_cost_cents / total_tokens / events_count — from the bounded
	// per-session event scan. A truncated scan (or an unavailable
	// windowed-read substrate) sets Partial: the counts are then an honest
	// lower bound, NEVER a silently-undercounted exact value.
	cents, tokens, evCount, partial := e.eventCounters(ctx, id)
	out.TotalCostCents = cents
	out.TotalTokens = tokens
	out.EventsCount = evCount
	out.Partial = partial || !tasksRead || !pausesRead

	return out
}

// taskCounters returns the session's task count, whether any task
// failed, and whether the read was actually taken. The registry read is
// session-scoped by the folded identity; a false third return means the
// counts are unmeasured and the caller must mark the rollup partial.
func (e *CounterEnricher) taskCounters(ctx context.Context, id identity.Identity) (count int, hasFailed bool, read bool) {
	idCtx, err := e.rowScopedCtx(ctx, id)
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: task counter row scope refused",
			slog.String("session_id", id.SessionID), slog.Any("error", err))
		return 0, false, false
	}
	summaries, err := e.tasks.List(idCtx, id, tasks.TaskFilter{})
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: task counter list failed",
			slog.String("session_id", id.SessionID), slog.Any("error", err))
		return 0, false, false
	}
	for _, s := range summaries {
		if s.Status == tasks.StatusFailed {
			hasFailed = true
		}
	}
	return len(summaries), hasFailed, true
}

// rowScopedCtx seats the ROW's own identity for a row-scoped read.
//
// A row inside the caller's verified tenant is ordinary narrowing. A row
// from ANOTHER tenant is a crossing — but the authorization for it
// already happened upstream: the fleet listing that produced this row
// was gated on the caller's verified admin-tier claim and recorded, and
// the projector only ever hands down identities that listing returned.
// Seating it as an audited re-scope names the tenant being read and why,
// so the crossing stays visible instead of being an unexplained widening
// buried in a projector.
func (e *CounterEnricher) rowScopedCtx(ctx context.Context, id identity.Identity) (context.Context, error) {
	verified, anchored := identity.FromVerified(ctx)
	if !anchored || id.TenantID == verified.TenantID {
		return identity.With(ctx, id)
	}
	// Defence in depth: re-check the claim the fleet listing gated on,
	// here where the crossing is actually minted. The listing upstream
	// should never hand down a foreign row to a caller without it, but a
	// crossing that relies on a caller one layer up having checked is a
	// crossing whose safety is inherited rather than held.
	if !auth.HasScope(ctx, auth.ScopeAdmin) && !auth.HasScope(ctx, auth.ScopeConsoleFleet) {
		return nil, fmt.Errorf("sessions counter rollup: %w", errRowScopeUnentitled)
	}
	return identity.WithElevated(ctx, id,
		"sessions counter rollup: reading a row returned by an authorized fleet listing under that row's own identity")
}

// errRowScopeUnentitled — a row from another tenant reached the rollup on
// a request carrying no admin-tier claim. The rollup refuses rather than
// reading it; the row's counters come back unmeasured and the row is
// marked partial.
var errRowScopeUnentitled = errors.New("row belongs to another tenant and the request carries no admin-tier claim")

// pendingIntervention reports whether the session has a paused pause record
// awaiting resume / approval. The pause read is session-scoped.
func (e *CounterEnricher) pendingIntervention(ctx context.Context, id identity.Identity) (pending bool, read bool) {
	idCtx, err := e.rowScopedCtx(ctx, id)
	if err != nil {
		e.logger.WarnContext(ctx, "sessions/protocol: intervention row scope refused",
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
		e.logger.WarnContext(ctx, "sessions/protocol: intervention list failed",
			slog.String("session_id", id.SessionID), slog.Any("error", err))
		return false, false
	}
	return resp.TotalRows > 0, true
}

// eventCounters performs the single bounded per-session event scan and
// returns the summed cost (cents), summed tokens, the event count, and
// whether the scan was partial (truncated or the substrate unavailable).
//
// One ListWindow scan (all event types, full triple, non-admin) serves
// both the events_count and the cost sum: the cost is summed from the
// `llm.cost.recorded` events within the SAME page, so a truncated scan
// makes cost / tokens / events all a lower bound together — Partial covers
// them uniformly (WARN-1).
func (e *CounterEnricher) eventCounters(ctx context.Context, id identity.Identity) (cents, tokens int64, count int, partial bool) {
	hr, ok := e.bus.(events.HistoryReplayer)
	if !ok {
		// The bus does not support windowed history reads. The cost / tokens
		// / events counts are UNKNOWN, not zero — surface Partial (a lower
		// bound of zero is non-authoritative) rather than a believable-but-
		// false exact zero. Logged, never silent.
		e.logger.WarnContext(ctx, "sessions/protocol: event substrate has no windowed-read; counters partial",
			slog.String("session_id", id.SessionID))
		return 0, 0, 0, true
	}
	query := events.EventListQuery{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{id.TenantID},
			UserIDs:    []string{id.UserID},
			SessionIDs: []string{id.SessionID},
		},
		Before: 0,
		Limit:  e.scanBound,
		Admin:  false,
	}
	if mr, ok := e.bus.(events.EventMetadataReplayer); ok {
		metadataPage, err := mr.ListWindowMetadata(ctx, query)
		if err != nil {
			if errors.Is(err, events.ErrReplayUnavailable) {
				e.logger.WarnContext(ctx, "sessions/protocol: metadata windowed read unavailable; counters partial",
					slog.String("session_id", id.SessionID))
			} else {
				e.logger.WarnContext(ctx, "sessions/protocol: metadata event scan failed; counters partial",
					slog.String("session_id", id.SessionID), slog.Any("error", err))
			}
			return 0, 0, 0, true
		}
		var costDollars float64
		for _, meta := range metadataPage.Events {
			if meta.CostSummary {
				costDollars += meta.CostDollars
				tokens += meta.TotalTokens
			}
		}
		count = len(metadataPage.Events)
		partial = metadataPage.HasMore || metadataPage.Truncated
		if partial {
			e.logger.WarnContext(ctx, "sessions/protocol: metadata event scan truncated; counters are a lower bound",
				slog.String("session_id", id.SessionID),
				slog.Int("scan_bound", e.scanBound),
				slog.Bool("has_more", metadataPage.HasMore),
				slog.Bool("retention_gap", metadataPage.Truncated))
		}
		return dollarsToCents(costDollars), tokens, count, partial
	}
	page, err := hr.ListWindow(ctx, query)
	if err != nil {
		// ErrReplayUnavailable and any other read error leave the bounded
		// counts unknown — honest partial, logged, never a silent zero.
		if errors.Is(err, events.ErrReplayUnavailable) {
			e.logger.WarnContext(ctx, "sessions/protocol: windowed read unavailable; counters partial",
				slog.String("session_id", id.SessionID))
		} else {
			e.logger.WarnContext(ctx, "sessions/protocol: per-session event scan failed; counters partial",
				slog.String("session_id", id.SessionID), slog.Any("error", err))
		}
		return 0, 0, 0, true
	}
	// Accumulate cost as a float64 DOLLAR sum across the whole page and
	// convert to cents ONCE after the loop. Per-call `llm.cost.recorded`
	// costs are routinely sub-cent (e.g. $0.004), so rounding each event to
	// whole cents before summing would floor every sub-cent call to 0 and
	// report a believable-but-false EXACT zero on a session that really
	// spent money — reintroducing the false-absence this phase closes. One
	// rounding at the end preserves the true total.
	var costDollars float64
	for i := range page.Events {
		ev := page.Events[i]
		if ev.Type == llm.EventTypeCostRecorded {
			d, t := costFromEvent(ev)
			costDollars += d
			tokens += t
		}
	}
	cents = dollarsToCents(costDollars)
	count = len(page.Events)
	// HasMore: older matching events remain below the page (the scan hit
	// e.scanBound). Truncated: a wrapped best-effort ring may have evicted
	// older events. Either makes the counts a lower bound.
	partial = page.HasMore || page.Truncated
	if partial {
		e.logger.WarnContext(ctx, "sessions/protocol: per-session scan truncated; counters are a lower bound",
			slog.String("session_id", id.SessionID),
			slog.Int("scan_bound", e.scanBound),
			slog.Bool("has_more", page.HasMore),
			slog.Bool("retention_gap", page.Truncated))
	}
	return cents, tokens, count, partial
}

// costFromEvent extracts (dollars, tokens) from an `llm.cost.recorded`
// event, handling both the typed SafePayload (inmem live — the payload
// passes through unredacted) and the RedactedMap the durable substrate
// rehydrates on replay (Payload stored as a generic JSON object). Cost is
// returned as a float64 DOLLAR amount — NOT pre-rounded to cents — so the
// caller can sum across the page before the single dollars→cents rounding
// (a per-event round would floor sub-cent per-call costs to 0). A payload
// that matches neither shape contributes zero (honest — an unrecognised
// payload is not a fabricated cost).
//
// Usage.CacheReadTokens / Usage.CacheWriteTokens are intentionally NOT
// extracted here: cache reads/writes are a subset of PromptTokens, which
// already rolls into TotalTokens, so the session-level token figure is
// unaffected by the cache split. No SessionRow wire field carries a
// cache-token count today, so extracting them would be dead code with
// nowhere to land — a session-level cache facet is Protocol-wire-shaped
// work (new SessionRow field + docs/TS regen), out of scope here.
func costFromEvent(ev events.Event) (dollars float64, tokens int64) {
	switch p := ev.Payload.(type) {
	case llm.CostRecordedPayload:
		return p.Cost.TotalCost, int64(p.Usage.TotalTokens)
	case events.RedactedMap:
		return costFromMap(p.Data)
	default:
		return 0, 0
	}
}

// costFromMap reads the cost (as float64 dollars) / tokens from the
// JSON-shaped payload the durable substrate stores. The keys mirror the
// exported field names of llm.CostRecordedPayload (no json tags) → `Cost` /
// `Usage` objects. Dollars are returned unrounded for page-level summation.
//
// Like costFromEvent, this reads only Usage.TotalTokens — the cache split
// (Usage.CacheReadTokens / CacheWriteTokens) is intentionally not read: it
// is a subset already inside TotalTokens and has no SessionRow field to land
// in this phase.
func costFromMap(m map[string]any) (dollars float64, tokens int64) {
	if c, ok := m["Cost"].(map[string]any); ok {
		if tc, ok := c["TotalCost"].(float64); ok {
			dollars = tc
		}
	}
	if u, ok := m["Usage"].(map[string]any); ok {
		if tt, ok := u["TotalTokens"].(float64); ok {
			tokens = int64(tt)
		}
	}
	return dollars, tokens
}

// dollarsToCents converts a USD dollar amount to whole cents, rounding to
// the nearest cent. A negative or NaN input clamps to zero (a cost is never
// negative; a NaN is a provider-correction bug, not a real charge).
func dollarsToCents(dollars float64) int64 {
	if math.IsNaN(dollars) || dollars <= 0 {
		return 0
	}
	return int64(math.Round(dollars * 100))
}

// Compile-time assertion: *CounterEnricher satisfies Enricher.
var _ Enricher = (*CounterEnricher)(nil)
