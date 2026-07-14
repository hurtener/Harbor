/**
 * Events page — `attribution.ts` unit tests (D-307 / HA-17).
 *
 * Pins: `projectTenantAttribution` folds `EventBucket.counts_by_tenant`
 * into a per-tenant breakdown that reconciles with the totals
 * (Σ counts_by_tenant == counts), returns null when no bucket carries
 * attribution (non-widened / opted-out reads), flags a tenant outside the
 * entitled set, and detects a non-reconciling breakdown.
 */
import { describe, expect, it } from 'vitest';
import { projectTenantAttribution } from '../attribution.js';
import type { EventBucket } from '../../protocol/events.js';

function bucket(
	counts: Record<string, number>,
	countsByTenant?: Record<string, Record<string, number>>
): EventBucket {
	return {
		bucket_start: '2026-07-14T12:00:00Z',
		bucket_end: '2026-07-14T12:01:00Z',
		counts,
		...(countsByTenant ? { counts_by_tenant: countsByTenant } : {})
	};
}

describe('events attribution: projectTenantAttribution', () => {
	it('returns null when no bucket carries counts_by_tenant', () => {
		const attr = projectTenantAttribution(
			[bucket({ 'runtime.error': 3 }), bucket({})],
			['t-a']
		);
		expect(attr).toBeNull();
	});

	it('folds per-tenant counts across buckets and reconciles with the totals', () => {
		const attr = projectTenantAttribution(
			[
				bucket(
					{ 'runtime.error': 3 },
					{ 't-a': { 'runtime.error': 2 }, 't-b': { 'runtime.error': 1 } }
				),
				bucket({ 'runtime.error': 2 }, { 't-a': { 'runtime.error': 2 } }),
				bucket({}) // empty bucket — no attribution
			],
			['t-a', 't-b']
		);
		expect(attr).not.toBeNull();
		expect(attr!.reconciles).toBe(true);
		expect(attr!.hasUnexpected).toBe(false);
		expect(attr!.total).toBe(5);
		// Rows sorted by total desc: t-a (4) then t-b (1).
		expect(attr!.rows.map((r) => r.tenant)).toEqual(['t-a', 't-b']);
		expect(attr!.rows[0].total).toBe(4);
		expect(attr!.rows[1].total).toBe(1);
		expect(attr!.rows[0].byType).toEqual({ 'runtime.error': 4 });
	});

	it('flags a tenant outside the entitled set as unexpected', () => {
		const attr = projectTenantAttribution(
			[bucket({ 'runtime.error': 2 }, { 't-a': { 'runtime.error': 1 }, 't-x': { 'runtime.error': 1 } })],
			['t-a'] // only t-a is entitled
		);
		expect(attr).not.toBeNull();
		expect(attr!.hasUnexpected).toBe(true);
		const xRow = attr!.rows.find((r) => r.tenant === 't-x');
		expect(xRow?.unexpected).toBe(true);
		const aRow = attr!.rows.find((r) => r.tenant === 't-a');
		expect(aRow?.unexpected).toBe(false);
	});

	it('detects a non-reconciling breakdown (Σ counts_by_tenant != counts)', () => {
		const attr = projectTenantAttribution(
			// counts says 5 but attribution only accounts for 3 — a runtime bug shape.
			[bucket({ 'runtime.error': 5 }, { 't-a': { 'runtime.error': 3 } })],
			['t-a']
		);
		expect(attr).not.toBeNull();
		expect(attr!.reconciles).toBe(false);
	});
});
