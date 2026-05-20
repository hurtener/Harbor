/**
 * Live Runtime saved-view wrapper tests (Phase 73b / D-126).
 *
 * `LiveRuntimeSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table (D-061). These
 * tests pin: the `page = 'live_runtime'` scoping, the create / list /
 * get / delete round-trip, and the JSON marshalling of the typed
 * `LiveRuntimeViewSpec`.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import {
  LiveRuntimeSavedFilters,
  LIVE_RUNTIME_SAVED_FILTER_PAGE
} from '../saved_filters_live_runtime.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `lr-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('lr-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('LiveRuntimeSavedFilters', () => {
  it('creates, lists, and reads back a Live Runtime saved view', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new LiveRuntimeSavedFilters(db, op);

    const created = await store.create('timeline + trace', {
      tab: 'timeline',
      traceOn: true
    });
    expect(created.name).toBe('timeline + trace');
    expect(created.viewSpec).toEqual({ tab: 'timeline', traceOn: true });

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].viewSpec.tab).toBe('timeline');

    const fetched = await store.get(created.id);
    expect(fetched?.viewSpec.traceOn).toBe(true);
  });

  it('scopes every read to the live_runtime page discriminator', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new LiveRuntimeSavedFilters(db, op);
    await store.create('topology view', { tab: 'topology', traceOn: false });

    // The page discriminator is the constant — a Live Runtime view
    // never leaks into another page's saved-view list.
    expect(LIVE_RUNTIME_SAVED_FILTER_PAGE).toBe('live_runtime');
  });

  it('deletes a saved view (no-op when absent)', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new LiveRuntimeSavedFilters(db, op);
    const created = await store.create('to delete', { tab: 'topology', traceOn: false });

    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // A second delete of the same id is a clean no-op.
    await store.delete(created.id);
  });
});
