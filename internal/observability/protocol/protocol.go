// Package protocol implements the observability.query domain service —
// the ONE bounded administrative query surface over the observability
// rollup projection. It answers operator cost/token/outcome questions from
// the durable, indexed rollup rows without scanning the raw event log at
// read time (CLAUDE.md §4.4: the service depends on the rollups read
// seams, never on a concrete driver and never on the canonical event log).
//
// # The closed administrative contract
//
//   - The time window is MANDATORY and must be aligned to the fixed UTC
//     bucket grid (minute / hour / day) — an unaligned or missing edge is
//     rejected loudly, never silently rounded.
//   - The group_by set is CLOSED to tenant / user / session / model (agent
//     is not a rollup dimension — the canonical payloads carry no
//     authoritative agent id, and an empty axis is not shipped).
//   - The measures are the CLOSED, source-backed measure set. A measure
//     with no canonical carrier (attempts, failed LLM calls, retry /
//     downgrade, task-spawned, user-message counts) is rejected loudly
//     with ErrInvalidRequest — never synthesized and never reported as an
//     inferred zero.
//   - The sort set is CLOSED and every sort is total, so the deterministic
//     cursor pagination never skips or repeats a row, and the cursor is
//     BOUND to the full query shape (window, bucket, filter, grouping,
//     measures, sort): a cursor produced by a differently-shaped query —
//     including one produced under a different identity scope — is
//     rejected with ErrBadCursor before any paging.
//   - The window / result / page budgets are bounded and fail loudly with
//     ErrBudgetExceeded rather than truncating silently.
//   - Every response carries exact integer / decimal measure values (cost
//     is integer micro-units of USD with the measure's fixed decimal
//     scale) plus a MANDATORY freshness block: state
//     current | catching_up | unavailable, the observed rollup watermark,
//     and the retention / window-coverage quality.
//
// # Authority and isolation (CLAUDE.md §6)
//
// The verified caller identity is read from the request context
// (identity.FromVerified) — the request body never supplies tenant / user
// / session identity for widening. An ordinary caller's query is forced to
// their own verified (tenant, user, session) triple: naming any other
// tenant, user, or session in the filters fails closed with the matching
// scope sentinel, and the effective filter is always the caller's own
// triple — one caller can never enumerate another user's, session's, or
// tenant's aggregates. A caller holding the verified admin OR
// console:fleet claim (injected as a ScopeChecker — the closed two-scope
// admit set) may run widened queries: naming other tenants, other users,
// or fanning in across sessions/users. Every widened fan-in emits EXACTLY
// ONE canonical audit.admin_scope_used event BEFORE the read reaches
// storage; an audit-emit failure fails the read loud. An elevated caller
// reading exactly their own triple is not a widened read and emits none.
//
// # Freshness and honesty
//
// Rollups are best-effort aggregates over successfully-persisted canonical
// events — never a billing-exact ledger and never exactly-once. The
// freshness block never pretends: state current / catching_up /
// unavailable comes from the rollup projector's quality seam, the
// watermark is the last applied sequence of the existing local durable
// sequence, and retention / coverage describe the retained horizon
// relative to the requested window. A query NEVER returns zero as a
// substitute for "projection unavailable": rows (when the store holds any
// for the window) carry exact values and the state is reported honestly.
//
// # Domain adapters (until the canonical wire types land)
//
// The service speaks this package's domain types (Request / Response). The
// Protocol wire handler (a later wiring step) adapts the canonical
// observability.query wire request into a Request, calls Query, and adapts
// the Response back onto the wire. The closed sets the service validates
// are the rollups domain's own (rollups.AllDimensions / AllMeasures /
// AllBucketSizes / AllSortKeys), so the adapter maps wire strings from a
// single source of truth and the service rejects anything else with
// ErrInvalidRequest. The wire handler must also attach the verified
// identity and the verified scope set to the request context (the
// transport's auth middleware does this for every method), and must
// provide the admin|console:fleet predicate as the Service's ScopeChecker.
//
// # Layout
//
//   - protocol.go — domain types, sentinel errors, seams, the Service.
//   - query.go — the Query method (authority, narrowing, execution).
//   - quality.go — the freshness block (state, watermark, coverage).
//   - audit.go — the widened-read audit emit.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/observability/rollups"
)

