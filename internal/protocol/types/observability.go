package types

import "time"

// Observability wire types — the canonical contract for the
// `observability.query` administrative read (HA-65): the ONE bounded
// query surface over the observability rollup projection. The wire
// shapes are FLAT: they mirror the rollup domain
// (`internal/observability/rollups`) as self-contained JSON types, so
// the Console never reads a runtime Go struct (RFC §5.1 / CLAUDE.md §13
// single-source rule).
//
// The closed contract:
//
//   - The time window is MANDATORY and must be aligned to the fixed UTC
//     bucket grid (minute / hour / day) — an unaligned or missing edge
//     is rejected loudly, never silently rounded.
//   - The group_by set is CLOSED to tenant / user / session / model.
//   - The measures are the CLOSED, source-backed measure set (see the
//     Measure consts below); a measure with no canonical carrier is
//     rejected loudly — never synthesized and never reported as an
//     inferred zero.
//   - The sort set is CLOSED and every sort is total, so the
//     deterministic cursor pagination never skips or repeats a row; the
//     cursor is BOUND to the full query shape (a cursor produced by a
//     differently-shaped query — including one produced under a
//     different identity scope — is rejected with `invalid_cursor`
//     before any paging).
//   - The window / result / page budgets are bounded and fail loudly
//     with `query_budget_exceeded` rather than truncating silently.
//   - Every response carries exact integer / decimal measure values
//     (cost is integer micro-units of USD with the measure's fixed
//     decimal scale) plus a MANDATORY freshness block: state
//     current | catching_up | unavailable, the observed rollup
//     watermark, and the retention / window-coverage quality.
//
// Authority and isolation: the verified caller identity is read from the
// request context — the request body never supplies tenant / user /
// session identity for widening. An ordinary caller's query is forced to
// their own verified triple; a caller holding the verified admin OR
// console:fleet claim (the closed two-scope admit set) may run widened
// queries. Every widened fan-in emits EXACTLY ONE canonical
// `audit.admin_scope_used` event BEFORE the read reaches storage.
//
// Identity is mandatory on every request (RFC §5.5 / CLAUDE.md §6
// rule 9): an incomplete IdentityScope fails closed at the wire edge
// with CodeIdentityRequired.

// Observability dimensions — the CLOSED group_by axis set. Values are
// authoritative: tenant / user / session come from the event's identity
// triple; model comes from the source event's model field. Agent is NOT
// a rollup dimension (no canonical payload carries an authoritative
// agent id) and is rejected loudly.
const (
	// ObservabilityDimensionTenant groups rows by the event's tenant_id.
	ObservabilityDimensionTenant = "tenant"
	// ObservabilityDimensionUser groups rows by the event's user_id.
	ObservabilityDimensionUser = "user"
	// ObservabilityDimensionSession groups rows by the event's
	// session_id.
	ObservabilityDimensionSession = "session"
	// ObservabilityDimensionModel groups rows by the source payload's
	// model (LLM completions only; empty for events with no
	// authoritative model).
	ObservabilityDimensionModel = "model"
)

