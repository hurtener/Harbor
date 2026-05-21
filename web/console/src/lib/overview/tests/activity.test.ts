/**
 * Overview page — `activity.ts` unit tests (Phase 73a / D-127).
 *
 * Pins the recent-activity feed projection: the operator-relevant
 * event-type subset filter, the newest-first sort, the row cap, the
 * unprefixed deep-link routing (CONVENTIONS.md §1), and the relative-
 * time rendering.
 */
import { describe, expect, it } from 'vitest';
import { deepLink, projectActivity, relativeTime } from '../activity.js';
import type { Event } from '../../protocol/harbor.js';

function ev(partial: Partial<Event> & { type: string; sequence: number }): Event {
	return {
		occurred_at: '2026-05-20T12:00:00Z',
		tenant: 'console',
		user: 'operator',
		session: 's1',
		...partial
	};
}

describe('activity: projectActivity', () => {
	it('keeps only the operator-relevant event-type subset', () => {
		const rows = projectActivity([
			ev({ type: 'session.opened', sequence: 1 }),
			ev({ type: 'tool.invoked', sequence: 2 }),
			ev({ type: 'task.failed', sequence: 3 })
		]);
		expect(rows.map((r) => r.type).sort()).toEqual(['session.opened', 'task.failed']);
	});

	it('sorts newest-first by sequence', () => {
		const rows = projectActivity([
			ev({ type: 'task.completed', sequence: 5 }),
			ev({ type: 'task.completed', sequence: 9 }),
			ev({ type: 'task.completed', sequence: 1 })
		]);
		expect(rows.map((r) => r.sequence)).toEqual([9, 5, 1]);
	});

	it('caps the feed at the row limit', () => {
		const events: Event[] = [];
		for (let i = 0; i < 50; i += 1) {
			events.push(ev({ type: 'task.completed', sequence: i }));
		}
		expect(projectActivity(events, 25)).toHaveLength(25);
	});

	it('assigns a severity + glyph to each surfaced type', () => {
		const [row] = projectActivity([ev({ type: 'task.failed', sequence: 1 })]);
		expect(row.severity).toBe('danger');
		expect(row.glyph).toBe('T✗');
	});
});

describe('activity: deepLink', () => {
	it('routes a session event to the unprefixed Sessions detail route', () => {
		expect(deepLink(ev({ type: 'session.opened', sequence: 1, session: 'sess-9' }))).toBe(
			'/sessions/sess-9'
		);
	});

	it('routes a run-scoped task event to the Tasks detail route', () => {
		expect(deepLink(ev({ type: 'task.failed', sequence: 1, run: 'run-7' }))).toBe(
			'/tasks/run-7'
		);
	});

	it('routes a run-less task event to the Tasks list route', () => {
		expect(deepLink(ev({ type: 'task.completed', sequence: 1 }))).toBe('/tasks');
	});

	it('routes an agent event to the Agents page', () => {
		expect(deepLink(ev({ type: 'agent.restarted', sequence: 1 }))).toBe('/agents');
	});

	it('never produces a /console/ prefixed route', () => {
		const links = [
			deepLink(ev({ type: 'session.opened', sequence: 1, session: 'a' })),
			deepLink(ev({ type: 'task.failed', sequence: 1, run: 'r' })),
			deepLink(ev({ type: 'agent.registered', sequence: 1 }))
		];
		for (const link of links) {
			expect(link.startsWith('/console/')).toBe(false);
		}
	});
});

describe('activity: relativeTime', () => {
	const now = Date.parse('2026-05-20T12:00:00Z');

	it('renders seconds / minutes / hours / days', () => {
		expect(relativeTime('2026-05-20T11:59:50Z', now)).toBe('10s');
		expect(relativeTime('2026-05-20T11:56:00Z', now)).toBe('4m');
		expect(relativeTime('2026-05-20T09:00:00Z', now)).toBe('3h');
		expect(relativeTime('2026-05-18T12:00:00Z', now)).toBe('2d');
	});

	it('renders an em-dash for a future or unparseable instant', () => {
		expect(relativeTime('2026-05-20T13:00:00Z', now)).toBe('—');
		expect(relativeTime('not-a-date', now)).toBe('—');
	});
});
