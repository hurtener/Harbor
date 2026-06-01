// Events page — top-level reactive state controller
// (Phase 73g / D-125 — Svelte 5 runes mode, D-092).
//
// This module owns the Events page's reactive state; the `.svelte`
// components read it and call its actions, never touching the Protocol
// client directly (CONVENTIONS.md §6). It composes:
//
//   - `EventsSubscription` — the `events.subscribe` SSE table feed.
//   - `EventsAggregator`    — the `events.aggregate` sparkline feed.
//   - the faceted filter state (`EventFacetState`).
//
// The page's four-state `<PageState>` (CONVENTIONS.md §4) is driven by
// `status`: `disconnected` when `connection.ts` returns null,
// `loading` while the first aggregate fetch is in flight, `error` on a
// thrown `ProtocolError`, `empty` when the stream + aggregate yield no
// events, `ready` otherwise. Disconnected is NEVER conflated with Error.

import { resolveConnection } from '$lib/connection.js';
import { HarborClient } from '$lib/protocol/harbor.js';
import type { ArtifactsNamespace } from '$lib/protocol/client.js';
import type { Event, EventFilter } from '$lib/protocol/events.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { EventsSubscription, type EventSourceFactory } from './subscription.svelte.js';
import { EventsAggregator } from './aggregate.svelte.js';
import {
	compileFilter,
	defaultFacetState,
	isCrossTenant,
	toggleEventType,
	type EventFacetState
} from './filters.js';
import { EVENT_TYPES } from './taxonomy.js';

/** The auth scope claims that authorise cross-tenant event viewing (D-079). */
export const ADMIN_SCOPES = ['admin', 'console:fleet'] as const;

/**
 * EventsPageState owns the whole Events page. Construct it once in the
 * page component's `onMount`; it lazily resolves the connection and
 * wires the subscription + aggregator. An optional `EventSourceFactory`
 * is injectable so the Playwright harness drives the SSE feed
 * deterministically (CONVENTIONS.md §6 — an injectable client).
 */
export class EventsPageState {
	/** The four-state async status the `<PageState>` boundary reads. */
	status = $state<PageStatus>('loading');
	/** The thrown error — populated only in the `error` status. */
	error = $state<{ code: string; message: string } | null>(null);
	/** The Console-local faceted-filter state. */
	facets = $state<EventFacetState>(defaultFacetState());
	/** The free-text search term (Console-local substring match). */
	search = $state<string>('');
	/** The currently row-selected event (drives the detail rail). */
	selected = $state<Event | null>(null);
	/** The applied saved-view id, or null. */
	activeSavedViewId = $state<string | null>(null);
	/** 1-based pagination page over the loaded rolling window. */
	page = $state<number>(1);
	/** Page size (Console-local — 50 / 100 / 250 per page-events.md §12). */
	pageSize = $state<number>(50);

	/**
	 * The live SSE table feed. Null until the connection resolves. MUST be
	 * `$state`: `load()` assigns it AFTER first render (it's async, in
	 * onMount), so a plain field never triggers the reactive re-read that
	 * surfaces live events in the table — the table would stay empty in
	 * production even while the bus fires (masked in tests because the mock
	 * EventSource factory dispatches synchronously inside `open()`, before
	 * the first render). Phase 108h §17.6 fix.
	 */
	subscription = $state<EventsSubscription | null>(null);
	/** The sparkline aggregate feed. Null until the connection resolves.
	 * `$state` for the same reason as {@link subscription}. */
	aggregator = $state<EventsAggregator | null>(null);
	/** The `artifacts.*` namespace — resolves heavy payloads (D-026). Null
	 * until the connection resolves. */
	artifacts: ArtifactsNamespace | null = null;

	/** The operator's own tenant — drives the cross-tenant facet gate. */
	#ownTenant = '';
	/** The verified scope claims the connection carries. */
	#scopes: string[] = [];
	readonly #esFactory: EventSourceFactory | undefined;

	constructor(esFactory?: EventSourceFactory) {
		this.#esFactory = esFactory;
	}

	/** True when the operator holds a cross-tenant scope claim (D-079). */
	get isAdmin(): boolean {
		return this.#scopes.some((s) => (ADMIN_SCOPES as readonly string[]).includes(s));
	}

	/** The operator's own tenant id (the only one a non-admin may pin). */
	get ownTenant(): string {
		return this.#ownTenant;
	}