// Sentinel errors the service returns. The future wire handler maps each
// onto a canonical Protocol Code; in-process callers compare with
// errors.Is.
var (
	// ErrIdentityRequired — the request context carries no verified
	// identity triple, or the triple is incomplete. Identity is mandatory
	// (CLAUDE.md §6 rule 9) and the request body can never supply it, so
	// the service fails closed.
	ErrIdentityRequired = errors.New("observability/protocol: verified identity required")
	// ErrCrossTenantScope — an ordinary caller's filter named a tenant
	// outside their verified tenant.
	ErrCrossTenantScope = errors.New("observability/protocol: cross-tenant query requires the admin or console:fleet claim")
	// ErrCrossUserScope — an ordinary caller's filter named a user outside
	// their verified user.
	ErrCrossUserScope = errors.New("observability/protocol: cross-user query requires the admin or console:fleet claim")
	// ErrCrossSessionScope — an ordinary caller's filter named a session
	// outside their verified session.
	ErrCrossSessionScope = errors.New("observability/protocol: cross-session query requires the admin or console:fleet claim")
	// ErrInvalidRequest — the request failed structural or closed-set
	// validation (unknown dimension / measure / sort / bucket, a
	// misaligned or missing window, an empty measure set, a missing or
	// non-positive page limit, an unsupported measure).
	ErrInvalidRequest = errors.New("observability/protocol: invalid request")
	// ErrBudgetExceeded — the query exceeds a result budget (the window
	// spans more buckets than the closed maximum, or the page limit
	// exceeds the maximum). Fails loudly; never a truncated response.
	ErrBudgetExceeded = errors.New("observability/protocol: query exceeds a result budget")
	// ErrBadCursor — the page cursor is malformed or was produced by a
	// different query shape (including a different identity scope). The
	// caller must restart from the first page.
	ErrBadCursor = errors.New("observability/protocol: invalid or incompatible page cursor")
	// ErrAuditFailed — a granted identity-scope widening could not emit its
	// mandatory audit.admin_scope_used record. The read fails closed.
	ErrAuditFailed = errors.New("observability/protocol: widened-read audit emit failed")
	// ErrQueryFailed — the rollup store rejected the (already-validated)
	// query. Wraps the store error.
	ErrQueryFailed = errors.New("observability/protocol: rollup query failed")
	// ErrQualityFailed — the freshness block could not be read. The
	// response is refused loud rather than shipped with a fabricated
	// freshness stamp.
	ErrQualityFailed = errors.New("observability/protocol: rollup quality read failed")
	// ErrMisconfigured — NewService was called with a nil mandatory
	// dependency.
	ErrMisconfigured = errors.New("observability/protocol: NewService missing a mandatory dependency")
)

// Filters is the closed-axis filter over the rollup dimensions. Each slice
// has set semantics (an empty slice matches every value on that axis for a
// WIDENED caller and is overridden to the verified triple for an ordinary
// caller); all axes are ANDed.
type Filters struct {
	// TenantIDs restricts to rows of these tenants.
	TenantIDs []string
	// UserIDs restricts to rows of these users.
	UserIDs []string
	// SessionIDs restricts to rows of these sessions.
	SessionIDs []string
	// Models restricts to rows with these model values. An empty Models
	// slice matches BOTH un-attributed (model "") and attributed rows.
	Models []string
}

