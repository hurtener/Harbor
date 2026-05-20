/**
 * Overview page — recent-activity feed projection (Phase 73a / D-127).
 *
 * The recent-activity feed (page-overview.md §3 + §12) renders the last
 * N session opens, task completions/failures, and agent restarts as a
 * newest-first list of typed rows: a typed-event icon glyph + a
 * session-id chip + a free-text description + a relative timestamp.
 *
 * This module is the PURE projection layer: it folds the
 * `events.subscribe` cursor `Event[]` into the rendered {@link
 * ActivityRow} shape and the deep-link route each row navigates to. No
 * `$state`, no Protocol call — unit-testable.
 *
 * # No new Protocol method (Phase 73a)
 *
 * The feed is composition over the SHIPPED `events.subscribe` SSE
 * surface (Phase 60/72). The event types it surfaces are a fixed,
 * operator-relevant subset; an event outside the subset is ignored
 * (the runtime still streams it — the feed simply does not render it).
 */

import type { Event } from '$lib/protocol/harbor.js';

/** The closed set of activity-row severities — maps over the StatusChip kinds. */
export type ActivitySeverity = 'success' | 'warning' | 'danger' | 'accent' | 'neutral';

/** One rendered row of the recent-activity feed. */
export interface ActivityRow {
	/** The per-bus monotonic sequence — the stable row key + sort key. */
	sequence: number;
	/** The canonical event type (e.g. `task.failed`). */
	type: string;
	/** A short glyph label for the typed-event icon column. */
	glyph: string;
	/** The row severity — drives the StatusChip colour. */
	severity: ActivitySeverity;
	/** The publishing session id — rendered as a session-id chip. */
	session: string;
	/** The publishing run id, when the event was run-scoped. */
	run?: string;
	/** The free-text human description. */
	description: string;
	/** The RFC-3339 UTC instant the event occurred. */
	occurredAt: string;
	/** The Console route the row deep-links into when clicked. */
	href: string;
}

/**
 * The operator-relevant event-type subset the recent-activity feed
 * surfaces — page-overview.md §3 bullet 2. An event whose type is not a
 * key here is dropped from the feed.
 */
const ACTIVITY_TYPES: Record<
	string,
	{ glyph: string; severity: ActivitySeverity; label: string }
> = {
	'session.opened': { glyph: 'S+', severity: 'accent', label: 'Session opened' },
	'session.closed': { glyph: 'S-', severity: 'neutral', label: 'Session closed' },
	'task.completed': { glyph: 'T✓', severity: 'success', label: 'Task completed' },
	'task.failed': { glyph: 'T✗', severity: 'danger', label: 'Task failed' },
	'task.cancelled': { glyph: 'T⊘', severity: 'warning', label: 'Task cancelled' },
	'agent.registered': { glyph: 'A+', severity: 'accent', label: 'Agent registered' },
	'agent.restarted': { glyph: 'A↻', severity: 'warning', label: 'Agent restarted' }
};

/**
 * `deepLink` resolves the unprefixed Console route an activity row
 * navigates to — CONVENTIONS.md §1 (no `/console/` prefix). A
 * session-scoped event links to the Sessions detail route; a
 * run-scoped task event links to the Tasks page; an agent event links
 * to the Agents page. Exported for the unit test.
 */
export function deepLink(ev: Event): string {
	if (ev.type.startsWith('session.') && ev.session !== '') {
		return `/sessions/${encodeURIComponent(ev.session)}`;
	}
	if (ev.type.startsWith('task.')) {
		return ev.run !== undefined && ev.run !== ''
			? `/tasks/${encodeURIComponent(ev.run)}`
			: '/tasks';
	}
	if (ev.type.startsWith('agent.')) {
		return '/agents';
	}
	return '/events';
}

/**
 * `projectActivity` folds the `events.subscribe` cursor into the
 * recent-activity feed: it filters to the operator-relevant subset,
 * sorts newest-first by `sequence`, and caps the list at `limit` rows
 * (the feed is a hub glance, not a full log — the Events page is the
 * full investigative surface). `limit` defaults to 25.
 */
export function projectActivity(events: readonly Event[], limit = 25): ActivityRow[] {
	const rows: ActivityRow[] = [];
	for (const ev of events) {
		const spec = ACTIVITY_TYPES[ev.type];
		if (spec === undefined) {
			continue;
		}
		rows.push({
			sequence: ev.sequence,
			type: ev.type,
			glyph: spec.glyph,
			severity: spec.severity,
			session: ev.session,
			run: ev.run,
			description: spec.label,
			occurredAt: ev.occurred_at,
			href: deepLink(ev)
		});
	}
	rows.sort((a, b) => b.sequence - a.sequence);
	return rows.slice(0, limit);
}

/**
 * `relativeTime` renders an RFC-3339 instant as a compact relative
 * label (`12s`, `4m`, `3h`, `2d`) against `now`. A future or
 * unparseable timestamp renders `—` rather than a misleading value.
 */
export function relativeTime(occurredAt: string, now: number): string {
	const t = Date.parse(occurredAt);
	if (Number.isNaN(t)) {
		return '—';
	}
	const deltaSec = Math.floor((now - t) / 1000);
	if (deltaSec < 0) {
		return '—';
	}
	if (deltaSec < 60) {
		return `${deltaSec}s`;
	}
	if (deltaSec < 3600) {
		return `${Math.floor(deltaSec / 60)}m`;
	}
	if (deltaSec < 86_400) {
		return `${Math.floor(deltaSec / 3600)}h`;
	}
	return `${Math.floor(deltaSec / 86_400)}d`;
}
