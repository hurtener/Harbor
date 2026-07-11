// Events page — the `events.list` historical-window read controller
// (Phase 162 / D-294 — Svelte 5 runes mode, D-092).
//
// This is the Console consumer of the durable, time-ranged, cross-session
// `events.list` Protocol method. Where `EventsSubscription` tails the LIVE
// forward edge over SSE, `EventsHistory` reads the BACKWARD window: the
// window picker's `since` bound (compiled into the same `EventFilter` the
// live feed uses) drives an `events.list` call, and scroll-up pagination
// walks older pages via `next_cursor`. The rows are the flat `StateEvent`
// projection — byte-identical to the SSE rows — so the page merges the two
// feeds into one table (see {@link mergeEventRows}). The `truncated` flag
// surfaces the honest retention-gap notice at the window edge.
//
// It never touches the Protocol client directly beyond the injected
// `EventsNamespace` (CONVENTIONS.md §6); no hand-rolled `fetch`.

import { DEFAULT_EVENTS_LIST_LIMIT } from '$lib/protocol/events.js';
import type { Event, EventFilter } from '$lib/protocol/events.js';
import type { EventsNamespace } from '$lib/protocol/client.js';
import type { StateEvent } from '$lib/protocol/state.js';

/** The async status the page reads to render the history controls. */
export type HistoryStatus = 'idle' | 'loading' | 'ready' | 'error';

/**
 * EventsHistory owns the Events page's `events.list` historical read. The
 * page constructs it with the `events` namespace and calls
 * {@link loadInitial} on load / re-filter, and {@link loadOlder} for the
 * scroll-up "Load older" affordance.
 */
export class EventsHistory {
	/** The accumulated historical rows, oldest-first. `$state`-backed. */
	rows = $state<StateEvent[]>([]);
	/** The `next_cursor` for the NEXT older page (0 ⇒ retained head reached). */
	cursor = $state<number>(0);
	/** Whether older matching events remain before the loaded rows. */
	hasMore = $state<boolean>(false);
	/** The honest retention-gap flag — older rows were evicted (inmem ring). */
	truncated = $state<boolean>(false);
	/** The async status the history controls read. */
	status = $state<HistoryStatus>('idle');
	/** The thrown error — populated only in the `error` status. */
	error = $state<{ code: string; message: string } | null>(null);

	readonly #ns: EventsNamespace;
	#filter: EventFilter = {};

	constructor(ns: EventsNamespace) {
		this.#ns = ns;
	}

	/** Sets the compiled EventFilter the next read uses. */
	setFilter(filter: EventFilter): void {
		this.#filter = filter;
	}

	/**
	 * Loads the newest historical page for the current filter (cursor 0 ⇒
	 * from the tail). Resets any previously-loaded rows first — a
	 * re-filter starts a fresh window.
	 */
	async loadInitial(limit: number = DEFAULT_EVENTS_LIST_LIMIT): Promise<void> {
		this.reset();
		await this.#fetch(0, limit, true);
	}

	/**
	 * Scrolls one page older via `next_cursor`. A no-op when the retained
	 * head is already reached (`hasMore` false / cursor 0).
	 */
	async loadOlder(limit: number = DEFAULT_EVENTS_LIST_LIMIT): Promise<void> {
		if (!this.hasMore || this.cursor === 0 || this.status === 'loading') {
			return;
		}
		await this.#fetch(this.cursor, limit, false);
	}

	async #fetch(cursor: number, limit: number, initial: boolean): Promise<void> {
		this.status = 'loading';
		this.error = null;
		try {
			const resp = await this.#ns.list({ filter: this.#filter, cursor, limit });
			// resp.events is oldest-first within the page. On an older page we
			// prepend (older rows sort before the ones already loaded).
			this.rows = initial ? resp.events : [...resp.events, ...this.rows];
			this.cursor = resp.next_cursor;
			this.hasMore = resp.has_more;
			// Truncation is sticky across pages: once any page reports the
			// retention edge, the loaded window is known-incomplete.
			this.truncated = this.truncated || resp.truncated === true;
			this.status = 'ready';
		} catch (e) {
			this.error = describeHistoryError(e);
			this.status = 'error';
		}
	}

	/** Clears all loaded state — a fresh window. */
	reset(): void {
		this.rows = [];
		this.cursor = 0;
		this.hasMore = false;
		this.truncated = false;
		this.status = 'idle';
		this.error = null;
	}
}

/**
 * Merges the historical `events.list` rows with the live SSE rows into one
 * newest-first, de-duplicated (by `sequence`) list — the shape the table
 * renders. Pure + side-effect-free so the fold is unit-tested without a
 * runtime. A `StateEvent` is structurally a superset of the SSE `Event`
 * (it adds only the optional `artifacts[]`), so both feeds coexist in one
 * `Event[]`; the live row wins on a sequence collision (it is the freshest
 * projection of the same event).
 */
export function mergeEventRows(historical: StateEvent[], live: Event[]): Event[] {
	const bySeq = new Map<number, Event>();
	// Seed with historical rows, then let live rows overwrite on collision.
	for (const row of historical) {
		bySeq.set(row.sequence, row);
	}
	for (const row of live) {
		bySeq.set(row.sequence, row);
	}
	return [...bySeq.values()].sort((a, b) => b.sequence - a.sequence);
}

/** Renders a thrown error into a page-friendly `{code, message}`. */
export function describeHistoryError(e: unknown): { code: string; message: string } {
	const pe = e as { code?: unknown; message?: unknown };
	if (typeof pe?.code === 'string' && typeof pe?.message === 'string') {
		switch (pe.code) {
			case 'identity_scope_required':
				return {
					code: pe.code,
					message: 'Cross-tenant historical read requires the admin or console:fleet scope.'
				};
			case 'identity_required':
				return { code: pe.code, message: 'Identity scope is incomplete — re-attach to the runtime.' };
			default:
				return { code: pe.code, message: pe.message };
		}
	}
	if (e instanceof Error) {
		return { code: 'runtime_error', message: e.message };
	}
	return { code: 'runtime_error', message: 'Unknown error' };
}
