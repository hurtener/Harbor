/**
 * Sessions-page saved-filter wrapper tests (Phase 73c / D-122).
 *
 * `SessionsSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table. These tests
 * pin: the `page = 'sessions'` scoping (a Sessions-page filter never
 * leaks into another page's view), the create / list / update /
 * delete round-trip, and the JSON marshalling of the typed
 * `SessionFilter` spec.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import {
  SessionsSavedFilters,
  SESSIONS_SAVED_FILTER_PAGE
} from '../saved_filters_sessions.js';
import { FlowsSavedFilters } from '../saved_filters_flows.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `sessions-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('sessions-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('SessionsSavedFilters', () => {
  it('creates, lists, and reads back a Sessions-page saved filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new SessionsSavedFilters(db, op);

    const created = await store.create('Failed today', {
      statuses: ['failed'],
      query: 'web_search'
    });
    expect(created.name).toBe('Failed today');
    expect(created.filterSpec.statuses).toEqual(['failed']);

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].id).toBe(created.id);
    expect(listed[0].filterSpec.query).toBe('web_search');

    const fetched = await store.get(created.id);
    expect(fetched?.name).toBe('Failed today');
  });

  it('scopes by page — a Sessions filter never sees a Flows filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const sessionsStore = new SessionsSavedFilters(db, op);
    const flowsStore = new FlowsSavedFilters(db, op);

    await sessionsStore.create('Sessions preset', { statuses: ['running'] });
    await flowsStore.create('Flows preset', {});

    const sessionsList = await sessionsStore.list();
    expect(sessionsList).toHaveLength(1);
    expect(sessionsList[0].name).toBe('Sessions preset');
    for (const row of sessionsList) {
      expect(SESSIONS_SAVED_FILTER_PAGE).toBe('sessions');
      void row;
    }
  });

  it('updates an existing filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new SessionsSavedFilters(db, op);

    const created = await store.create('Old name', { statuses: ['paused'] });
    const updated = await store.update(created.id, {
      name: 'New name',
      filterSpec: { statuses: ['completed'] }
    });
    expect(updated.name).toBe('New name');
    expect(updated.filterSpec.statuses).toEqual(['completed']);
  });

  it('throws when updating a non-existent filter — fail-loud', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new SessionsSavedFilters(db, op);
    await expect(store.update('no-such-id', { name: 'x' })).rejects.toThrow();
  });

  it('deletes a filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new SessionsSavedFilters(db, op);

    const created = await store.create('Doomed', {});
    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // Deleting an absent id is a silent no-op.
    await store.delete(created.id);
  });
});
