/**
 * Events page — pure sparkline bucket-aggregation helpers
 * (Phase 73g / D-125).
 *
 * The event-rate sparkline is a per-event-type stacked-area chart over
 * the active window. Its data source is `events.aggregate` (Phase 72a),
 * which returns a deterministic `EventBucket[]` series. This module is
 * the PURE projection layer: it folds the wire `EventBucket[]` into the
 * stripe / stacked-total shape `EventRateSparkline.svelte` renders, and
 * it can rebucket an existing series when the operator switches windows
 * (so a window change does not always force an `events.aggregate`
 * re-fetch). No `$state`, no Protocol call — unit-tested.
 */

import type { EventBucket } from '$lib/protocol/harbor.js';

/** One rendered column of the stacked-area sparkline. */
export interface SparklineColumn {
	/** The bucket's lower bound (inclusive, RFC-3339 UTC) — the x label. */
	startISO: string;
	/** Per-event-type count for this column. */
	counts: Record<string, number>;
	/** The stacked total — sum of every type's count. */
	total: number;
}

/** The fully-projected sparkline: columns + the type set + the y-max. */
export interface SparklineSeries {
	/** Columns, oldest-first. */
	columns: SparklineColumn[];
	/** The distinct event-type names present across all columns, sorted. */
	eventTypes: string[];
	/** The largest stacked total across columns — the y-axis ceiling. */
	peak: number;
}

/**
 * `projectBuckets` folds a wire `EventBucket[]` into the rendered
 * {@link SparklineSeries}. Empty buckets are preserved (the wire series
 * is gap-free by construction — `types.EventBucket` godoc) so the chart
 * scans a contiguous time axis. The stacked total of every column is
 * the sum of its per-type counts — the test asserts this invariant.
 */
export function projectBuckets(buckets: EventBucket[]): SparklineSeries {
	const typeSet = new Set<string>();
	let peak = 0;
	const columns: SparklineColumn[] = buckets.map((b) => {
		let total = 0;
		const counts: Record<string, number> = {};
		for (const [type, n] of Object.entries(b.counts)) {
			counts[type] = n;
			total += n;
			typeSet.add(type);
		}
		if (total > peak) {
			peak = total;
		}
		return { startISO: b.bucket_start, counts, total };
	});
	return {
		columns,
		eventTypes: [...typeSet].sort(),
		peak
	};
}

/**
 * `rebucket` coarsens a fine-grained series into wider buckets by
 * merging every `factor` consecutive columns. It lets the page switch
 * from a finer window (e.g. 5 min / 30 s buckets) to a coarser one
 * without an `events.aggregate` re-fetch when the underlying time span
 * is a superset. `factor` must be ≥ 1; `factor === 1` is the identity.
 *
 * Note this only COARSENS — a switch to a FINER window does need a
 * re-fetch (the runtime is the authority for sub-bucket counts); the
 * page calls `events.aggregate` in that direction.
 */
export function rebucket(series: SparklineSeries, factor: number): SparklineSeries {
	if (factor <= 1) {
		return series;
	}
	const merged: EventBucket[] = [];
	for (let i = 0; i < series.columns.length; i += factor) {
		const slice = series.columns.slice(i, i + factor);
		const counts: Record<string, number> = {};
		for (const col of slice) {
			for (const [type, n] of Object.entries(col.counts)) {
				counts[type] = (counts[type] ?? 0) + n;
			}
		}
		merged.push({
			bucket_start: slice[0].startISO,
			bucket_end: slice[slice.length - 1].startISO,
			counts
		});
	}
	return projectBuckets(merged);
}
