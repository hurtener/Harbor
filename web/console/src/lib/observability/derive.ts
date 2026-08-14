// Harbor Console — the bounded `observability.query` consumer projections
// (HA-65, Phase 247 Console consumer).
//
// Pure, DOM-free derivations the Observability page renders. Extracted
// into a unit-testable module so no `.svelte` component re-implements
// UTC-grid alignment, exact integer/decimal measure formatting, or the
// freshness-block presentation, and so each projection is locked by a
// Vitest against its honest states. None of these add a Protocol field or
// issue a Protocol call — they fold the already-shipped
// `observability.query` wire types (CLAUDE.md §4.5 rule 5: the typed
// client is the ONLY fetch path).
//
// The honest-value contract (Phase 247 acceptance criteria):
//   - The query window is MANDATORY and must be UTC-grid-aligned to the
//     requested bucket; the wire rejects an unaligned edge loudly, so
//     this consumer ONLY ever builds aligned windows (never rounds
//     silently, never sends a raw operator input).
//   - Measure values arrive as exact integers (`N`) with a fixed decimal
//     `Scale` (1 for counts/tokens/latency-ms; 1_000_000 for cost
//     micro-units of USD). Formatting is exact integer arithmetic via
//     BigInt over the value AS RECEIVED — no float division, no
//     `toFixed`, no `n / scale` — so no precision is lost or invented at
//     the edge.
//   - A missing / non-finite measure or component renders as `null` (the
//     view shows "—"), NEVER as an ambiguous zero. Unsupported measures
//     are absent from the closed set, not synthesized.

import type {
	ObservabilityBucket,
	ObservabilityDimension,
	ObservabilityMeasureValue,
	ObservabilityQualityBlock,
	ObservabilitySort
} from '../protocol/observability.js';

/* ================================================================== */
/* The closed wire sets — surfaced verbatim for the control surface    */
/* ================================================================== */

/** One display entry for a closed dimension axis (tenant/user/session/
 * model — agent is NOT a rollup dimension, CLAUDE.md §13). */
export interface DimensionMeta {
	/** The wire key. */
	key: ObservabilityDimension;
	/** The operator-facing label. */
	label: string;
}

/** The closed `group_by` axis set, in the wire's canonical order. */
export const OBSERVABILITY_DIMENSIONS: readonly DimensionMeta[] = [
	{ key: 'tenant', label: 'Tenant' },
	{ key: 'user', label: 'User' },
	{ key: 'session', label: 'Session' },
	{ key: 'model', label: 'Model' }
];

/** One display entry for a closed bucket size. */
export interface BucketMeta {
	/** The wire key (minute | hour | day). */
	key: ObservabilityBucket;
	/** The operator-facing label. */
	label: string;
}

/** The closed bucket grid — rows are stored at the UTC minute grid and
 * coarsened to the requested bucket at read time. */
export const OBSERVABILITY_BUCKETS: readonly BucketMeta[] = [
	{ key: 'minute', label: 'Minute' },
	{ key: 'hour', label: 'Hour' },
	{ key: 'day', label: 'Day' }
];

/** One display entry for a closed sort key. */
export interface SortMeta {
	/** The wire key (bucket_asc | bucket_desc | measure_asc |
	 * measure_desc). */
	key: ObservabilitySort;
	/** The operator-facing label. */
	label: string;
}

/** The closed sort set — every sort is total (deterministic paging). */
export const OBSERVABILITY_SORTS: readonly SortMeta[] = [
	{ key: 'bucket_asc', label: 'Bucket ↑' },
	{ key: 'bucket_desc', label: 'Bucket ↓' },
	{ key: 'measure_asc', label: 'Measure ↑' },
	{ key: 'measure_desc', label: 'Measure ↓' }
];

