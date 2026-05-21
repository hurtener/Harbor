/**
 * Events page — `sparkline.ts` unit tests (Phase 73g / D-125).
 *
 * Pins: `projectBuckets` folds the wire series and the stacked total of
 * every column equals the sum of its per-type counts; `rebucket`
 * coarsens correctly; the y-peak tracks the largest stacked total.
 */
import { describe, expect, it } from 'vitest';
import { projectBuckets, rebucket } from '../sparkline.js';
import type { EventBucket } from '../../protocol/events.js';

function bucket(startISO: string, counts: Record<string, number>): EventBucket {
	return { bucket_start: startISO, bucket_end: startISO, counts };
}

describe('events sparkline: projectBuckets', () => {
	it('stacked total of a column equals the sum of its per-type counts', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', { 'tool.failed': 3, 'planner.error': 2 }),
			bucket('2026-05-20T12:01:00Z', { 'tool.failed': 5 })
		]);
		expect(series.columns[0].total).toBe(5);
		expect(series.columns[1].total).toBe(5);
	});

	it('collects the distinct event-type set across columns, sorted', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', { 'tool.failed': 1 }),
			bucket('2026-05-20T12:01:00Z', { 'planner.error': 1, 'audit.admin_scope_used': 1 })
		]);
		expect(series.eventTypes).toEqual(['audit.admin_scope_used', 'planner.error', 'tool.failed']);
	});

	it('preserves empty buckets so the time axis stays contiguous', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', {}),
			bucket('2026-05-20T12:01:00Z', { 'tool.failed': 4 }),
			bucket('2026-05-20T12:02:00Z', {})
		]);
		expect(series.columns).toHaveLength(3);
		expect(series.columns[0].total).toBe(0);
		expect(series.columns[2].total).toBe(0);
	});

	it('the peak is the largest stacked total', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', { a: 2, b: 1 }),
			bucket('2026-05-20T12:01:00Z', { a: 7 }),
			bucket('2026-05-20T12:02:00Z', { a: 3 })
		]);
		expect(series.peak).toBe(7);
	});
});

describe('events sparkline: rebucket', () => {
	it('factor 1 is the identity', () => {
		const series = projectBuckets([bucket('2026-05-20T12:00:00Z', { a: 1 })]);
		expect(rebucket(series, 1)).toBe(series);
	});

	it('merges every `factor` consecutive columns, summing counts', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', { a: 1 }),
			bucket('2026-05-20T12:00:30Z', { a: 2, b: 1 }),
			bucket('2026-05-20T12:01:00Z', { a: 3 }),
			bucket('2026-05-20T12:01:30Z', { b: 4 })
		]);
		const coarse = rebucket(series, 2);
		expect(coarse.columns).toHaveLength(2);
		// Column 0 = buckets[0..1]: a=3, b=1.
		expect(coarse.columns[0].counts).toEqual({ a: 3, b: 1 });
		// Column 1 = buckets[2..3]: a=3, b=4.
		expect(coarse.columns[1].counts).toEqual({ a: 3, b: 4 });
	});

	it('rebucketed stacked totals still equal the per-type sum', () => {
		const series = projectBuckets([
			bucket('2026-05-20T12:00:00Z', { a: 1, b: 2 }),
			bucket('2026-05-20T12:00:30Z', { a: 3 })
		]);
		const coarse = rebucket(series, 2);
		expect(coarse.columns[0].total).toBe(6);
		expect(coarse.peak).toBe(6);
	});
});
