/**
 * Events page — `filters.ts` unit tests (Phase 73g / D-125).
 *
 * Pins: `compileFilter` round-trips the faceted state onto the
 * `EventFilter` wire shape; the identity quadruple propagates; the
 * window derives a `since` lower bound; cross-tenant detection; and the
 * pure `toggleEventType` helper.
 */
import { describe, expect, it } from 'vitest';
import {
	aggregateFilter,
	compileFilter,
	defaultFacetState,
	isCrossTenant,
	toggleEventType,
	type EventFacetState
} from '../filters.js';

describe('events filters: compileFilter', () => {
	it('emits no identity axis for the default (unpinned) state', () => {
		const filter = compileFilter(defaultFacetState(), new Date('2026-05-20T12:00:00Z'));
		expect(filter.event_types).toBeUndefined();
		expect(filter.tenant_ids).toBeUndefined();
		expect(filter.user_ids).toBeUndefined();
		expect(filter.session_ids).toBeUndefined();
		expect(filter.run_ids).toBeUndefined();
	});

	it('derives a since lower bound from the active window', () => {
		const state: EventFacetState = { ...defaultFacetState(), window: '1h' };
		const now = new Date('2026-05-20T12:00:00Z');
		const filter = compileFilter(state, now);
		// 1 h window → since = now - 1h.
		expect(filter.since).toBe('2026-05-20T11:00:00.000Z');
	});

	it('projects every pinned identity axis onto the wire shape', () => {
		const state: EventFacetState = {
			eventTypes: ['tool.failed', 'planner.error'],
			tenant: 'tenant-x',
			user: 'user-y',
			session: 'sess-z',
			run: 'run-1',
			window: '24h'
		};
		const filter = compileFilter(state, new Date('2026-05-20T12:00:00Z'));
		// event_types are sorted for a stable wire payload.
		expect(filter.event_types).toEqual(['planner.error', 'tool.failed']);
		expect(filter.tenant_ids).toEqual(['tenant-x']);
		expect(filter.user_ids).toEqual(['user-y']);
		expect(filter.session_ids).toEqual(['sess-z']);
		expect(filter.run_ids).toEqual(['run-1']);
	});

	it('a 7 d window yields a since exactly 7 days back', () => {
		const state: EventFacetState = { ...defaultFacetState(), window: '7d' };
		const filter = compileFilter(state, new Date('2026-05-20T00:00:00Z'));
		expect(filter.since).toBe('2026-05-13T00:00:00.000Z');
	});
});

describe('events filters: aggregateFilter (D-306 grid, D-308 widened fan-in)', () => {
	it('drops the window-derived since so the grid stays anchor-determined', () => {
		const state: EventFacetState = { ...defaultFacetState(), window: '1h' };
		const filter = aggregateFilter(state);
		expect(filter.since).toBeUndefined();
	});

	it('keeps own-scope identity axes untouched', () => {
		const state: EventFacetState = {
			...defaultFacetState(),
			tenant: 'tenant-own',
			user: 'user-y',
			session: 'sess-z'
		};
		const filter = aggregateFilter(state);
		expect(filter.tenant_ids).toEqual(['tenant-own']);
		expect(filter.user_ids).toEqual(['user-y']);
		expect(filter.session_ids).toEqual(['sess-z']);
	});

	it('PRESERVES a foreign tenant pin with no user/session (the runtime fans it in, D-308)', () => {
		const state: EventFacetState = { ...defaultFacetState(), tenant: 'tenant-other' };
		const filter = aggregateFilter(state);
		// No longer dropped: the widened, scope-gated, audited aggregate fans
		// in across the named tenant's users/sessions server-side (D-308), so
		// the sparkline shows the real cross-tenant rate — not an own-scope
		// fallback and not a silently-blank grid.
		expect(filter.tenant_ids).toEqual(['tenant-other']);
		expect(filter.user_ids).toBeUndefined();
		expect(filter.session_ids).toBeUndefined();
	});

	it('preserves a foreign tenant pin narrowed by a session too', () => {
		const state: EventFacetState = {
			...defaultFacetState(),
			tenant: 'tenant-other',
			session: 'sess-target'
		};
		const filter = aggregateFilter(state);
		expect(filter.tenant_ids).toEqual(['tenant-other']);
		expect(filter.session_ids).toEqual(['sess-target']);
	});
});

describe('events filters: isCrossTenant', () => {
	it('is false when no tenant is pinned', () => {
		expect(isCrossTenant(defaultFacetState(), 'tenant-own')).toBe(false);
	});

	it('is false when the pinned tenant is the operator own tenant', () => {
		const state: EventFacetState = { ...defaultFacetState(), tenant: 'tenant-own' };
		expect(isCrossTenant(state, 'tenant-own')).toBe(false);
	});

	it('is true when the pinned tenant differs from the operator tenant', () => {
		const state: EventFacetState = { ...defaultFacetState(), tenant: 'tenant-other' };
		expect(isCrossTenant(state, 'tenant-own')).toBe(true);
	});
});

describe('events filters: toggleEventType', () => {
	it('pins a type that was not present', () => {
		const next = toggleEventType(defaultFacetState(), 'tool.failed');
		expect(next.eventTypes).toEqual(['tool.failed']);
	});

	it('unpins a type that was already present', () => {
		const start: EventFacetState = {
			...defaultFacetState(),
			eventTypes: ['tool.failed', 'planner.error']
		};
		const next = toggleEventType(start, 'tool.failed');
		expect(next.eventTypes).toEqual(['planner.error']);
	});

	it('returns a new object (immutability — no in-place mutation)', () => {
		const start = defaultFacetState();
		const next = toggleEventType(start, 'tool.failed');
		expect(next).not.toBe(start);
		expect(start.eventTypes).toEqual([]);
	});
});
