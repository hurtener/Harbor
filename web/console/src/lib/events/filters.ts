/**
 * Events page — pure filter-builder helpers (Phase 73g / D-125).
 *
 * The Events page's faceted filter chips (Event type / Tenant / User /
 * Session / Run / Window / More filters — page-events.md §12) are
 * Console-local UI state. `compileFilter` projects that state onto the
 * `EventFilter` wire shape Phase 72a's `events.subscribe` /
 * `events.aggregate` accept. Pure, side-effect-free, unit-tested — no
 * Protocol call, no `$state`.
 *
 * # Identity is mandatory (CLAUDE.md §6)
 *
 * The runtime backfills an empty identity axis with the caller's own
 * tuple component; `compileFilter` therefore emits an axis ONLY when
 * the operator pinned a value. A cross-tenant filter (a `tenant_ids`
 * value other than the operator's own) is gated server-side on
 * `auth.ScopeAdmin` / `auth.ScopeConsoleFleet` (D-079) — there is no
 * dedicated `events.crosstenant` scope.
 */

import type { EventFilter, TimeWindow } from '$lib/protocol/harbor.js';
import { WINDOW_SPEC } from '$lib/protocol/harbor.js';

/**
 * The Console-local faceted-filter state the page's chips drive.
 * Every axis is optional; an unset axis means "unconstrained" (the
 * runtime resolves it to the caller's own identity component).
 */
export interface EventFacetState {
	/** Pinned event-type names (e.g. `tool.failed`). Empty = all types. */
	eventTypes: string[];
	/** Pinned tenant — cross-tenant when not the operator's own (D-079). */
	tenant: string | null;
	/** Pinned user. */
	user: string | null;
	/** Pinned session. */
	session: string | null;
	/** Pinned run. */
	run: string | null;
	/** The active sparkline / subscription window. */
	window: TimeWindow;
}

/** The default facet state — last 1 h, all types, no identity pin. */
export function defaultFacetState(): EventFacetState {
	return {
		eventTypes: [],
		tenant: null,
		user: null,
		session: null,
		run: null,
		window: '1h'
	};
}

/**
 * `compileFilter` projects {@link EventFacetState} onto the `EventFilter`
 * wire shape. The `since` lower bound is derived from the active window
 * relative to `now` (the caller passes the clock so the function stays
 * pure / testable). `until` is left unset — "now" — so the subscription
 * tails live.
 */
export function compileFilter(state: EventFacetState, now: Date = new Date()): EventFilter {
	const filter: EventFilter = {};
	if (state.eventTypes.length > 0) {
		filter.event_types = [...state.eventTypes].sort();
	}
	if (state.tenant) {
		filter.tenant_ids = [state.tenant];
	}
	if (state.user) {
		filter.user_ids = [state.user];
	}
	if (state.session) {
		filter.session_ids = [state.session];
	}
	if (state.run) {
		filter.run_ids = [state.run];
	}
	const windowMs = WINDOW_SPEC[state.window].windowNs / 1_000_000;
	filter.since = new Date(now.getTime() - windowMs).toISOString();
	return filter;
}

/**
 * True when the compiled filter requests cross-tenant fan-in — a
 * `tenant_ids` value other than the operator's own tenant. The page
 * uses this to surface the `audit.admin_scope_used` expectation and to
 * disable the `Tenant ▾` facet for non-admin operators (defence in
 * depth; the authoritative gate is the Phase 61 transport edge).
 */
export function isCrossTenant(state: EventFacetState, ownTenant: string): boolean {
	return state.tenant !== null && state.tenant !== ownTenant;
}

/**
 * Pins (or, when already pinned to the same value, clears) an
 * event-type chip. Used by both the chip row and the sparkline
 * click-to-pin interaction. Returns a NEW state — the page's `$state`
 * rune is reassigned, never mutated in place.
 */
export function toggleEventType(state: EventFacetState, type: string): EventFacetState {
	const has = state.eventTypes.includes(type);
	return {
		...state,
		eventTypes: has
			? state.eventTypes.filter((t) => t !== type)
			: [...state.eventTypes, type]
	};
}
