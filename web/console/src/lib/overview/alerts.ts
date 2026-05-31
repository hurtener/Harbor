/**
 * Overview page — alerts-strip + audit-ribbon projection (Phase 108c).
 *
 * page-overview.md §3/§5 tag the alerts strip and audit ribbon `[shipped]`:
 * both are pure client-side folds of the `events.subscribe` cursor — NO new
 * Protocol method. The alerts strip renders the most-recent event per alert
 * type seen within the last 5 minutes; the audit ribbon is the count of
 * `audit.admin_scope_used` events in the window. Both render empty on a
 * healthy/quiet runtime (rows appear only when those events actually flow —
 * never synthesised, procedure §1).
 *
 * Pure projection: no `$state`, no Protocol call — unit-testable.
 */

import type { Event } from '$lib/protocol/events.js';

/** The closed set of event types promoted to the alerts strip (spec §5). */
export const ALERT_TYPES = [
	'governance.budget_exceeded',
	'governance.rate_limited',
	'runtime.warning',
	'runtime.error',
	'bus.dropped',
	'memory.health_changed',
	'audit.redaction_failed'
] as const;

export type AlertType = (typeof ALERT_TYPES)[number];

/** The audit-ribbon event type. */
export const AUDIT_TYPE = 'audit.admin_scope_used';

/** Severity tone an alert row renders with. */
export type AlertSeverity = 'warning' | 'danger';

/** One alert-strip row — most-recent event of its type in the window. */
export interface AlertRow {
	/** The originating event type (the strip shows one row per type). */
	type: AlertType;
	/** A short human label for the alert. */
	label: string;
	/** Severity tone. */
	severity: AlertSeverity;
	/** When the most-recent event of this type occurred (ms epoch). */
	occurredMillis: number;
}

const ALERT_META: Record<AlertType, { label: string; severity: AlertSeverity }> = {
	'governance.budget_exceeded': { label: 'Budget exceeded', severity: 'danger' },
	'governance.rate_limited': { label: 'Rate limited', severity: 'warning' },
	'runtime.warning': { label: 'Runtime warning', severity: 'warning' },
	'runtime.error': { label: 'Runtime error', severity: 'danger' },
	'bus.dropped': { label: 'Event bus dropped frames', severity: 'warning' },
	'memory.health_changed': { label: 'Memory health changed', severity: 'warning' },
	'audit.redaction_failed': { label: 'Audit redaction failed', severity: 'danger' }
};

function occurredMillis(ev: Event): number {
	const t = Date.parse(ev.occurred_at ?? '');
	return Number.isFinite(t) ? t : 0;
}

/**
 * `projectAlerts` folds the event cursor into the alerts strip: the
 * most-recent event per {@link ALERT_TYPES} type whose timestamp is within
 * `windowMillis` of `nowMillis`. Rows are newest-first.
 */
export function projectAlerts(
	events: readonly Event[],
	nowMillis: number,
	windowMillis = 5 * 60 * 1000
): AlertRow[] {
	const newestByType = new Map<AlertType, number>();
	const allowed = new Set<string>(ALERT_TYPES);
	for (const ev of events) {
		if (!allowed.has(ev.type)) continue;
		const ms = occurredMillis(ev);
		if (nowMillis - ms > windowMillis) continue;
		const prev = newestByType.get(ev.type as AlertType) ?? 0;
		if (ms >= prev) newestByType.set(ev.type as AlertType, ms);
	}
	const rows: AlertRow[] = [];
	for (const [type, ms] of newestByType) {
		rows.push({ type, label: ALERT_META[type].label, severity: ALERT_META[type].severity, occurredMillis: ms });
	}
	return rows.sort((a, b) => b.occurredMillis - a.occurredMillis);
}

/** Count of `audit.admin_scope_used` events within the window. */
export function auditScopeCount(
	events: readonly Event[],
	nowMillis: number,
	windowMillis = 24 * 60 * 60 * 1000
): number {
	let n = 0;
	for (const ev of events) {
		if (ev.type !== AUDIT_TYPE) continue;
		if (nowMillis - occurredMillis(ev) <= windowMillis) n += 1;
	}
	return n;
}
