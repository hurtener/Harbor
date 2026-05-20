/**
 * MCPSavedFilters tests (D-121, MCP refactor) — the typed wrapper over
 * the Phase 72h `saved_filters` Console DB table for the MCP Connections
 * page.
 *
 * The wrapper adds NO new table (D-061). These tests run against the
 * real IndexedDB driver (via `fake-indexeddb`) — no mocks at the seam
 * (CLAUDE.md §17.3) — and pin: (a) MCP-page rows round-trip, (b) the
 * wrapper scopes to `page="mcp_connections"` and never returns / mutates
 * another page's chips, (c) a malformed `filter_spec_json` fails loudly.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import type { SavedFilter } from '../schema.js';
import { freshDBName } from './idb-helpers.js';
import { MCPSavedFilters, MCP_SAVED_FILTER_PAGE } from '../saved_filters_mcp.js';

async function openForOperator(tenantID: string, userID: string, dbName: string) {
  const masterKey = await deriveMasterKey('mcp-saved-filter-pass', generateKdfSalt());
  return openConsoleDB({ operatorIdentity: { tenantID, userID }, masterKey, databaseName: dbName });
}

describe('MCPSavedFilters: typed wrapper over saved_filters', () => {
  it('round-trips an MCP-page saved filter', async () => {
    const db = await openForOperator('t-a', 'u-a', freshDBName('mcpf-rt'));
    const op = await operatorIdOf('t-a', 'u-a');
    const wrap = new MCPSavedFilters(db, op, () => 1_700_000_000_000);

    const created = await wrap.create('Errored only', {
      state: ['error'],
      has_recent_error: true
    });
    expect(created.name).toBe('Errored only');
    expect(created.filterSpec.state).toEqual(['error']);

    const got = await wrap.get(created.id);
    expect(got).not.toBeNull();
    expect(got?.filterSpec.has_recent_error).toBe(true);

    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].id).toBe(created.id);

    await db.close();
  });

  it('persists the row under page="mcp_connections"', async () => {
    const db = await openForOperator('t-b', 'u-b', freshDBName('mcpf-page'));
    const op = await operatorIdOf('t-b', 'u-b');
    const wrap = new MCPSavedFilters(db, op);

    const created = await wrap.create('Online stdio', { transport: ['stdio'] });

    const raw = await db.savedFilters.get(op, created.id);
    expect(raw?.page).toBe(MCP_SAVED_FILTER_PAGE);
    expect(MCP_SAVED_FILTER_PAGE).toBe('mcp_connections');

    await db.close();
  });

  it("does not return another page's saved filters", async () => {
    const db = await openForOperator('t-c', 'u-c', freshDBName('mcpf-iso'));
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

    const wrap = new MCPSavedFilters(db, op);
    const mine = await wrap.create('MCP chip', {});

    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].id).toBe(mine.id);

    // get() of the tools-page row id returns null (off-page).
    expect(await wrap.get('sf-tools')).toBeNull();

    await db.close();
  });

  it('list() is name-sorted', async () => {
    const db = await openForOperator('t-s', 'u-s', freshDBName('mcpf-sort'));
    const op = await operatorIdOf('t-s', 'u-s');
    const wrap = new MCPSavedFilters(db, op);

    await wrap.create('Zeta', {});
    await wrap.create('Alpha', {});
    await wrap.create('Mu', {});

    const names = (await wrap.list()).map((f) => f.name);
    expect(names).toEqual(['Alpha', 'Mu', 'Zeta']);

    await db.close();
  });

  it('delete() is a no-op for a non-MCP-page row id', async () => {
    const db = await openForOperator('t-d', 'u-d', freshDBName('mcpf-del'));
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

    const wrap = new MCPSavedFilters(db, op);
    await wrap.delete('sf-tools-2'); // off-page id — must NOT delete it.

    expect(await db.savedFilters.get(op, 'sf-tools-2')).not.toBeNull();
    await db.close();
  });

  it('delete() removes an MCP-page saved filter', async () => {
    const db = await openForOperator('t-e', 'u-e', freshDBName('mcpf-del2'));
    const op = await operatorIdOf('t-e', 'u-e');
    const wrap = new MCPSavedFilters(db, op);

    const created = await wrap.create('Gone', {});
    expect(await wrap.get(created.id)).not.toBeNull();
    await wrap.delete(created.id);
    expect(await wrap.get(created.id)).toBeNull();

    await db.close();
  });

  it('fails loudly on a malformed filter_spec_json', async () => {
    const db = await openForOperator('t-f', 'u-f', freshDBName('mcpf-bad'));
    const op = await operatorIdOf('t-f', 'u-f');

    const badRow: SavedFilter = {
      operator_id: op,
      id: 'sf-bad',
      created_at: 1,
      updated_at: 1,
      page: MCP_SAVED_FILTER_PAGE,
      name: 'Corrupt',
      filter_spec_json: '{not valid json'
    };
    await db.savedFilters.upsert(op, badRow);

    const wrap = new MCPSavedFilters(db, op);
    await expect(wrap.get('sf-bad')).rejects.toThrow(/malformed filter_spec_json/);

    await db.close();
  });
});
