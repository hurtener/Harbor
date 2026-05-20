/**
 * Overview page — pure client-side event-rate aggregation
 * (Phase 73a / D-127).
 *
 * The Overview counter cards (Events/min, Tasks Running, Background
 * Jobs, MCP Connections) each render a mini-sparkline showing the
 * trend over the selected window. Phase 73a ships NO new Protocol
 * method: the sparkline is folded CLIENT-SIDE from the `events.subscribe`
 * SSE cursor (`Event[]`, newest-first) — exactly the windowed event-rate
 * aggregation page-overview.md §12 calls a `[shipped]` subscription-
 * derived counter, no runtime work.
 *
 * This module is the PURE projection layer: it folds an `Event[]` slice
 * into per-bucket counts over a fixed-width window. No `$state`, no
 * Protocol call, no clock dependency beyond the injected `now` — fully
 * unit-tested (the plan's named Vitest surface).
 *
 * # Why per-minute, not per-second (page-overview.md §12)
 *
 * The mockup uses per-MINUTE aggregation for the counter cards —
 * per-second buckets are too noisy for an at-a-glance hub counter and
 * stay in the Events page's dedicated rate chart. The `1m` / `5m` /
 * `15m` windows below all bucket at one-minute resolution.
 */

import type { Event } from '$lib/protocol/harbor.js';

/** The closed set of counter-card aggregation windows. */
export type CounterWindow = '1m' | '5m' | '15m';

/** The full window list — drives the window selector. */
export const COUNTER_WINDOWS: readonly CounterWindow[] = ['1m', '5m', '15m'] as const;

/** How many one-minute buckets each window spans. */
export const WINDOW_BUCKETS: Record<CounterWindow, number> = {
	'1m': 1,
	'5m': 5,
	'15m': 15
};

/** One bucket of the rate sparkline — a one-minute slot and its event count. */
export interface RateBucket {
	/** The bucket's lower bound, unix-epoch millis (bucket spans `[start, start+60s)`). */
	startMillis: number;
	/** The count of events whose `occurred_at` fell in this bucket. */
	count: number;
}

/** The fully-projected rate series the counter-card sparkline renders. */
export interface RateSeries {
	/** The per-minute buckets, oldest-first; always `WINDOW_BUCKETS[window]` long. */
	buckets: RateBucket[];
	/** The window the series covers. */
	window: CounterWindow;
	/** The largest bucket count — the sparkline y-axis ceiling. */
	peak: number;
	/** The events-per-minute rate of the most recent (current) bucket. */
	currentRate: number;
}

const ONE_MINUTE_MS = 60_000;

/**
 * Folds the lower bound of the minute-bucket `t` falls into.
 * Exported for the unit test's bucket-boundary assertions.
 */
export function bucketStart(t: number): number {
	return Math.floor(t / ONE_MINUTE_MS) * ONE_MINUTE_MS;
}

/**
 * `aggregateRate` folds an `Event[]` slice (newest-first, as the
 * `events.subscribe` cursor delivers it) into a gap-free per-minute
 * {@link RateSeries} over the `window` ending at `now`.
 *
 * The series is ALWAYS `WINDOW_BUCKETS[window]` buckets long — an empty
 * window yields all-zero buckets so the sparkline renders a flat-zero
 * line ("the runtime is up but quiet" — page-overview.md §12), never a
 * collapsed or missing chart. Events outside the window are ignored;
 * an event with an unparseable `occurred_at` is dropped (it cannot be
 * placed on the time axis — never silently mis-bucketed).
 */
export function aggregateRate(
	events: readonly Event[],
	window: CounterWindow,
	now: number
): RateSeries {
	const bucketCount = WINDOW_BUCKETS[window];
	// The newest bucket is the minute `now` falls into; older buckets
	// step back one minute at a time.
	const newestStart = bucketStart(now);
	const oldestStart = newestStart - (bucketCount - 1) * ONE_MINUTE_MS;

	const buckets: RateBucket[] = [];
	for (let i = 0; i < bucketCount; i += 1) {
		buckets.push({ startMillis: oldestStart + i * ONE_MINUTE_MS, count: 0 });
	}

	for (const ev of events) {
		const t = Date.parse(ev.occurred_at);
		if (Number.isNaN(t)) {
			// An unparseable timestamp cannot be placed on the axis — drop
			// it loudly-by-omission rather than mis-bucket it (CLAUDE.md §13).
			continue;
		}
		const start = bucketStart(t);
		const idx = (start - oldestStart) / ONE_MINUTE_MS;
		if (idx < 0 || idx >= bucketCount) {
			continue;
		}
		buckets[idx].count += 1;
	}

	let peak = 0;
	for (const b of buckets) {
		if (b.count > peak) {
			peak = b.count;
		}
	}
	return {
		buckets,
		window,
		peak,
		currentRate: buckets[buckets.length - 1]?.count ?? 0
	};
}

/**
 * `eventsPerMinute` is the headline scalar the Events/min counter card
 * shows: the average events-per-minute across the whole `window`. It is
 * the windowed mean of the {@link RateSeries} bucket counts — a steadier
 * number than the noisy current-bucket `currentRate`.
 */
export function eventsPerMinute(series: RateSeries): number {
	if (series.buckets.length === 0) {
		return 0;
	}
	const total = series.buckets.reduce((sum, b) => sum + b.count, 0);
	return total / series.buckets.length;
}

/**
 * `countByType` folds an `Event[]` slice into a per-type count map over
 * the `window` ending at `now`. The Overview's recent-activity feed and
 * the alert-ribbon notification tally both read it. Events outside the
 * window — or with an unparseable timestamp — are excluded.
 */
export function countByType(
	events: readonly Event[],
	window: CounterWindow,
	now: number
): Record<string, number> {
	const bucketCount = WINDOW_BUCKETS[window];
	const oldest = bucketStart(now) - (bucketCount - 1) * ONE_MINUTE_MS;
	const horizon = bucketStart(now) + ONE_MINUTE_MS;
	const counts: Record<string, number> = {};
	for (const ev of events) {
		const t = Date.parse(ev.occurred_at);
		if (Number.isNaN(t) || t < oldest || t >= horizon) {
			continue;
		}
		counts[ev.type] = (counts[ev.type] ?? 0) + 1;
	}
	return counts;
}
