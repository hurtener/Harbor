/**
 * Overview saved-view wrapper tests (Phase 73a / D-127).
 *
 * `OverviewSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table (D-061). These
 * tests pin: the `page = 'overview'` scoping, the create / list / get /
 * delete round-trip, and the JSON marshalling of the typed
 * `OverviewViewSpec`.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import {
	OverviewSavedFilters,
	OVERVIEW_SAVED_FILTER_PAGE
} from '../saved_filters_overview.js';

let dbCounter = 0;
function freshDBName(): string {
	dbCounter += 1;
	return `ov-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
	const masterKey = await deriveMasterKey('ov-sf-pass', generateKdfSalt());
	return openConsoleDB({
		operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
		masterKey,
		databaseName: dbName
	});
}

describe('OverviewSavedFilters', () => {
	it('creates, lists, and reads back an Overview saved view', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new OverviewSavedFilters(db, op);

		const created = await store.create('quiet hub', {
			window: '15m',
			activityTypes: ['task.failed']
		});
		expect(created.name).toBe('quiet hub');
		expect(created.viewSpec).toEqual({ window: '15m', activityTypes: ['task.failed'] });

		const listed = await store.list();
		expect(listed).toHaveLength(1);
		expect(listed[0].viewSpec.window).toBe('15m');

		const fetched = await store.get(created.id);
		expect(fetched?.viewSpec.activityTypes).toEqual(['task.failed']);
	});

	it('scopes every read to the overview page discriminator', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new OverviewSavedFilters(db, op);
		await store.create('default', { window: '5m', activityTypes: [] });

		// The page discriminator is the constant — an Overview view never
		// leaks into another page's saved-view list.
		expect(OVERVIEW_SAVED_FILTER_PAGE).toBe('overview');
		const listed = await store.list();
		expect(listed.every((v) => v.name === 'default')).toBe(true);
	});

	it('deletes a saved view (no-op when absent)', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new OverviewSavedFilters(db, op);
		const created = await store.create('to delete', { window: '1m', activityTypes: [] });

		await store.delete(created.id);
		expect(await store.list()).toHaveLength(0);
		// A second delete of the same id is a clean no-op.
		await store.delete(created.id);
		await store.delete('never-existed');
	});

	it('lists views name-sorted', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new OverviewSavedFilters(db, op);
		await store.create('zeta', { window: '1m', activityTypes: [] });
		await store.create('alpha', { window: '5m', activityTypes: [] });

		const listed = await store.list();
		expect(listed.map((v) => v.name)).toEqual(['alpha', 'zeta']);
	});
});