/** One display entry for a closed, source-backed measure. */
export interface MeasureMeta {
	/** The wire key (e.g. `llm_cost_micros`). */
	key: string;
	/** The operator-facing label. */
	label: string;
	/** The display unit ('' for pure counts, 'USD' for cost). */
	unit: string;
	/** The measure family the UI groups by. */
	group: 'llm' | 'task';
	/** The fixed decimal denominator — 1 for integer measures,
	 * 1_000_000 for cost micro-units (single-sourced from the wire
	 * contract's `scale` semantics). */
	scale: number;
}

/** The decimal denominator of the cost measure (one USD = 1_000_000
 * micro-units) — mirrors the Go-side `CostScaleMicros`. */
export const COST_SCALE_MICROS = 1_000_000;

/**
 * The CLOSED, source-backed measure set, in the wire's canonical order
 * (mirrors `internal/protocol/types/observability.go`). Every measure
 * maps to an existing canonical event payload; a measure with no
 * canonical carrier is ABSENT from the set and never synthesized. In
 * particular, attempts, failed LLM calls, retry/downgrade, task-spawned,
 * and user-message counts have no canonical backing and are absent.
 */
export const OBSERVABILITY_MEASURES: readonly MeasureMeta[] = [
	{ key: 'llm_completions', label: 'LLM completions', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_prompt', label: 'Prompt tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_completion', label: 'Completion tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_reasoning', label: 'Reasoning tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_cache_read', label: 'Cache-read tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_cache_write', label: 'Cache-write tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_tokens_total', label: 'Total tokens', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_cost_micros', label: 'LLM cost', unit: 'USD', group: 'llm', scale: COST_SCALE_MICROS },
	{ key: 'llm_latency_count', label: 'Latency samples', unit: '', group: 'llm', scale: 1 },
	{ key: 'llm_latency_sum_ms', label: 'Latency sum', unit: 'ms', group: 'llm', scale: 1 },
	{ key: 'llm_latency_min_ms', label: 'Latency min', unit: 'ms', group: 'llm', scale: 1 },
	{ key: 'llm_latency_max_ms', label: 'Latency max', unit: 'ms', group: 'llm', scale: 1 },
	{ key: 'tasks_completed', label: 'Tasks completed', unit: '', group: 'task', scale: 1 },
	{ key: 'tasks_failed', label: 'Tasks failed', unit: '', group: 'task', scale: 1 },
	{ key: 'tasks_cancelled', label: 'Tasks cancelled', unit: '', group: 'task', scale: 1 }
];

/** The default measure selection a fresh page loads with — a bounded,
 * readable first query (completions, total tokens, cost, task outcomes).
 * Every key is a member of the closed set. */
export const DEFAULT_OBSERVABILITY_MEASURES: readonly string[] = [
	'llm_completions',
	'llm_tokens_total',
	'llm_cost_micros',
	'tasks_completed',
	'tasks_failed',
	'tasks_cancelled'
];

/** Look up one closed measure's display metadata (null when the key is
 * not part of the closed set — the caller treats it as unavailable). */
export function measureMeta(key: string): MeasureMeta | null {
	return OBSERVABILITY_MEASURES.find((m) => m.key === key) ?? null;
}

/** The operator-facing label for a measure key (falls back to the raw
 * key when the key is not in the closed set — never fabricates one). */
export function measureLabel(key: string): string {
	return measureMeta(key)?.label ?? key;
}

/* ================================================================== */
/* UTC-grid-aligned window                                            */
/* ================================================================== */

export const MINUTE_MS = 60_000;
export const HOUR_MS = 3_600_000;
export const DAY_MS = 86_400_000;

/** The fixed UTC grid unit for a bucket (rows are stored at the UTC
 * minute grid and coarsened at read time). */
export function bucketMs(bucket: ObservabilityBucket): number {
	switch (bucket) {
		case 'minute':
			return MINUTE_MS;
		case 'hour':
			return HOUR_MS;
		case 'day':
			return DAY_MS;
	}
}

/** Floor an epoch-ms instant onto the bucket's UTC grid. */
export function alignFloorUtc(ms: number, bucket: ObservabilityBucket): number {
	const unit = bucketMs(bucket);
	return Math.floor(ms / unit) * unit;
}