// Observability measures — the CLOSED, source-backed measure set. Every
// measure accumulates in exact integer form (int64): counts and token
// counts are plain integers, cost is integer micro-units of USD (1 USD =
// 1_000_000 micro-units), latency is integer milliseconds. Nothing is
// normalised to float64 anywhere. A measure with no canonical carrier is
// absent from the set and rejected loudly if requested.
const (
	// ObservabilityMeasureLLMCompletions — count of successfully-recorded
	// LLM completions.
	ObservabilityMeasureLLMCompletions = "llm_completions"
	// ObservabilityMeasureLLMTokensPrompt — sum of prompt tokens.
	ObservabilityMeasureLLMTokensPrompt = "llm_tokens_prompt" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMTokensCompletion — sum of completion tokens.
	ObservabilityMeasureLLMTokensCompletion = "llm_tokens_completion" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMTokensReasoning — sum of reasoning tokens.
	ObservabilityMeasureLLMTokensReasoning = "llm_tokens_reasoning" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMTokensCacheRead — sum of cache-read tokens.
	ObservabilityMeasureLLMTokensCacheRead = "llm_tokens_cache_read" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMTokensCacheWrite — sum of cache-write tokens.
	ObservabilityMeasureLLMTokensCacheWrite = "llm_tokens_cache_write" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMTokensTotal — sum of total tokens.
	ObservabilityMeasureLLMTokensTotal = "llm_tokens_total" //nolint:gosec // G101 false positive: closed observability measure-name wire constant, not a credential
	// ObservabilityMeasureLLMCostMicros — sum of provider-reported
	// TotalCost in exact integer micro-units of USD.
	ObservabilityMeasureLLMCostMicros = "llm_cost_micros"
	// ObservabilityMeasureLLMLatencyCount — count of latency-bearing
	// completions.
	ObservabilityMeasureLLMLatencyCount = "llm_latency_count"
	// ObservabilityMeasureLLMLatencySumMS — sum of latency in integer
	// milliseconds.
	ObservabilityMeasureLLMLatencySumMS = "llm_latency_sum_ms"
	// ObservabilityMeasureLLMLatencyMinMS — minimum latency in integer
	// milliseconds.
	ObservabilityMeasureLLMLatencyMinMS = "llm_latency_min_ms"
	// ObservabilityMeasureLLMLatencyMaxMS — maximum latency in integer
	// milliseconds.
	ObservabilityMeasureLLMLatencyMaxMS = "llm_latency_max_ms"
	// ObservabilityMeasureTasksCompleted — count of task.completed events.
	ObservabilityMeasureTasksCompleted = "tasks_completed"
	// ObservabilityMeasureTasksFailed — count of task.failed events.
	ObservabilityMeasureTasksFailed = "tasks_failed"
	// ObservabilityMeasureTasksCancelled — count of task.cancelled events.
	ObservabilityMeasureTasksCancelled = "tasks_cancelled"
)

// Observability bucket sizes — the CLOSED query bucket grid. Rows are
// stored at the fixed UTC minute grid and coarsened to the requested
// bucket at read time.
const (
	// ObservabilityBucketMinute — one-minute buckets.
	ObservabilityBucketMinute = "minute"
	// ObservabilityBucketHour — one-hour buckets.
	ObservabilityBucketHour = "hour"
	// ObservabilityBucketDay — one-day buckets.
	ObservabilityBucketDay = "day"
)

// Observability sort keys — the CLOSED sort set. Every sort is total
// (primary key, then bucket start, then the grouped dimension values in
// canonical order as deterministic tie-breakers).
const (
	// ObservabilitySortBucketAsc — chronological, oldest bucket first.
	ObservabilitySortBucketAsc = "bucket_asc"
	// ObservabilitySortBucketDesc — newest bucket first.
	ObservabilitySortBucketDesc = "bucket_desc"
	// ObservabilitySortMeasureAsc — by the query's SortMeasure sum,
	// ascending.
	ObservabilitySortMeasureAsc = "measure_asc"
	// ObservabilitySortMeasureDesc — by the query's SortMeasure sum,
	// descending.
	ObservabilitySortMeasureDesc = "measure_desc"
)

// ObservabilityQueryFilter is the closed-axis filter over the rollup
// dimensions. Each slice has set semantics (an empty slice matches every
// value on that axis for a WIDENED caller and is overridden to the
// verified triple for an ordinary caller); all axes are ANDed.
type ObservabilityQueryFilter struct {
	// TenantIDs restricts to rows of these tenants.
	TenantIDs []string `json:"tenant_ids,omitempty"`
	// UserIDs restricts to rows of these users.
	UserIDs []string `json:"user_ids,omitempty"`
	// SessionIDs restricts to rows of these sessions.
	SessionIDs []string `json:"session_ids,omitempty"`
	// Models restricts to rows with these model values. An empty Models
	// slice matches BOTH un-attributed (model "") and attributed rows.
	Models []string `json:"models,omitempty"`
}

