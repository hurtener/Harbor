/**
 * Flows-page saved-filter wrapper tests (Phase 73i / D-117, refactored
 * onto the design-system foundation — D-121).
 *
 * `FlowsSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table. These tests
 * pin: the `page = 'flows'` scoping (a Flows-page filter never leaks
 * into another page's view), the create / list / update / delete
 * round-trip, and the JSON marshalling of the typed `FlowFilter` spec.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import { FlowsSavedFilters, FLOWS_SAVED_FILTER_PAGE } from '../saved_filters_flows.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `flows-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('flows-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('FlowsSavedFilters', () => {
  it('creates, lists, and reads back a Flows-page saved filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new FlowsSavedFilters(db, op);

    const created = await store.create('Graph flows', {
      planner_families: ['graph'],
      query: 'ingest'
    });
    expect(created.name).toBe('Graph flows');
    expect(created.filterSpec.planner_families).toEqual(['graph']);

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].id).toBe(created.id);

    const fetched = await store.get(created.id);
    expect(fetched?.filterSpec.query).toBe('ingest');

    await db.close();
  });

  it('scopes rows to page = "flows" — another page\'s row is invisible', async () => {
    const dbName = freshDBName();
    const db = await openDB(dbName);
    const op = await operatorIdOf('tenant-x', 'user-x');

    // Write a saved_filters row under a DIFFERENT page discriminator
    // directly via the table scope; the Flows wrapper must not see it.
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

    const store = new FlowsSavedFilters(db, op);
    await store.create('flows filter', { query: 'x' });

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].name).toBe('flows filter');
    // The tools-page row is not reachable through the Flows wrapper.
    expect(await store.get('tools-row-1')).toBeNull();

    await db.close();
  });

  it('updates a saved filter name and spec in place', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new FlowsSavedFilters(db, op);

    const created = await store.create('original', { query: 'a' });
    const updated = await store.update(created.id, {
      name: 'renamed',
      filterSpec: { query: 'b', planner_families: ['workflow'] }
    });
    expect(updated.name).toBe('renamed');
    expect(updated.filterSpec.planner_families).toEqual(['workflow']);

    const fetched = await store.get(created.id);
    expect(fetched?.name).toBe('renamed');

    await db.close();
  });

  it('throws when updating a non-existent filter — fail loud, no silent no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new FlowsSavedFilters(db, op);

    await expect(store.update('ghost-id', { name: 'x' })).rejects.toThrow();

    await db.close();
  });

  it('deletes a saved filter; a second delete is a no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new FlowsSavedFilters(db, op);

    const created = await store.create('to-delete', {});
    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // Idempotent.
    await store.delete(created.id);

    await db.close();
  });

  it('exports the canonical page discriminator', () => {
    expect(FLOWS_SAVED_FILTER_PAGE).toBe('flows');
  });
});
