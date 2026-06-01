/**
 * Live Runtime page — pure, non-reactive helpers (Phase 108d Stage 3).
 *
 * The Live Runtime page (`routes/(console)/live-runtime/+page.svelte`) is a
 * large workbench. The genuinely reactive orchestration (`load()`, the
 * `$effect` event fold, `onMount`/`onDestroy`, the `$state`/`$derived`
 * declarations) MUST stay in the component — moving Svelte runes into a plain
 * `.ts` module breaks reactivity. What CAN leave cleanly is the pure logic:
 *
 *   - `toError` — normalise an unknown throw into a `{ code, message }`.
 *   - `nodeStateForEvent` — map a `task.*` / `planner.*` event type onto a
 *     topology node run state (or null when the event changes nothing).
 *   - `sessionStatusLabel` — derive the rail's session-level status label
 *     from the live status-counter strip + the page status (a pure function
 *     of its inputs; the page wraps it in a `$derived`).
 *   - `LIVE_RUNTIME_EVENT_TYPES` — the SSE subscription vocabulary the page
 *     opens the event stream against (the 108c named-event fix).
 *
 * Pure logic here so a Vitest unit test exercises it without a DOM.
 */
import { ProtocolError } from '$lib/protocol/errors.js';
import type { NodeState } from '$lib/live-runtime/topology-adapter.js';
import type { StatusCounterStrip } from '$lib/live-runtime/strip.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { Event } from '$lib/protocol/events.js';
import type { RecentSession } from '$lib/components/live-runtime/active-sessions-panel.svelte';

/**
 * The named SSE event types the Live Runtime page subscribes to. The Runtime
 * emits NAMED SSE frames, so the subscription MUST list the event types it
 * wants (108c named-event fix) — an empty `open()` receives nothing.
 */
export const LIVE_RUNTIME_EVENT_TYPES: string[] = [
	'task.spawned',
	'task.started',
	'task.completed',
	'task.failed',
	'task.cancelled',
	'tool.invoked',
	'tool.completed',
	'tool.failed',
	'planner.decision',
	'planner.finish',
	'planner.error',
	'pause.requested',
	'pause.resumed',
	'tool.approval_requested',
	'tool.approved',
	'tool.rejected',
	'tool.auth_required',
	'control.received',
	'control.applied',
	'control.rejected',
	'session.opened',
	'session.closed',
	'llm.cost.recorded'
];

/**
 * Normalise an unknown throw into the page's `{ code, message }` error shape.
 * A `ProtocolError` keeps its code; anything else maps to `runtime_error`.
 */
export function toError(err: unknown): { code: string; message: string } {
	if (err instanceof ProtocolError) {
		return { code: err.code, message: err.message };
	}
	return {
		code: 'runtime_error',
		message: err instanceof Error ? err.message : 'unknown error'
	};
}

/**
 * Maps a `task.*` event type to a topology node run state, or null when the
 * event does not change a node's state. Pure — no fabrication: a type that
 * carries no state mapping returns null and the node keeps its last-known
 * state.
 */
export function nodeStateForEvent(type: string, payload?: unknown): NodeState | null {
	switch (type) {
		case 'task.spawned':
			return { state: 'pending' };
		case 'task.started':
			return { state: 'running' };
		case 'task.completed':
			return { state: 'completed' };
		case 'pause.requested':
			return { state: 'paused' };
		case 'pause.resumed':
			return { state: 'running' };
		case 'task.failed':
		case 'tool.failed':
		case 'planner.error': {
			const rec = (payload ?? {}) as Record<string, unknown>;
			const code = typeof rec['code'] === 'string' ? (rec['code'] as string) : 'failed';
			return { state: 'failed', failureCode: code };
		}
		default:
			return null;
	}
}

/**
 * Derive the right rail's session-level status label from the live
 * status-counter strip + the page status. Pure function of its inputs; the
 * page wraps it in a `$derived`.
 *
 * W10 (Phase 83x): the rail's `Status` field used to mirror the PAGE's
 * PageStatus, which wired the rail to react to a topology-snapshot failure.
 * Deriving from the strip instead reflects the SESSION's lifecycle truthfully
 * (active while anything is in-flight; complete when everything terminated
 * cleanly; failed when something failed) — the page's own loading status
 * stays separate and drives the PageState chrome.
 */
export function sessionStatusLabel(strip: StatusCounterStrip, status: PageStatus): string {
	if (status === 'disconnected') {
		return 'disconnected';
	}
	if (strip.running + strip.pending + strip.paused > 0) {
		return 'active';
	}
	if (strip.failed > 0 && strip.completed === 0) {
		return 'failed';
	}
	if (strip.completed > 0) {
		return 'complete';
	}
	return 'idle';
}

/**
 * Fold the live event stream into the cockpit's "Active sessions" list
 * (Phase 108e). There is no runtime-scoped `sessions.list` Protocol surface
 * yet, so the cockpit derives a best-effort, honest, partial recent-sessions
 * view from the `session.opened` / `session.closed` frames the SSE stream
 * delivers. A session's state is its LATEST observed lifecycle event; an
 * unobserved session never appears (no fabrication — CLAUDE.md §13). Newest
 * activity first; capped at `limit` rows.
 */
export function foldRecentSessions(events: readonly Event[], limit = 8): RecentSession[] {
	const latest = new Map<string, RecentSession>();
	for (const ev of events) {
		if (ev.type !== 'session.opened' && ev.type !== 'session.closed') {
			continue;
		}
		const session = ev.session;
		if (session === undefined || session === '') {
			continue;
		}
		const state: RecentSession['state'] = ev.type === 'session.closed' ? 'closed' : 'open';
		const existing = latest.get(session);
		// The events array is newest-first; keep the first (latest) seen per id.
		if (existing === undefined) {
			latest.set(session, { session, state, at: ev.occurred_at });
		}
	}
	return [...latest.values()].slice(0, limit);
}
