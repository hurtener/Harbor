/**
 * Overview page — `cost.ts` unit tests (Phase 73a / D-127).
 *
 * Pins the cost-rollup projection: the `llm.cost.recorded` filter, the
 * per-agent / per-tenant grouping, the descending sort, the grand
 * total, and the defensive payload parse (a malformed cost event is
 * SKIPPED, never folded as a misleading zero — CLAUDE.md §13).
 */
import { describe, expect, it } from 'vitest';
import { extractCostUSD, projectCost } from '../cost.js';
import type { Event } from '../../protocol/events.js';

function costEvent(
	tenant: string,
	model: string,
	total: number,
	sequence: number
): Event {
	return {
		type: 'llm.cost.recorded',
		sequence,
		occurred_at: '2026-05-20T12:00:00Z',
		tenant,
		user: 'operator',
		session: 's1',
		payload: { Model: model, Cost: { TotalCost: total, Currency: 'USD' } }
	};
}

describe('cost: extractCostUSD', () => {
	it('reads the nested Cost.TotalCost field', () => {
		expect(extractCostUSD(costEvent('t1', 'gpt', 1.25, 1))).toBe(1.25);
	});

	it('returns null for a payload with no cost object', () => {
		const ev: Event = {
			type: 'llm.cost.recorded',
			sequence: 1,
			occurred_at: '2026-05-20T12:00:00Z',
			tenant: 't1',
			user: 'u',
			session: 's',
			payload: { Model: 'gpt' }
		};
		expect(extractCostUSD(ev)).toBeNull();
	});

	it('returns null for a missing / non-object payload', () => {
		const ev: Event = {
			type: 'llm.cost.recorded',
			sequence: 1,
			occurred_at: '2026-05-20T12:00:00Z',
			tenant: 't1',
			user: 'u',
			session: 's'
		};
		expect(extractCostUSD(ev)).toBeNull();
	});
});

describe('cost: projectCost', () => {
	it('groups by agent (Model) and sums per-key', () => {
		const rollup = projectCost(
			[
				costEvent('t1', 'research-agent', 1.0, 1),
				costEvent('t1', 'research-agent', 0.5, 2),
				costEvent('t1', 'support-agent', 2.0, 3)
			],
			'agent'
		);
		expect(rollup.totalUSD).toBeCloseTo(3.5);
		// Descending by cost — support-agent (2.0) before research-agent (1.5).
		expect(rollup.rows[0].key).toBe('support-agent');
		expect(rollup.rows[1].key).toBe('research-agent');
		expect(rollup.rows[1].costUSD).toBeCloseTo(1.5);
		expect(rollup.rows[1].events).toBe(2);
	});

	it('groups by tenant when the breakdown is per-tenant (admin)', () => {
		const rollup = projectCost(
			[costEvent('tenant-a', 'm', 1.0, 1), costEvent('tenant-b', 'm', 3.0, 2)],
			'tenant'
		);
		expect(rollup.breakdown).toBe('tenant');
		expect(rollup.rows.map((r) => r.key)).toEqual(['tenant-b', 'tenant-a']);
	});

	it('ignores non-cost events', () => {
		const other: Event = {
			type: 'task.completed',
			sequence: 9,
			occurred_at: '2026-05-20T12:00:00Z',
			tenant: 't1',
			user: 'u',
			session: 's'
		};
		const rollup = projectCost([other, costEvent('t1', 'm', 1.0, 1)], 'agent');
		expect(rollup.totalUSD).toBe(1.0);
		expect(rollup.rows).toHaveLength(1);
	});

	it('skips a malformed cost event rather than folding a zero', () => {
		const malformed: Event = {
			type: 'llm.cost.recorded',
			sequence: 1,
			occurred_at: '2026-05-20T12:00:00Z',
			tenant: 't1',
			user: 'u',
			session: 's',
			payload: { Model: 'm' } // no Cost object
		};
		const rollup = projectCost([malformed, costEvent('t1', 'm', 2.0, 2)], 'agent');
		// Only the well-formed event is folded — the agent row has 1
		// event, not 2.
		expect(rollup.rows[0].events).toBe(1);
		expect(rollup.totalUSD).toBe(2.0);
	});
});