/** Ceil an epoch-ms instant onto the bucket's UTC grid (a zero `to`
 * edge becomes the next grid boundary). */
export function alignCeilUtc(ms: number, bucket: ObservabilityBucket): number {
	const unit = bucketMs(bucket);
	return Math.ceil(ms / unit) * unit;
}

/** True when an epoch-ms instant lies exactly on the bucket's UTC grid. */
export function isAlignedUtc(ms: number, bucket: ObservabilityBucket): boolean {
	return ms % bucketMs(bucket) === 0;
}

/** True when an RFC-3339 instant lies exactly on the bucket's UTC grid
 * (the wire's mandatory-window precondition). */
export function isAlignedIso(iso: string, bucket: ObservabilityBucket): boolean {
	const ms = Date.parse(iso);
	if (Number.isNaN(ms)) return false;
	return isAlignedUtc(ms, bucket);
}

/** Render an epoch-ms instant as an RFC-3339 UTC string (the wire's
 * `from` / `to` shape). */
export function toUtcIso(ms: number): string {
	return new Date(ms).toISOString();
}

/** One window preset for a bucket. */
export interface WindowPreset {
	/** The stable preset id, unique within a bucket. */
	id: string;
	/** The operator-facing label. */
	label: string;
	/** The half-open window length in ms. */
	durationMs: number;
}

/** The bounded preset windows, keyed by bucket. The `to` edge is always
 * floored to the current bucket boundary so every preset window is
 * grid-aligned by construction. */
export const WINDOW_PRESETS: Readonly<Record<ObservabilityBucket, readonly WindowPreset[]>> = {
	minute: [
		{ id: '15m', label: 'Last 15 min', durationMs: 15 * MINUTE_MS },
		{ id: '60m', label: 'Last 60 min', durationMs: 60 * MINUTE_MS }
	],
	hour: [
		{ id: '24h', label: 'Last 24 h', durationMs: 24 * HOUR_MS },
		{ id: '72h', label: 'Last 72 h', durationMs: 72 * HOUR_MS }
	],
	day: [
		{ id: '7d', label: 'Last 7 d', durationMs: 7 * DAY_MS },
		{ id: '30d', label: 'Last 30 d', durationMs: 30 * DAY_MS }
	]
};

/** An aligned, wire-ready query window. */
export interface AlignedWindow {
	/** The floored start edge (epoch ms). */
	fromMs: number;
	/** The ceiled end edge (epoch ms). */
	toMs: number;
	/** RFC-3339 UTC start edge. */
	from: string;
	/** RFC-3339 UTC end edge. */
	to: string;
}

/** Build the aligned window for a preset id at a given "now". Falls back
 * to the bucket's first preset for an unknown id (never throws). */
export function presetWindow(
	presetId: string,
	bucket: ObservabilityBucket,
	nowMs: number
): AlignedWindow {
	const preset =
		WINDOW_PRESETS[bucket].find((p) => p.id === presetId) ?? WINDOW_PRESETS[bucket][0];
	const toMs = alignFloorUtc(nowMs, bucket);
	const fromMs = toMs - preset.durationMs;
	return { fromMs, toMs, from: toUtcIso(fromMs), to: toUtcIso(toMs) };
}

/** Align an arbitrary [from, to) pair onto the bucket's UTC grid — floor
 * the start, ceil the end, and enforce at least one full bucket. The
 * wire rejects unaligned edges loudly, so the consumer NEVER sends a raw
 * operator instant; this is the single alignment choke point. */
export function alignWindow(
	fromMs: number,
	toMs: number,
	bucket: ObservabilityBucket
): AlignedWindow {
	const f = alignFloorUtc(fromMs, bucket);
	let t = alignCeilUtc(toMs, bucket);
	if (t <= f) {
		t = f + bucketMs(bucket);
	}
	return { fromMs: f, toMs: t, from: toUtcIso(f), to: toUtcIso(t) };
}

