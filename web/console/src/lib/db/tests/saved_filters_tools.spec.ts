/**
 * Tools-page saved-filter wrapper tests (Phase 73f / D-116).
 *
 * `ToolsSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table. These tests
 * pin: the `page = 'tools'` scoping (a Tools-page filter never leaks
 * into another page's view), the create / list / update / delete
 * round-trip, and the JSON marshalling of the typed `ToolFilter` spec.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import { ToolsSavedFilters, TOOLS_SAVED_FILTER_PAGE } from '../saved_filters_tools.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `tools-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('tools-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('ToolsSavedFilters', () => {
  it('creates, lists, and reads back a Tools-page saved filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ToolsSavedFilters(db, op);

    const created = await store.create('MCP failures', {
      transports: ['MCP'],
      search: 'fail'
    });
    expect(created.name).toBe('MCP failures');
    expect(created.filterSpec.transports).toEqual(['MCP']);

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].id).toBe(created.id);

    const fetched = await store.get(created.id);
    expect(fetched?.filterSpec.search).toBe('fail');

    await db.close();
  });

  it('scopes rows to page = "tools" — a saved_views-style page row is invisible', async () => {
    const dbName = freshDBName();
    const db = await openDB(dbName);
    const op = await operatorIdOf('tenant-x', 'user-x');

    // Write a saved_filters row under a DIFFERENT page discriminator
    // directly via the table scope; the Tools wrapper must not see it.
    const now = Date.now();
    await db.savedFilters.upsert(op, {
      operator_id: op,
      id: 'sessions-row-1',
      created_at: now,
      updated_at: now,
      page: 'sessions',
      name: 'sessions filter',
      filter_spec_json: '{}'
    });

    const store = new ToolsSavedFilters(db, op);
    await store.create('tools filter', { search: 'x' });

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].name).toBe('tools filter');
    // The sessions-page row is not reachable through the Tools wrapper.
    expect(await store.get('sessions-row-1')).toBeNull();

    await db.close();
  });

  it('updates a saved filter name and spec in place', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ToolsSavedFilters(db, op);

    const created = await store.create('original', { search: 'a' });
    const updated = await store.update(created.id, {
      name: 'renamed',
      filterSpec: { search: 'b', transports: ['HTTP'] }
    });
    expect(updated.name).toBe('renamed');
    expect(updated.filterSpec.transports).toEqual(['HTTP']);

    const fetched = await store.get(created.id);
    expect(fetched?.name).toBe('renamed');

    await db.close();
  });

  it('throws when updating a non-existent filter — fail loud, no silent no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ToolsSavedFilters(db, op);

    await expect(store.update('ghost-id', { name: 'x' })).rejects.toThrow();

    await db.close();
  });

  it('deletes a saved filter; a second delete is a no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ToolsSavedFilters(db, op);

    const created = await store.create('to-delete', {});
    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // Idempotent.
    await store.delete(created.id);

    await db.close();
  });

  it('exports the canonical page discriminator', () => {
    expect(TOOLS_SAVED_FILTER_PAGE).toBe('tools');
  });
});
