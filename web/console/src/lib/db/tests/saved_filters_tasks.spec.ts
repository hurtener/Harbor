/**
 * Tasks-page saved-filter wrapper tests (Phase 73d / D-123).
 *
 * `TasksSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table. These tests
 * pin: the `page = 'tasks'` scoping (a Tasks-page filter never leaks
 * into another page's view), the create / list / update / delete
 * round-trip, and the JSON marshalling of the typed `TaskFilter` spec.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import { TasksSavedFilters, TASKS_SAVED_FILTER_PAGE } from '../saved_filters_tasks.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `tasks-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('tasks-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('TasksSavedFilters', () => {
  it('creates, lists, and reads back a Tasks-page saved filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new TasksSavedFilters(db, op);

    const created = await store.create('failed in the last hour', {
      statuses: ['failed'],
      search: 'timeout'
    });
    expect(created.name).toBe('failed in the last hour');
    expect(created.filterSpec.statuses).toEqual(['failed']);

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].id).toBe(created.id);

    const fetched = await store.get(created.id);
    expect(fetched?.filterSpec.search).toBe('timeout');

    await db.close();
  });

  it('scopes rows to page = "tasks" — a foreign-page row is invisible', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');

    const now = Date.now();
    await db.savedFilters.upsert(op, {
      operator_id: op,
      id: 'tools-row-1',
      created_at: now,
      updated_at: now,
      page: 'tools',
      name: 'tools filter',
      filter_spec_json: '{}'
    });

    const store = new TasksSavedFilters(db, op);
    await store.create('tasks filter', { kinds: ['background'] });

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].name).toBe('tasks filter');
    // The tools-page row is not reachable through the Tasks wrapper.
    expect(await store.get('tools-row-1')).toBeNull();

    await db.close();
  });

  it('updates a saved filter name and spec in place', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new TasksSavedFilters(db, op);

    const created = await store.create('original', { search: 'a' });
    const updated = await store.update(created.id, {
      name: 'renamed',
      filterSpec: { search: 'b', statuses: ['running'] }
    });
    expect(updated.name).toBe('renamed');
    expect(updated.filterSpec.statuses).toEqual(['running']);

    const fetched = await store.get(created.id);
    expect(fetched?.name).toBe('renamed');

    await db.close();
  });

  it('throws when updating a non-existent filter — fail loud, no silent no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new TasksSavedFilters(db, op);

    await expect(store.update('ghost-id', { name: 'x' })).rejects.toThrow();

    await db.close();
  });

  it('deletes a saved filter; a second delete is a no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new TasksSavedFilters(db, op);

    const created = await store.create('to-delete', {});
    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // Idempotent.
    await store.delete(created.id);

    await db.close();
  });

  it('exports the canonical page discriminator', () => {
    expect(TASKS_SAVED_FILTER_PAGE).toBe('tasks');
  });
});
