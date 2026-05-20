/**
 * EventsSavedFilters tests (Phase 73g / D-125) — the typed wrapper over
 * the Phase 72h `saved_filters` Console DB table for the Events page.
 *
 * The wrapper adds NO new table (D-061). These tests run against the
 * real IndexedDB driver (via `fake-indexeddb`) — no mocks at the seam
 * (CLAUDE.md §17.3) — and pin: (a) Events-page rows round-trip,
 * (b) the wrapper scopes to `page="events"` and never returns / mutates
 * another page's chips, (c) a malformed `filter_spec_json` fails loudly,
 * (d) NO Protocol method is touched — the wrapper is Console-DB only.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import type { SavedFilter } from '../schema.js';
import { freshDBName } from './idb-helpers.js';
import { EventsSavedFilters, EVENTS_SAVED_FILTER_PAGE } from '../saved_filters_events.js';
import { defaultFacetState } from '../../events/filters.js';

async function openForOperator(tenantID: string, userID: string, dbName: string) {
	const masterKey = await deriveMasterKey('events-saved-filter-pass', generateKdfSalt());
	return openConsoleDB({ operatorIdentity: { tenantID, userID }, masterKey, databaseName: dbName });
}

describe('EventsSavedFilters: typed wrapper over saved_filters', () => {
	it('round-trips an Events-page saved filter', async () => {
		const db = await openForOperator('t-a', 'u-a', freshDBName('evf-rt'));
		const op = await operatorIdOf('t-a', 'u-a');
		const wrap = new EventsSavedFilters(db, op, () => 1_700_000_000_000);

		const spec = { ...defaultFacetState(), eventTypes: ['tool.failed'], window: '24h' as const };
		const created = await wrap.create('Failed tools', spec);
		expect(created.name).toBe('Failed tools');
		expect(created.filterSpec.eventTypes).toEqual(['tool.failed']);
		expect(created.filterSpec.window).toBe('24h');

		const got = await wrap.get(created.id);
		expect(got).not.toBeNull();
		expect(got?.filterSpec.eventTypes).toEqual(['tool.failed']);

		await db.close();
	});

	it('persists the row under page="events"', async () => {
		const db = await openForOperator('t-b', 'u-b', freshDBName('evf-page'));
		const op = await operatorIdOf('t-b', 'u-b');
		const wrap = new EventsSavedFilters(db, op);

		const created = await wrap.create('Budget alerts', {
			...defaultFacetState(),
			eventTypes: ['governance.budget_exceeded']
		});

		const raw = await db.savedFilters.get(op, created.id);
		expect(raw?.page).toBe(EVENTS_SAVED_FILTER_PAGE);
		expect(EVENTS_SAVED_FILTER_PAGE).toBe('events');

		await db.close();
	});

	it("does not return another page's saved filters", async () => {
		const db = await openForOperator('t-c', 'u-c', freshDBName('evf-iso'));
		const op = await operatorIdOf('t-c', 'u-c');

		const toolsRow: SavedFilter = {
			operator_id: op,
			id: 'sf-tools',
			created_at: 1,
			updated_at: 1,
			page: 'tools',
			name: 'My tools',
			filter_spec_json: JSON.stringify({ transport: 'http' })
		};
		await db.savedFilters.upsert(op, toolsRow);

		const wrap = new EventsSavedFilters(db, op);
		const mine = await wrap.create('Events chip', defaultFacetState());

		const all = await wrap.list();
		expect(all).toHaveLength(1);
		expect(all[0].id).toBe(mine.id);
		expect(await wrap.get('sf-tools')).toBeNull();

		await db.close();
	});

	it('list() is name-sorted', async () => {
		const db = await openForOperator('t-s', 'u-s', freshDBName('evf-sort'));
		const op = await operatorIdOf('t-s', 'u-s');
		const wrap = new EventsSavedFilters(db, op);

		await wrap.create('Zeta', defaultFacetState());
		await wrap.create('Alpha', defaultFacetState());
		await wrap.create('Mu', defaultFacetState());

		const names = (await wrap.list()).map((f) => f.name);
		expect(names).toEqual(['Alpha', 'Mu', 'Zeta']);

		await db.close();
	});

	it('delete() removes an Events-page saved filter', async () => {
		const db = await openForOperator('t-e', 'u-e', freshDBName('evf-del'));
		const op = await operatorIdOf('t-e', 'u-e');
		const wrap = new EventsSavedFilters(db, op);

		const created = await wrap.create('Gone', defaultFacetState());
		expect(await wrap.get(created.id)).not.toBeNull();
		await wrap.delete(created.id);
		expect(await wrap.get(created.id)).toBeNull();

		await db.close();
	});

	it('delete() is a no-op for a non-Events-page row id', async () => {
		const db = await openForOperator('t-d', 'u-d', freshDBName('evf-del-iso'));
		const op = await operatorIdOf('t-d', 'u-d');

		const toolsRow: SavedFilter = {
			operator_id: op,
			id: 'sf-tools-2',
			created_at: 1,
			updated_at: 1,
			page: 'tools',
			name: 'Tools chip',
			filter_spec_json: '{}'
		};
		await db.savedFilters.upsert(op, toolsRow);

		const wrap = new EventsSavedFilters(db, op);
		await wrap.delete('sf-tools-2');
		expect(await db.savedFilters.get(op, 'sf-tools-2')).not.toBeNull();

		await db.close();
	});

	it('fails loudly on a malformed filter_spec_json', async () => {
		const db = await openForOperator('t-f', 'u-f', freshDBName('evf-bad'));
		const op = await operatorIdOf('t-f', 'u-f');

		const badRow: SavedFilter = {
			operator_id: op,
			id: 'sf-bad',
			created_at: 1,
			updated_at: 1,
			page: EVENTS_SAVED_FILTER_PAGE,
			name: 'Corrupt',
			filter_spec_json: '{not valid json'
		};
		await db.savedFilters.upsert(op, badRow);

		const wrap = new EventsSavedFilters(db, op);
		await expect(wrap.get('sf-bad')).rejects.toThrow(/malformed filter_spec_json/);

		await db.close();
	});
});
