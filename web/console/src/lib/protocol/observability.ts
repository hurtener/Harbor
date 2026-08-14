// Harbor Console — the `observability.*` wire types (HA-65).
//
// Hand-synced field-for-field from
// `internal/protocol/types/observability.go` (the Go-side single source,
// D-002). One method consumes these:
//   - observability.query — ObservabilityQueryRequest →
//     ObservabilityQueryResponse
//
// The route is `POST /v1/observability/query`. The request body folds
// identity at the Transport choke point, so the TS request interface
// omits `identity` (the single sanctioned per-field omission). The
// verified caller identity is read from the request context — the body
// NEVER supplies tenant / user / session identity for widening; a
// widened (admin OR console:fleet) fan-in is gated + audited server-side.
//
// Closed contract: a mandatory UTC-grid-aligned window, closed
// group/measure/sort sets (see the string constants below), exact
// integer/decimal measure values (cost is integer micro-units of USD
// with the measure's fixed decimal scale), a full-query-bound
// deterministic cursor, and a MANDATORY freshness block.

/** The closed group_by axis set — tenant | user | session | model. */
export type ObservabilityDimension = 'tenant' | 'user' | 'session' | 'model';

/** The closed bucket grid — rows are stored at the UTC minute grid and
 * coarsened to the requested bucket at read time. */
export type ObservabilityBucket = 'minute' | 'hour' | 'day';

/** The closed sort set — every sort is total (deterministic paging). */
export type ObservabilitySort =
	| 'bucket_asc'
	| 'bucket_desc'
	| 'measure_asc'
	| 'measure_desc';

/** The closed-axis filter over the rollup dimensions (all axes ANDed). */
export interface ObservabilityQueryFilter {
	tenant_ids?: string[];
	user_ids?: string[];
	session_ids?: string[];
	/** Empty matches BOTH un-attributed (model "") and attributed rows. */
	models?: string[];
}

/** `observability.query` request. The window is MANDATORY and aligned; the
 * measures set is mandatory, non-empty, closed; the limit is 1..max. */
export interface ObservabilityQueryRequest {
	/** Half-open [from, to), both UTC and aligned to the bucket grid. */
	from: string;
	to: string;
	/** minute | hour | day. */
	bucket: ObservabilityBucket;
	/** Closed dimension subset to group by (empty = one row per bucket). */
	group_by?: ObservabilityDimension[];
	/** Constrains rows before grouping. For an ordinary caller the tenant /
	 * user / session axes are overridden to the verified triple. */
	filters?: ObservabilityQueryFilter;
	/** Mandatory, non-empty, closed, deduplicated. */
	measures: string[];
	/** Empty defaults to bucket ascending. */
	sort?: ObservabilitySort;
	/** Must be a closed measure AND a member of the selected measures for a
	 * measure sort. */
	sort_measure?: string;
	/** Page size (1..max, mandatory). */
	limit: number;
	/** Opaque full-query-bound pagination cursor ("" = the first page). A
	 * stale or foreign cursor is rejected with `invalid_cursor`. */
	cursor?: string;
}

/** The exact, wire-ready value of one measure for one row. N is the exact
 * accumulated integer (never float64); Scale is the measure's fixed decimal
 * denominator (1 for integer measures; 1_000_000 for cost), so decimal USD
 * formats exactly as N / Scale at the edge. */
export interface ObservabilityMeasureValue {
	n: number;
	scale: number;
}

/** One grouped result row. */
export interface ObservabilityQueryRow {
	/** The bucket the row aggregates (coarsened to the query's bucket, UTC). */
	bucket_start: string;
	/** The query's GroupBy dimension values (empty when GroupBy was empty). */
	dimensions?: Record<string, string>;
	/** The query's requested measures and their exact integer sums / folds. */
	measures: Record<string, ObservabilityMeasureValue>;
}

/** The MANDATORY freshness / completeness stamp on every response. It never
 * pretends: the state is the projector's catch-up quality, the watermark is
 * the last applied sequence of the local durable sequence, and the retention
 * / coverage fields describe the retained horizon relative to the window. */
export interface ObservabilityQualityBlock {
	/** current | catching_up | unavailable. */
	state: string;
	/** Last successfully applied sequence (0 = nothing applied). */
	watermark: number;
	watermark_at?: string;
	/** Oldest retained bucket start (zero when the store holds no rows). */
	retention_start?: string;
	/** Newest retained bucket start (zero when the store holds no rows). */
	retention_end?: string;
	/** Retention quality of the window relative to the horizon: covered |
	 * partial | gap. */
	coverage: string;
}

/** `observability.query` response — one deterministic page of exact-value
 * rows, the full-query-bound cursor for the next page, and the mandatory
 * freshness block. */
export interface ObservabilityQueryResponse {
	rows: ObservabilityQueryRow[];
	/** Opaque cursor for the next page ("" = last page). */
	next_cursor?: string;
	quality: ObservabilityQualityBlock;
	protocol_version: string;
}