// ObservabilityQueryRequest is the `observability.query` request body.
// The verified caller identity comes from ctx; the body carries the
// window, the closed sets, the filters, the page bound, and the opaque
// cursor.
type ObservabilityQueryRequest struct {
	// Identity is the (tenant, user, session) scope the query runs
	// under. Mandatory — an incomplete triple fails closed. The verified
	// identity is read from the request context; this body triple is
	// reconciled against it (the body never widens).
	Identity IdentityScope `json:"identity"`
	// From / To bound the bucket window (half-open [From, To), both UTC
	// and aligned to the Bucket grid — each must fall exactly on a fixed
	// UTC bucket boundary). MANDATORY.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Bucket is the closed query bucket size (minute | hour | day).
	Bucket string `json:"bucket"`
	// GroupBy is the closed dimension subset the rows are grouped by
	// (may be empty — then one row per bucket aggregates the whole
	// window). The closed set is exactly tenant / user / session / model.
	GroupBy []string `json:"group_by,omitempty"`
	// Filters constrains the rows before grouping. For an ordinary
	// caller the tenant / user / session axes are overridden to the
	// verified triple, and naming any other tenant / user / session
	// fails closed.
	Filters ObservabilityQueryFilter `json:"filters,omitempty"`
	// Measures selects the measures each result row carries (mandatory,
	// non-empty, closed, deduplicated).
	Measures []string `json:"measures"`
	// Sort is the closed sort key (empty defaults to bucket ascending).
	Sort string `json:"sort,omitempty"`
	// SortMeasure names the measure used by a measure sort; it must be a
	// closed measure AND a member of the selected Measures.
	SortMeasure string `json:"sort_measure,omitempty"`
	// Limit bounds the page size (1..max, MANDATORY).
	Limit int `json:"limit"`
	// Cursor is the opaque full-query-bound pagination cursor returned
	// by a previous page ("" = the first page). A stale or malformed
	// cursor is rejected with `invalid_cursor`.
	Cursor string `json:"cursor,omitempty"`
}

// ObservabilityMeasureValue is the exact, wire-ready value of one
// measure for one row. Every measure accumulates in integer form only:
// counts, tokens, latency ms, and cost micro-units. N is the exact
// accumulated integer — counters above 2^53 stay exact because nothing
// is ever normalised to float64. Scale is the measure's fixed decimal
// denominator (1 for integer measures; 1_000_000 for cost), so a
// consumer formats decimal USD exactly as N / Scale at the edge.
type ObservabilityMeasureValue struct {
	// N is the exact accumulated integer.
	N int64 `json:"n"`
	// Scale is the decimal denominator of the measure's unit; constant
	// per measure.
	Scale uint32 `json:"scale"`
}

// ObservabilityQueryRow is one grouped result row.
type ObservabilityQueryRow struct {
	// BucketStart is the bucket the row aggregates (coarsened to the
	// query's Bucket size, UTC).
	BucketStart time.Time `json:"bucket_start"`
	// Dimensions carries the query's GroupBy dimension values (empty
	// when GroupBy was empty — the row aggregates the whole window).
	Dimensions map[string]string `json:"dimensions,omitempty"`
	// Measures carries the query's requested measures and their exact
	// integer sums / folds.
	Measures map[string]ObservabilityMeasureValue `json:"measures"`
}

// ObservabilityQualityBlock is the mandatory freshness / completeness
// stamp on every response. It never pretends: the state is the
// projector's catch-up quality, the watermark is the last applied
// sequence of the existing local durable sequence, and the retention /
// coverage fields describe the retained horizon relative to the
// requested window.
type ObservabilityQualityBlock struct {
	// State is current | catching_up | unavailable (the projector's
	// catch-up quality).
	State string `json:"state"`
	// Watermark is the last successfully applied sequence of the local
	// durable event sequence (0 = nothing applied).
	Watermark uint64 `json:"watermark"`
	// WatermarkAt is the wall-clock instant the watermark last advanced
	// (zero before the first advance).
	WatermarkAt time.Time `json:"watermark_at,omitempty"`
	// RetentionStart is the oldest retained bucket start (zero when the
	// store holds no rows).
	RetentionStart time.Time `json:"retention_start,omitempty"`
	// RetentionEnd is the newest retained bucket start (zero when the
	// store holds no rows).
	RetentionEnd time.Time `json:"retention_end,omitempty"`
	// Coverage is the retention quality of the requested window relative
	// to the retained horizon: covered | partial | gap.
	Coverage string `json:"coverage"`
}

// ObservabilityQueryResponse is the `observability.query` response: one
// deterministic page of exact-value rows, the full-query-bound cursor
// for the next page ("" = last page), and the mandatory freshness
// block.
type ObservabilityQueryResponse struct {
	// Rows is the page in the query's total order (nil when empty).
	Rows []ObservabilityQueryRow `json:"rows"`
	// NextCursor is the opaque cursor for the next page ("" when this is
	// the last page).
	NextCursor string `json:"next_cursor,omitempty"`
	// Quality is the mandatory freshness / completeness stamp.
	Quality ObservabilityQualityBlock `json:"quality"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under.
	ProtocolVersion string `json:"protocol_version"`
}
