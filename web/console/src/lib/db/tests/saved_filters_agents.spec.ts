/**
 * AgentsSavedFilters tests (Phase 73e / D-124) — the typed wrapper over
 * the Phase 72h `saved_filters` Console DB table for the Agents page.
 *
 * The wrapper adds NO new table (D-061). These tests run against the
 * real IndexedDB driver (via `fake-indexeddb`) — no mocks at the seam
 * (CLAUDE.md §17.3) — and pin: (a) Agents-page rows round-trip, (b) the
 * wrapper scopes to `page="agents"` and never returns / mutates another
 * page's chips, (c) a malformed `filter_spec_json` degrades to an empty
 * filter rather than throwing.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import type { SavedFilter } from '../schema.js';
import { freshDBName } from './idb-helpers.js';
import {
  AgentsSavedFilters,
  AGENTS_SAVED_FILTER_PAGE
} from '../saved_filters_agents.js';

async function openForOperator(tenantID: string, userID: string, dbName: string) {
  const masterKey = await deriveMasterKey('agents-saved-filter-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID, userID },
    masterKey,
    databaseName: dbName
  });
}

describe('AgentsSavedFilters: typed wrapper over saved_filters', () => {
  it('round-trips an Agents-page saved filter', async () => {
    const db = await openForOperator('t-a', 'u-a', freshDBName('agentsf-rt'));
    const op = await operatorIdOf('t-a', 'u-a');
    const wrap = new AgentsSavedFilters(db, op);

    const created = await wrap.create('Active react agents', {
      status: ['active'],
      planner_type: ['react']
    });
    expect(created.name).toBe('Active react agents');
    expect(created.filterSpec.status).toEqual(['active']);

    const got = await wrap.get(created.id);
    expect(got).not.toBeNull();
    expect(got?.filterSpec.planner_type).toEqual(['react']);

    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].id).toBe(created.id);
  });

  it('scopes to page="agents" — never returns another page tab chip', async () => {
    const db = await openForOperator('t-b', 'u-b', freshDBName('agentsf-scope'));
    const op = await operatorIdOf('t-b', 'u-b');
    const wrap = new AgentsSavedFilters(db, op);

    // A foreign-page row written directly to the table.
    const foreign: SavedFilter = {
      operator_id: op,
      id: 'foreign-1',
      created_at: 1,
      updated_at: 1,
      page: 'tools',
      name: 'A tools filter',
      filter_spec_json: '{}'
    };
    await db.savedFilters.upsert(op, foreign);

    await wrap.create('An agents filter', { search: 'support' });

    const all = await wrap.list();
    expect(all).toHaveLength(1);
    expect(all[0].name).toBe('An agents filter');
    // The foreign row is invisible to the agents-scoped get + delete.
    expect(await wrap.get('foreign-1')).toBeNull();
    await wrap.delete('foreign-1');
    const stillThere = await db.savedFilters.get(op, 'foreign-1');
    expect(stillThere).not.toBeNull();
  });

  it('degrades a malformed filter_spec_json to an empty filter', async () => {
    const db = await openForOperator('t-c', 'u-c', freshDBName('agentsf-bad'));
    const op = await operatorIdOf('t-c', 'u-c');
    const wrap = new AgentsSavedFilters(db, op);

    const corrupt: SavedFilter = {
      operator_id: op,
      id: 'corrupt-1',
      created_at: 1,
      updated_at: 1,
      page: AGENTS_SAVED_FILTER_PAGE,
      name: 'Corrupt',
      filter_spec_json: '{not json'
    };
    await db.savedFilters.upsert(op, corrupt);

    const got = await wrap.get('corrupt-1');
    expect(got).not.toBeNull();
    expect(got?.filterSpec).toEqual({});
  });
});
