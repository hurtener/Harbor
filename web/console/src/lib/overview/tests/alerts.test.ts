/**
 * Overview alerts/audit projection tests (Phase 108c).
 *
 * `projectAlerts` folds the event cursor into the alerts strip (most-recent
 * event per type within the 5m window); `auditScopeCount` counts
 * `audit.admin_scope_used` within the 24h window. These are pure functions —
 * exercised here against synthetic frames at the window boundaries.
 */
import { describe, expect, it } from 'vitest';
import { projectAlerts, auditScopeCount, ALERT_TYPES, AUDIT_TYPE } from '../alerts.js';
import type { Event } from '$lib/protocol/events.js';

const NOW = Date.parse('2026-05-31T12:00:00Z');

function ev(type: string, agoMs: number, seq = 1): Event {
	return {
		type,
		sequence: seq,
		occurred_at: new Date(NOW - agoMs).toISOString(),
		tenant: 'dev',
		user: 'dev',
		session: 'dev'
	};
}

describe('alerts: projectAlerts', () => {
	it('renders one row per in-window alert type, newest-first', () => {
		const rows = projectAlerts(
			[
				ev('governance.budget_exceeded', 60_000), // 1m ago
				ev('governance.rate_limited', 30_000), // 30s ago (newer)
				ev('task.completed', 1_000) // not an alert type
			],
			NOW
		);
		expect(rows).toHaveLength(2);
		expect(rows[0].type).toBe('governance.rate_limited'); // newest first
		expect(rows[1].type).toBe('governance.budget_exceeded');
		expect(rows[0].severity).toBe('warning');
		expect(rows[1].severity).toBe('danger');
	});

	it('keeps only the most-recent event per type', () => {
		const rows = projectAlerts(
			[ev('runtime.error', 120_000, 1), ev('runtime.error', 10_000, 2)],
			NOW
		);
		expect(rows).toHaveLength(1);
		expect(rows[0].occurredMillis).toBe(NOW - 10_000);
	});

	it('drops events older than the 5m window', () => {
		const rows = projectAlerts(
			[ev('bus.dropped', 6 * 60 * 1000), ev('memory.health_changed', 60_000)],
			NOW
		);
		expect(rows.map((r) => r.type)).toEqual(['memory.health_changed']);
	});

	it('returns empty on a quiet runtime (no alert events)', () => {
		expect(projectAlerts([ev('task.completed', 1_000)], NOW)).toEqual([]);
	});

	it('covers every declared alert type with metadata', () => {
		const rows = projectAlerts(
			ALERT_TYPES.map((t, i) => ev(t, 1_000 + i)),
			NOW
		);
		expect(rows).toHaveLength(ALERT_TYPES.length);
		for (const r of rows) {
			expect(r.label.length).toBeGreaterThan(0);
			expect(['warning', 'danger']).toContain(r.severity);
		}
	});
});

describe('alerts: auditScopeCount', () => {
	it('counts admin-scope-used events within the 24h window', () => {
		const count = auditScopeCount(
			[
				ev(AUDIT_TYPE, 1_000),
				ev(AUDIT_TYPE, 60 * 60 * 1000), // 1h ago
				ev(AUDIT_TYPE, 25 * 60 * 60 * 1000), // 25h ago — outside window
				ev('task.completed', 1_000) // not audit
			],
			NOW
		);
		expect(count).toBe(2);
	});

	it('is zero on a quiet runtime', () => {
		expect(auditScopeCount([ev('task.completed', 1_000)], NOW)).toBe(0);
	});
});