/** A human UTC label for the window, e.g. "2026-08-14 05:00 → 06:00 UTC". */
export function windowLabel(fromMs: number, toMs: number, bucket: ObservabilityBucket): string {
	return `${formatBucketStart(toUtcIso(fromMs), bucket)} → ${formatBucketStart(
		toUtcIso(toMs),
		bucket
	)} UTC`;
}

/* ================================================================== */
/* Exact integer / micro-unit formatting (no float anywhere)           */
/* ================================================================== */

/**
 * Render an exact integer value as its decimal string using integer
 * arithmetic over the value AS RECEIVED. `BigInt(Math.trunc(n))` is the
 * exact conversion for any integral JS number — no float division, no
 * `toFixed`, no exponent form — so a value that arrived exactly stays
 * exactly rendered. Returns `null` for a non-finite payload (the view
 * renders "—", never an ambiguous zero).
 */
export function formatExactInteger(n: number): string | null {
	if (!Number.isFinite(n)) return null;
	return BigInt(Math.trunc(n)).toString();
}

/**
 * Render an integer-with-fixed-scale value exactly: `N / Scale` in pure
 * integer arithmetic. Scale 1 renders an integer string; scale 1_000_000
 * renders "123" or "123.456789" with the fractional part zero-padded to
 * the scale's digit count. Returns `null` for a non-finite or non-
 * positive-scale payload.
 */
export function formatScaledInteger(n: number, scale: number): string | null {
	if (!Number.isFinite(n) || !Number.isInteger(scale) || scale <= 0) return null;
	const value = BigInt(Math.trunc(n));
	const divisor = BigInt(scale);
	const negative = value < 0n;
	const abs = negative ? -value : value;
	const units = abs / divisor;
	const frac = abs % divisor;
	if (frac === 0n) {
		return `${negative ? '-' : ''}${units.toString()}`;
	}
	const digits = scale.toString().length - 1;
	const fracText = frac.toString().padStart(digits, '0');
	return `${negative ? '-' : ''}${units.toString()}.${fracText}`;
}

/** Render one wire measure value exactly (integer measures as integers,
 * cost as N/1_000_000 decimal USD). `null` when the value is not
 * finite — the caller renders the unavailable marker, never a zero. */
export function formatMeasureValue(mv: ObservabilityMeasureValue): string | null {
	return formatScaledInteger(mv.n, mv.scale);
}

/**
 * The exact text of a measure for one row, or `null` when the row did
 * not carry the requested measure. A missing measure is "unavailable",
 * NEVER a zero — the Phase 247 honesty contract (an unsupported measure
 * is omitted, never synthesized as 0).
 */
export function rowMeasureText(
	measures: Readonly<Record<string, ObservabilityMeasureValue>>,
	key: string
): string | null {
	const mv = measures[key];
	if (mv === undefined) return null;
	return formatMeasureValue(mv);
}

/** The exact decimal string of the rollup watermark (a uint64 sequence),
 * rendered via integer arithmetic — never float-normalised. `null` when
 * the wire carried a non-finite value. */
export function formatWatermark(n: number): string | null {
	return formatExactInteger(n);
}

/* ================================================================== */
/* Freshness / retention presentation                                  */
/* ================================================================== */

/** The StatusChip kind for a freshness state. */
export type QualityKind = 'success' | 'warning' | 'danger' | 'neutral';

/** The freshness state → chip kind: current = success, catching_up =
 * warning, unavailable = danger, anything else = neutral (never a
 * fabricated state). */
export function qualityKind(state: string): QualityKind {
	switch (state) {
		case 'current':
			return 'success';
		case 'catching_up':
			return 'warning';
		case 'unavailable':
			return 'danger';
		default:
			return 'neutral';
	}
}

/** The operator-facing freshness-state label. */
export function qualityLabel(state: string): string {
	switch (state) {
		case 'current':
			return 'Current';
		case 'catching_up':
			return 'Catching up';
		case 'unavailable':
			return 'Unavailable';
		default:
			return state;
	}
}

/** The retention-coverage value → chip kind: covered = success, partial =
 * warning, gap = danger. */
