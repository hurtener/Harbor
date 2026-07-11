/**
 * EventsHistory `events.list` window/paging fold tests (Phase 162 / D-294).
 *
 * These pin the Console consumer of the durable historical read: the
 * initial tail-first window, scroll-up pagination that PREPENDS older
 * pages, the sticky `truncated` retention-gap flag, and the
 * {@link mergeEventRows} fold that composes the historical backward window
 * with the live SSE forward edge into one newest-first, de-duplicated list.
 */
import { describe, expect, it } from 'vitest';
import { EventsHistory, mergeEventRows } from '../history.svelte.js';
import type { EventsListRequest, EventsListResponse } from '$lib/protocol/events.js';
import type { Event } from '$lib/protocol/events.js';
import type { StateEvent } from '$lib/protocol/state.js';

/** A minimal EventsNamespace stub exposing only `list` — the surface used. */
class FakeEventsNs {
	readonly calls: EventsListRequest[] = [];
	#pages: EventsListResponse[];
	constructor(pages: EventsListResponse[]) {
		this.#pages = pages;
	}
	list(req: EventsListRequest): Promise<EventsListResponse> {
		this.calls.push(req);
		const page = this.#pages.shift();
		return Promise.resolve(page ?? { events: [], next_cursor: 0, has_more: false });
	}
}

function stateRow(sequence: number, truncated = false): StateEvent {
	void truncated;
	return {
		type: 'runtime.warning',
		sequence,
		occurred_at: '2026-07-11T12:00:00Z',
		tenant: 'dev',
		user: 'dev',
		session: 'dev'
	} as StateEvent;
}

function liveRow(sequence: number): Event {
	return {
		type: 'runtime.warning',
		sequence,
		occurred_at: '2026-07-11T12:00:01Z',
		tenant: 'dev',
		user: 'dev',
		session: 'dev'
	} as Event;
}

describe('EventsHistory', () => {
	it('loadInitial fetches the tail page (cursor 0) and records has_more', async () => {
		const ns = new FakeEventsNs([
			{ events: [stateRow(3), stateRow(4), stateRow(5)], next_cursor: 3, has_more: true }
		]);
		const h = new EventsHistory(ns as unknown as import('$lib/protocol/client.js').EventsNamespace);
		await h.loadInitial(3);
		expect(ns.calls[0].cursor).toBe(0);
		expect(h.rows.map((r) => r.sequence)).toEqual([3, 4, 5]);
		expect(h.cursor).toBe(3);
		expect(h.hasMore).toBe(true);
		expect(h.status).toBe('ready');
	});

	it('loadOlder scrolls up via next_cursor and PREPENDS older rows', async () => {
		const ns = new FakeEventsNs([
			{ events: [stateRow(4), stateRow(5)], next_cursor: 4, has_more: true },
			{ events: [stateRow(1), stateRow(2), stateRow(3)], next_cursor: 0, has_more: false }
		]);
		const h = new EventsHistory(ns as unknown as import('$lib/protocol/client.js').EventsNamespace);
		await h.loadInitial(2);
		await h.loadOlder(3);
		// The older page (1,2,3) is prepended before the loaded (4,5).
		expect(h.rows.map((r) => r.sequence)).toEqual([1, 2, 3, 4, 5]);
		expect(ns.calls[1].cursor).toBe(4);
		expect(h.hasMore).toBe(false);
		expect(h.cursor).toBe(0);
	});

	it('loadOlder is a no-op once the retained head is reached', async () => {
		const ns = new FakeEventsNs([{ events: [stateRow(1)], next_cursor: 0, has_more: false }]);
		const h = new EventsHistory(ns as unknown as import('$lib/protocol/client.js').EventsNamespace);
		await h.loadInitial();
		await h.loadOlder();
		expect(ns.calls).toHaveLength(1); // no second fetch
	});

	it('truncated is sticky across pages', async () => {
		const ns = new FakeEventsNs([
			{ events: [stateRow(5)], next_cursor: 4, has_more: true, truncated: false },
			{ events: [stateRow(4)], next_cursor: 0, has_more: false, truncated: true }
		]);
		const h = new EventsHistory(ns as unknown as import('$lib/protocol/client.js').EventsNamespace);
		await h.loadInitial(1);
		expect(h.truncated).toBe(false);
		await h.loadOlder(1);
		expect(h.truncated).toBe(true);
	});

	it('surfaces a scope-mismatch error without throwing', async () => {
		const ns = {
			list: () => Promise.reject({ code: 'identity_scope_required', message: 'nope' })
		};
		const h = new EventsHistory(ns as unknown as import('$lib/protocol/client.js').EventsNamespace);
		await h.loadInitial();
		expect(h.status).toBe('error');
		expect(h.error?.code).toBe('identity_scope_required');
	});
});

describe('mergeEventRows', () => {
	it('composes historical + live, de-duplicated by sequence, newest-first', () => {
		const historical = [stateRow(1), stateRow(2), stateRow(3)];
		const live = [liveRow(3), liveRow(4)]; // seq 3 overlaps
		const merged = mergeEventRows(historical, live);
		expect(merged.map((r) => r.sequence)).toEqual([4, 3, 2, 1]);
		// The live row wins on the seq-3 collision (freshest projection).
		expect(merged.find((r) => r.sequence === 3)?.occurred_at).toBe('2026-07-11T12:00:01Z');
	});

	it('returns the live feed unchanged when there is no history', () => {
		const live = [liveRow(2), liveRow(1)];
		expect(mergeEventRows([], live).map((r) => r.sequence)).toEqual([2, 1]);
	});
});