// Request is the domain shape of the observability.query method. It is
// immutable in the intended usage — the service never mutates it.
type Request struct {
	// From / To bound the bucket window (half-open [From, To), both UTC
	// and aligned to the Bucket grid — each must fall exactly on a fixed
	// UTC bucket boundary). MANDATORY: a zero or unaligned edge is
	// rejected with ErrInvalidRequest.
	From time.Time
	To   time.Time
	// Bucket is the closed query bucket size (minute / hour / day). Rows
	// are stored at the fixed UTC minute grid and coarsened to Bucket at
	// read time.
	Bucket rollups.BucketSize
	// GroupBy is the closed dimension subset the rows are grouped by (may
	// be empty — then one row per bucket aggregates the whole window).
	// The closed set is exactly tenant / user / session / model; any
	// other dimension is rejected with ErrInvalidRequest.
	GroupBy []rollups.Dimension
	// Filters constrains the rows before grouping. For an ordinary caller
	// the tenant / user / session axes are overridden to the verified
	// triple, and naming any other tenant / user / session fails closed
	// with the matching scope sentinel.
	Filters Filters
	// Measures selects the measures each result row carries (mandatory,
	// non-empty, closed, deduplicated). A measure with no canonical
	// source is rejected loudly — never synthesized, never inferred zero.
	Measures []rollups.Measure
	// Sort is the closed sort key (empty defaults to bucket ascending).
	Sort rollups.SortKey
	// SortMeasure names the measure used by a measure sort; it must be a
	// closed measure AND a member of the selected Measures.
	SortMeasure rollups.Measure
	// Limit bounds the page size (1..rollups.MaxRowsPerQuery, MANDATORY).
	Limit int
	// Cursor is the opaque full-query-bound pagination cursor returned by
	// a previous page ("" = the first page). A stale or malformed cursor
	// is rejected with ErrBadCursor.
	Cursor string
}

// Coverage reports how the requested window relates to the retained rollup
// horizon — the retention-quality signal on every response.
type Coverage string

const (
	// CoverageCovered — the window sits entirely inside the retained
	// horizon: no retained data is missing for the window.
	CoverageCovered Coverage = "covered"
	// CoveragePartial — the window overlaps the retained horizon but
	// extends outside it (older or newer data has been retained away or
	// never arrived).
	CoveragePartial Coverage = "partial"
	// CoverageGap — the store holds no retained rows for the window at
	// all (nothing was ever applied to it, or its rows were retained
	// away). Never reported as zero totals — the caller sees gap and the
	// exact (empty) rows.
	CoverageGap Coverage = "gap"
)

// QualityBlock is the mandatory freshness / completeness stamp on every
// response. It never pretends: the state is the projector's catch-up
// quality, the watermark is the last applied sequence of the existing
// local durable sequence, and the retention / coverage fields describe the
// retained horizon relative to the requested window.
type QualityBlock struct {
	// State is current | catching_up | unavailable (the projector's
	// catch-up quality).
	State rollups.State
	// Watermark is the last successfully applied sequence of the local
	// durable event sequence (0 = nothing applied).
	Watermark uint64
	// WatermarkAt is the wall-clock instant the watermark last advanced
	// in the projector instance (zero before the first advance).
	WatermarkAt time.Time
	// RetentionStart is the oldest retained bucket start (zero when the
	// store holds no rows).
	RetentionStart time.Time
	// RetentionEnd is the newest retained bucket start (zero when the
	// store holds no rows).
	RetentionEnd time.Time
	// Coverage is the retention quality of the requested window relative
	// to the retained horizon.
	Coverage Coverage
	// Err is the projector's last ingestion failure, present only when
	// State is StateUnavailable. The wire adapter MAY elide it; in-process
	// callers use it for diagnostics.
	Err error
}

// Response is the domain shape of the observability.query result: one
// deterministic page of exact-value rows, the full-query-bound cursor for
// the next page ("" = last page), and the mandatory freshness block.
type Response struct {
	// Rows is the page in the query's total order (nil when empty).
	Rows []rollups.Row
	// NextCursor is the opaque cursor for the next page ("" when this is
	// the last page).
	NextCursor string
	// Quality is the mandatory freshness / completeness stamp.
	Quality QualityBlock
}

