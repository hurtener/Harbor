/**
 * Playground saved-view wrapper tests (Phase 73n / D-130).
 *
 * `PlaygroundSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table (D-061). These
 * tests pin: the `page = 'playground'` scoping, the create / list /
 * get / delete round-trip, and the JSON marshalling of the typed
 * `PlaygroundViewSpec`.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import {
	PlaygroundSavedFilters,
	PLAYGROUND_SAVED_FILTER_PAGE
} from '../saved_filters_playground.js';

let dbCounter = 0;
function freshDBName(): string {
	dbCounter += 1;
	return `pg-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
	const masterKey = await deriveMasterKey('pg-sf-pass', generateKdfSalt());
	return openConsoleDB({
		operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
		masterKey,
		databaseName: dbName
	});
}

describe('PlaygroundSavedFilters', () => {
	it('creates, lists, and reads back a Playground saved view', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new PlaygroundSavedFilters(db, op);

		const created = await store.create('high-effort preset', {
			reasoningEffort: 'high',
			temperature: 0.7,
			maxTokens: 2048,
			traceOn: true
		});
		expect(created.name).toBe('high-effort preset');
		expect(created.viewSpec.reasoningEffort).toBe('high');

		const listed = await store.list();
		expect(listed).toHaveLength(1);
		expect(listed[0].viewSpec.temperature).toBe(0.7);

		const fetched = await store.get(created.id);
		expect(fetched).not.toBeNull();
		expect(fetched?.viewSpec.maxTokens).toBe(2048);
		expect(fetched?.viewSpec.traceOn).toBe(true);
	});

	it('scopes rows under the playground page discriminator', async () => {
		expect(PLAYGROUND_SAVED_FILTER_PAGE).toBe('playground');
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new PlaygroundSavedFilters(db, op);
		const created = await store.create('p', { reasoningEffort: 'low' });
		const raw = await db.savedFilters.get(op, created.id);
		expect(raw?.page).toBe('playground');
	});

	it('deletes a Playground saved view', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		const store = new PlaygroundSavedFilters(db, op);
		const created = await store.create('to-delete', {});
		await store.delete(created.id);
		expect(await store.get(created.id)).toBeNull();
		expect(await store.list()).toHaveLength(0);
	});

	it('decodes a corrupt spec to an empty preset', async () => {
		const db = await openDB(freshDBName());
		const op = await operatorIdOf('tenant-x', 'user-x');
		// Insert a row with a corrupt filter_spec_json directly.
		await db.savedFilters.upsert(op, {
			operator_id: op,
			id: 'corrupt-1',
			created_at: Date.now(),
			updated_at: Date.now(),
			page: 'playground',
			name: 'corrupt',
			filter_spec_json: '{not json'
		});
		const store = new PlaygroundSavedFilters(db, op);
		const fetched = await store.get('corrupt-1');
		expect(fetched).not.toBeNull();
		expect(fetched?.viewSpec).toEqual({});
	});
});
