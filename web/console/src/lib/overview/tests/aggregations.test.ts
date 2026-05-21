/**
 * Overview page — `aggregations.ts` unit tests (Phase 73a / D-127).
 *
 * Pins the windowed event-rate aggregation correctness the plan names
 * as the Console-side Vitest surface — bucket-boundary placement, the
 * gap-free fixed-length series, the 1m / 5m / 15m windows, the flat-zero
 * empty window, and the unparseable-timestamp drop (CLAUDE.md §13 — a
 * mis-buckable event is dropped, never silently mis-counted).
 */
import { describe, expect, it } from 'vitest';
import {
	aggregateRate,
	bucketStart,
	countByType,
	eventsPerMinute,
	WINDOW_BUCKETS,
	type CounterWindow
} from '../aggregations.js';
import type { Event } from '../../protocol/events.js';

const MINUTE = 60_000;
// A fixed reference instant on a minute boundary keeps the arithmetic
// readable: 2026-05-20T12:00:00Z.
const NOW = Date.parse('2026-05-20T12:00:00Z');

function ev(type: string, occurredAt: string, sequence = 1): Event {
	return {
		type,
		sequence,
		occurred_at: occurredAt,
		tenant: 'console',
		user: 'operator',
		session: 's1'
	};
}

describe('aggregations: bucketStart', () => {
	it('floors an instant to its containing minute', () => {
		const mid = Date.parse('2026-05-20T12:00:37Z');
		expect(bucketStart(mid)).toBe(NOW);
	});
});

describe('aggregations: aggregateRate', () => {
	it('produces a fixed-length gap-free series per window', () => {
		for (const w of ['1m', '5m', '15m'] as CounterWindow[]) {
			const series = aggregateRate([], w, NOW);
			expect(series.buckets).toHaveLength(WINDOW_BUCKETS[w]);
			// The series is contiguous — each bucket is one minute after
			// the previous.
			for (let i = 1; i < series.buckets.length; i += 1) {
				expect(series.buckets[i].startMillis - series.buckets[i - 1].startMillis).toBe(
					MINUTE
				);
			}
		}
	});

	it('an empty window yields all-zero buckets and a flat-zero peak', () => {
		const series = aggregateRate([], '5m', NOW);
		expect(series.peak).toBe(0);
		expect(series.currentRate).toBe(0);
		expect(series.buckets.every((b) => b.count === 0)).toBe(true);
	});

	it('places an event in the minute bucket its timestamp falls into', () => {
		// Three events in the current minute, one in the previous minute.
		const events: Event[] = [
			ev('task.completed', '2026-05-20T12:00:05Z'),
			ev('task.completed', '2026-05-20T12:00:42Z'),
			ev('task.failed', '2026-05-20T12:00:59Z'),
			ev('task.completed', '2026-05-20T11:59:30Z')
		];
		const series = aggregateRate(events, '5m', NOW);
		// 5m window: buckets 11:56..12:00. The current (last) bucket has
		// 3, the prior has 1.
		expect(series.buckets[series.buckets.length - 1].count).toBe(3);
		expect(series.buckets[series.buckets.length - 2].count).toBe(1);
		expect(series.peak).toBe(3);
		expect(series.currentRate).toBe(3);
	});

	it('ignores events outside the window', () => {
		// An event 20 minutes old falls outside a 15m window.
		const old = new Date(NOW - 20 * MINUTE).toISOString();
		const series = aggregateRate([ev('task.completed', old)], '15m', NOW);
		expect(series.peak).toBe(0);
	});

	it('drops an event with an unparseable timestamp (no mis-bucketing)', () => {
		const series = aggregateRate([ev('task.completed', 'not-a-date')], '5m', NOW);
		expect(series.peak).toBe(0);
	});
});

describe('aggregations: eventsPerMinute', () => {
	it('is the windowed mean of the bucket counts', () => {
		// 5m window, 10 events in the current minute → mean 10/5 = 2.
		const events: Event[] = [];
		for (let i = 0; i < 10; i += 1) {
			events.push(ev('task.completed', '2026-05-20T12:00:01Z', i));
		}
		const series = aggregateRate(events, '5m', NOW);
		expect(eventsPerMinute(series)).toBe(2);
	});

	it('is zero for an empty series', () => {
		expect(eventsPerMinute(aggregateRate([], '1m', NOW))).toBe(0);
	});
});

describe('aggregations: countByType', () => {
	it('folds per-type counts over the window', () => {
		const events: Event[] = [
			ev('task.failed', '2026-05-20T12:00:01Z'),
			ev('task.failed', '2026-05-20T12:00:02Z'),
			ev('session.opened', '2026-05-20T11:58:00Z')
		];
		const counts = countByType(events, '5m', NOW);
		expect(counts['task.failed']).toBe(2);
		expect(counts['session.opened']).toBe(1);
	});

	it('excludes events outside the window', () => {
		const old = new Date(NOW - 30 * MINUTE).toISOString();
		const counts = countByType([ev('task.failed', old)], '5m', NOW);
		expect(counts['task.failed']).toBeUndefined();
	});
});