// Querier is the read seam over the rollup projection. The V1 production
// implementation is any rollups.Store driver (in-memory, SQLite, Postgres
// — all behind the rollups.Store interface); the service depends ONLY on
// this narrow read surface, never on a concrete driver and never on the
// canonical event log.
type Querier interface {
	// Query executes a validated rollup query. The store re-validates and
	// returns the wrapped rollups.ErrQueryInvalid / ErrQueryBudget /
	// ErrBadCursor sentinels. The response page is deterministic for a
	// stable store and the access is bounded indexed — never a raw event
	// scan.
	Query(ctx context.Context, q rollups.Query) (rollups.Result, error)
}

// QualitySource reports the rollup projection's operational quality — the
// freshness block's substrate. The V1 production implementation is the
// rollups.Projector (its Quality method). The wiring MUST point the
// Querier and the QualitySource at the SAME underlying store so the
// watermark / retention they report describe the rows the Querier reads.
type QualitySource interface {
	// Quality returns the projector's operational snapshot: catch-up
	// state, the durable watermark, and the retained horizon.
	Quality(ctx context.Context) (rollups.Quality, error)
}

// ScopeChecker is the narrow predicate the service consults to decide
// whether the caller may widen. The production implementation reads the
// verified admin OR console:fleet scope claim from the request context
// (the closed two-scope admit set — never a request-body value); tests
// inject a deterministic predicate.
type ScopeChecker func(ctx context.Context) bool

// AuditSink is the mandatory, narrow event sink used to publish the
// canonical audit.admin_scope_used event before a granted widening reaches
// storage. An events.EventBus satisfies this seam via its Publish method.
type AuditSink func(ctx context.Context, ev events.Event) error

// Service implements the observability.query domain method. It is a
// safe-for-concurrent-reuse compiled artifact: every dependency is set at
// construction and never mutated; per-call state lives in the call's
// arguments and locals, never on the Service.
type Service struct {
	querier  Querier
	quality  QualitySource
	scope    ScopeChecker
	audit    AuditSink
	redactor audit.Redactor
	logger   *slog.Logger
}

// Option configures NewService.
type Option func(*Service)

// WithLogger sets the slog.Logger the Service logs admin actions and
// audit-emit failures to. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewService builds the observability.query domain service. Every
// dependency is mandatory — a nil one fails loud with ErrMisconfigured
// rather than building a Service that would nil-panic on the first query
// (CLAUDE.md §5). The returned *Service is immutable after construction
// and safe for concurrent use by N goroutines.
func NewService(
	querier Querier,
	quality QualitySource,
	scope ScopeChecker,
	audit AuditSink,
	redactor audit.Redactor,
	opts ...Option,
) (*Service, error) {
	if querier == nil {
		return nil, fmt.Errorf("%w: Querier is nil", ErrMisconfigured)
	}
	if quality == nil {
		return nil, fmt.Errorf("%w: QualitySource is nil", ErrMisconfigured)
	}
	if scope == nil {
		return nil, fmt.Errorf("%w: ScopeChecker is nil", ErrMisconfigured)
	}
	if audit == nil {
		return nil, fmt.Errorf("%w: AuditSink is nil", ErrMisconfigured)
	}
	if redactor == nil {
		return nil, fmt.Errorf("%w: Redactor is nil", ErrMisconfigured)
	}
	s := &Service{
		querier:  querier,
		quality:  quality,
		scope:    scope,
		audit:    audit,
		redactor: redactor,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// singleScopeValue renders a filter axis as the audit payload's target
// component: the single value when the axis names exactly one, or the
// canonical blank wildcard/fan-in spelling when it names many or none —
// recording only the first member of a multi-principal read would make it
// look narrower than it was.
func singleScopeValue(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return ""
}

// auditString extracts a string field from the redacted payload, failing
// loud when the redactor dropped or reshaped it (CLAUDE.md §7 rule 6).
func auditString(redacted map[string]any, key string) (string, error) {
	value, ok := redacted[key]
	if !ok {
		return "", fmt.Errorf("redactor dropped audit field %q", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("redactor changed audit field %q to %T", key, value)
	}
	return text, nil
}
