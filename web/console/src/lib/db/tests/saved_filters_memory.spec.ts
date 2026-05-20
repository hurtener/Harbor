/**
 * MemorySavedFilters tests (Phase 73j / D-118) — the typed wrapper over
 * the Phase 72h `saved_filters` Console DB table for the Memory page.
 *
 * The wrapper adds NO new table (D-061). These tests run against the
 * real IndexedDB driver (via `fake-indexeddb`) — no mocks at the seam
 * (CLAUDE.md §17.3) — and pin: (a) memory-page rows round-trip, (b)
 * the wrapper scopes to `page="memory"` and never returns / mutates
 * another page's chips, (c) a malformed `filter_spec_json` fails loudly.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import type { SavedFilter } from '../schema.js';
import { freshDBName } from './idb-helpers.js';
import { MemorySavedFilters, MEMORY_PAGE } from '../saved_filters_memory.js';

async function openForOperator(tenantID: string, userID: string, dbName: string) {
  const masterKey = await deriveMasterKey('memory-saved-filter-pass', generateKdfSalt());
  return openConsoleDB({ operatorIdentity: { tenantID, userID }, masterKey, databaseName: dbName });
}

describe('MemorySavedFilters: typed wrapper over saved_filters', () => {
  it('round-trips a memory-page saved filter', async () => {
    const db = await openForOperator('t-a', 'u-a', freshDBName('msf-rt'));
    const op = await operatorIdOf('t-a', 'u-a');
    const wrap = new MemorySavedFilters(db, op, () => 1_700_000_000_000);

    await wrap.put({
      id: 'sf-1',
      name: 'Expiring soon',
      filter: { has_ttl_expiring: true, scopes: ['session'] }
    });

    const got = await wrap.get('sf-1');
    expect(got).not.toBeNull();
    expect(got?.name).toBe('Expiring soon');
    expect(got?.filter.has_ttl_expiring).toBe(true);
    expect(got?.filter.scopes).toEqual(['session']);

    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].id).toBe('sf-1');

    await db.close();
  });

  it('persists the row under page="memory"', async () => {
    const db = await openForOperator('t-b', 'u-b', freshDBName('msf-page'));
    const op = await operatorIdOf('t-b', 'u-b');
    const wrap = new MemorySavedFilters(db, op);

    await wrap.put({ id: 'sf-x', name: 'By driver', filter: { drivers: ['postgres'] } });

    // The underlying raw row carries the memory discriminator.
    const raw = await db.savedFilters.get(op, 'sf-x');
    expect(raw?.page).toBe(MEMORY_PAGE);
    expect(MEMORY_PAGE).toBe('memory');

    await db.close();
  });

  it('does not return another page\'s saved filters', async () => {
    const db = await openForOperator('t-c', 'u-c', freshDBName('msf-iso'));
    const op = await operatorIdOf('t-c', 'u-c');

    // A non-memory-page row written directly into saved_filters.
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

    const wrap = new MemorySavedFilters(db, op);
    await wrap.put({ id: 'sf-mem', name: 'Memory chip', filter: {} });

    // list() returns ONLY the memory-page row.
    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].id).toBe('sf-mem');

    // get() of the tools row id returns null (off-page).
    expect(await wrap.get('sf-tools')).toBeNull();

    await db.close();
  });

  it('delete() is a no-op for a non-memory-page row id', async () => {
    const db = await openForOperator('t-d', 'u-d', freshDBName('msf-del'));
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

    const wrap = new MemorySavedFilters(db, op);
    await wrap.delete('sf-tools-2'); // off-page id — must NOT delete it.

    expect(await db.savedFilters.get(op, 'sf-tools-2')).not.toBeNull();
    await db.close();
  });

  it('delete() removes a memory-page saved filter', async () => {
    const db = await openForOperator('t-e', 'u-e', freshDBName('msf-del2'));
    const op = await operatorIdOf('t-e', 'u-e');
    const wrap = new MemorySavedFilters(db, op);

    await wrap.put({ id: 'sf-gone', name: 'Gone', filter: {} });
    expect(await wrap.get('sf-gone')).not.toBeNull();
    await wrap.delete('sf-gone');
    expect(await wrap.get('sf-gone')).toBeNull();

    await db.close();
  });

  it('fails loudly on a malformed filter_spec_json', async () => {
    const db = await openForOperator('t-f', 'u-f', freshDBName('msf-bad'));
    const op = await operatorIdOf('t-f', 'u-f');

    const badRow: SavedFilter = {
      operator_id: op,
      id: 'sf-bad',
      created_at: 1,
      updated_at: 1,
      page: MEMORY_PAGE,
      name: 'Corrupt',
      filter_spec_json: '{not valid json'
    };
    await db.savedFilters.upsert(op, badRow);

    const wrap = new MemorySavedFilters(db, op);
    await expect(wrap.get('sf-bad')).rejects.toThrow(/malformed filter_spec_json/);

    await db.close();
  });
});