export function coverageKind(coverage: string): QualityKind {
	switch (coverage) {
		case 'covered':
			return 'success';
		case 'partial':
			return 'warning';
		case 'gap':
			return 'danger';
		default:
			return 'neutral';
	}
}

/** The operator-facing coverage label. */
export function coverageLabel(coverage: string): string {
	switch (coverage) {
		case 'covered':
			return 'Covered';
		case 'partial':
			return 'Partial';
		case 'gap':
			return 'Gap';
		default:
			return coverage;
	}
}

/** True when the freshness stamp reports the projection is not usable
 * for totals — the view must surface this loudly and never let a zero
 * row masquerade as "no data" (Phase 247 honesty). */
export function isQualityUnavailable(q: ObservabilityQualityBlock): boolean {
	return q.state === 'unavailable';
}

/** True when the requested window falls wholly outside the retained
 * horizon (or nothing is retained) — the rows are exact but the totals
 * are empty BY RETENTION, which is distinct from a plain empty window. */
export function isRetentionGap(q: ObservabilityQualityBlock): boolean {
	return q.coverage === 'gap';
}

/** The retained-horizon label ("—" when the store holds no rows). */
export function retentionHorizonLabel(q: ObservabilityQualityBlock): string {
	if (!q.retention_start || !q.retention_end) return '—';
	return `${formatBucketStart(q.retention_start, 'minute')} → ${formatBucketStart(
		q.retention_end,
		'minute'
	)} UTC`;
}

/* ================================================================== */
/* Row / dimension presentation                                        */
/* ================================================================== */

/** A compact UTC label for a bucket-start instant, coarse to the bucket:
 * day "2026-08-14", hour "2026-08-14 05:00", minute "2026-08-14 05:03". */
export function formatBucketStart(iso: string, bucket: ObservabilityBucket): string {
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return iso;
	const yyyy = d.getUTCFullYear();
	const mm = String(d.getUTCMonth() + 1).padStart(2, '0');
	const dd = String(d.getUTCDate()).padStart(2, '0');
	const hh = String(d.getUTCHours()).padStart(2, '0');
	const mi = String(d.getUTCMinutes()).padStart(2, '0');
	if (bucket === 'day') return `${yyyy}-${mm}-${dd}`;
	if (bucket === 'hour') return `${yyyy}-${mm}-${dd} ${hh}:00`;
	return `${yyyy}-${mm}-${dd} ${hh}:${mi}`;
}

/** The cell text for one authoritative dimension value. An empty model
 * value is the wire's "un-attributed" marker (authoritative — the source
 * payload carried no model) and reads as "unattributed", never a zero or
 * a fabricated label. */
export function dimensionCellText(dim: ObservabilityDimension, value: string): string {
	if (value === '') return dim === 'model' ? 'unattributed' : '—';
	return value;
}

/** A stable table row key — the bucket plus the grouped dimension values
 * (the deterministic total order makes this unique per page). */
export function rowKey(row: {
	bucket_start: string;
	dimensions?: Readonly<Record<string, string>>;
}): string {
	const dims = row.dimensions ?? {};
	const dimPart = Object.keys(dims)
		.sort()
		.map((k) => `${k}=${dims[k]}`)
		.join('&');
	return `${row.bucket_start}${dimPart !== '' ? `#${dimPart}` : ''}`;
}

/** Parse a comma-separated model filter input into the wire's closed
 * `models` string[] (trimmed, empty parts dropped, deduplicated). */
export function parseModelFilter(text: string): string[] {
	const seen = new Set<string>();
	const out: string[] = [];
	for (const part of text.split(',')) {
		const trimmed = part.trim();
		if (trimmed !== '' && !seen.has(trimmed)) {
			seen.add(trimmed);
			out.push(trimmed);
		}
	}
	return out;
}

/** The default page size for the bounded query. */
export const DEFAULT_OBSERVABILITY_LIMIT = 100;

/** The closed page-size options the pager offers (1..MaxRowsPerQuery). */
export const OBSERVABILITY_PAGE_SIZES = [50, 100, 250, 500] as const;