	/** True when the active facet state requests cross-tenant fan-in. */
	get crossTenant(): boolean {
		return isCrossTenant(this.facets, this.#ownTenant);
	}

	/**
	 * The search-narrowed projection of the subscription's events —
	 * substring match on the event name + the payload-JSON string
	 * (Console-local, the page-events.md §12 fallback mode until the
	 * Phase 72c runtime-side `search.events` lands).
	 */
	get visibleEvents(): Event[] {
		const all = this.subscription?.events ?? [];
		const q = this.search.trim().toLowerCase();
		if (q === '') {
			return all;
		}
		return all.filter((e) => {
			if (e.type.toLowerCase().includes(q)) {
				return true;
			}
			const payloadStr = e.payload === undefined ? '' : JSON.stringify(e.payload).toLowerCase();
			return payloadStr.includes(q);
		});
	}

	/** The current page slice of `visibleEvents`. */
	get pagedEvents(): Event[] {
		const start = (this.page - 1) * this.pageSize;
		return this.visibleEvents.slice(start, start + this.pageSize);
	}

	/** The total matched-row count (search-narrowed). */
	get total(): number {
		return this.visibleEvents.length;
	}

	/**
	 * The status the `<PageState>` boundary should render. `loading` /
	 * `error` / `disconnected` pass through unchanged; the ready/empty
	 * split is derived LIVE from the subscription's event count so the
	 * table appears the moment events stream in. The plain `status` field
	 * is only updated at load / re-filter time, so reading it directly
	 * would keep the page stuck on `empty` while the SSE fills (the table
	 * was hidden behind the empty-state even though events arrived) —
	 * Phase 108h §17.6 fix.
	 */
	get displayStatus(): PageStatus {
		if (this.status === 'loading' || this.status === 'error' || this.status === 'disconnected') {
			return this.status;
		}
		return this.visibleEvents.length > 0 ? 'ready' : 'empty';
	}

	/**
	 * Resolves the connection, wires the subscription + aggregator, and
	 * opens both feeds. On a null connection the page renders the
	 * Disconnected state — NEVER an Error (CONVENTIONS.md §4/§8).
	 */
	async load(): Promise<void> {
		const connection = resolveConnection();
		if (connection === null) {
			this.status = 'disconnected';
			return;
		}
		this.status = 'loading';
		this.error = null;
		this.#ownTenant = connection.identity.tenant;
		this.#scopes = connection.scopes;
		const client = new HarborClient({ connection });
		this.subscription = new EventsSubscription(client.events, this.#esFactory);
		this.aggregator = new EventsAggregator(client.events);
		this.artifacts = client.artifacts;
		try {
			this.#reopen();
			await this.aggregator.refresh();
			this.status = this.subscription.events.length === 0 ? 'empty' : 'ready';
		} catch (e) {
			this.error = describePageError(e);
			this.status = 'error';
		}
	}

	/** Re-opens the SSE subscription + re-fetches the aggregate. */
	#reopen(): void {
		if (this.subscription === null || this.aggregator === null) {
			return;
		}
		const filter: EventFilter = compileFilter(this.facets);
		// An empty type facet means "all types". The SSE transport needs a
		// NAMED listener per subscribed type (the runtime emits `event:
		// <type>` frames — subscription.svelte.ts); subscribing with an
		// empty list registers NO listeners, so the table would stay empty
		// even while the bus fires. Default to the full taxonomy so the
		// unfiltered firehose actually streams (Phase 108h — §17.6 fix).
		const types =
			this.facets.eventTypes.length > 0 ? this.facets.eventTypes : [...EVENT_TYPES];
		this.subscription.open({
			eventTypes: types,
			admin: this.crossTenant,
			// The Session facet re-scopes the live table feed to that session
			// (Phase 108h — previously the facet only re-scoped the aggregate,
			// so the table could not be filtered to a session). Null = the
			// connection's default session.
			session: this.facets.session ?? undefined
		});
		this.aggregator.window = this.facets.window;
		this.aggregator.setFilter(filter);
		// A `ready`/`empty` page that re-filters stays out of `loading`
		// — the SSE feed re-populates live; only the first load shows the
		// skeleton.
		if (this.status === 'disconnected') {
			this.status = 'ready';
		} else if (this.status !== 'error') {
			this.status = 'ready';
		}
	}

	/** Applies a new faceted-filter state and re-opens the feeds. */
	applyFacets(next: EventFacetState): void {
		this.facets = next;
		this.activeSavedViewId = null;
		this.selected = null;
		this.page = 1;
		this.#reopen();
	}

	/** Pins / unpins one event-type facet chip (also the sparkline click). */
	toggleType(type: string): void {
		this.applyFacets(toggleEventType(this.facets, type));
	}

	/** Applies a saved view by replacing the whole facet state. */
	applySavedView(id: string, spec: EventFacetState): void {
		this.facets = spec;
		this.activeSavedViewId = id;
		this.selected = null;
		this.page = 1;
		this.#reopen();
	}

	/** Sets the Console-local free-text search term; no Protocol call. */
	setSearch(term: string): void {
		this.search = term;
		this.page = 1;
	}

	/** Selects an event row — drives the detail rail. */
	selectEvent(ev: Event | null): void {
		this.selected = ev;
	}

	/** Requests a new 1-based pagination page. */
	goToPage(page: number): void {
		this.page = page;
	}

	/** Changes the pagination page size and resets to page 1. */
	setPageSize(size: number): void {
		this.pageSize = size;
		this.page = 1;
	}

	/** Closes the SSE subscription — the page calls this on unmount. */
	close(): void {
		this.subscription?.close();
	}
}

/** Renders a thrown error into a page-friendly `{code, message}`. */
export function describePageError(e: unknown): { code: string; message: string } {
	const pe = e as { code?: unknown; message?: unknown };
	if (typeof pe?.code === 'string' && typeof pe?.message === 'string') {
		switch (pe.code) {
			case 'identity_required':
			case 'identity_scope_required':
				return {
					code: pe.code,
					message: 'Identity scope is incomplete — re-attach to the runtime.'
				};
			case 'scope_mismatch':
				return {
					code: pe.code,
					message: 'Cross-tenant event viewing requires the admin scope claim.'
				};
			case 'auth_rejected':
				return { code: pe.code, message: 'Your session token expired — re-authenticate.' };
			default:
				return { code: pe.code, message: pe.message };
		}
	}
	if (e instanceof Error) {
		return { code: 'runtime_error', message: e.message };
	}
	return { code: 'runtime_error', message: 'Unknown error' };
}
