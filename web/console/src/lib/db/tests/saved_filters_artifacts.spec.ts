/**
 * Artifacts-page saved-filter wrapper tests (D-121 — Artifacts refactor
 * onto the design-system foundation).
 *
 * `ArtifactsSavedFilters` is a typed wrapper over the shipped
 * `saved_filters` Console DB table — it adds NO table. These tests pin:
 * the `page = 'artifacts'` scoping (an Artifacts-page filter never leaks
 * into another page's view), the create / list / get / delete round-trip,
 * the JSON marshalling of the typed `ArtifactsFilterSpec`, and the
 * fail-loud decode of a malformed spec.
 */
import { describe, expect, it } from 'vitest';
import { openConsoleDB } from '../index.js';
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import { operatorIdOf } from '../schema.js';
import {
  ArtifactsSavedFilters,
  ARTIFACTS_SAVED_FILTER_PAGE
} from '../saved_filters_artifacts.js';

let dbCounter = 0;
function freshDBName(): string {
  dbCounter += 1;
  return `artifacts-sf-${dbCounter}`;
}

async function openDB(dbName: string) {
  const masterKey = await deriveMasterKey('artifacts-sf-pass', generateKdfSalt());
  return openConsoleDB({
    operatorIdentity: { tenantID: 'tenant-x', userID: 'user-x' },
    masterKey,
    databaseName: dbName
  });
}

describe('ArtifactsSavedFilters', () => {
  it('creates, lists, and reads back an Artifacts-page saved filter', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ArtifactsSavedFilters(db, op);

    const created = await store.create('Large PNGs', {
      mimeType: 'image/png',
      source: 'tool'
    });
    expect(created.name).toBe('Large PNGs');
    expect(created.filterSpec.mimeType).toBe('image/png');
    expect(created.filterSpec.source).toBe('tool');

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].id).toBe(created.id);

    const fetched = await store.get(created.id);
    expect(fetched?.filterSpec.source).toBe('tool');

    await db.close();
  });

  it('scopes rows to page = "artifacts" — a different-page row is invisible', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');

    // Write a saved_filters row under a DIFFERENT page discriminator
    // directly via the table scope; the Artifacts wrapper must not see it.
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

    const store = new ArtifactsSavedFilters(db, op);
    await store.create('artifacts filter', { mimeType: 'application/pdf' });

    const listed = await store.list();
    expect(listed).toHaveLength(1);
    expect(listed[0].name).toBe('artifacts filter');
    // The tools-page row is not reachable through the Artifacts wrapper.
    expect(await store.get('tools-row-1')).toBeNull();

    await db.close();
  });

  it('lists name-sorted', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ArtifactsSavedFilters(db, op);

    await store.create('Zeta', {});
    await store.create('Alpha', {});
    await store.create('Mu', {});

    const names = (await store.list()).map((s) => s.name);
    expect(names).toEqual(['Alpha', 'Mu', 'Zeta']);

    await db.close();
  });

  it('deletes an Artifacts-page saved filter; a second delete is a no-op', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const store = new ArtifactsSavedFilters(db, op);

    const created = await store.create('to-delete', {});
    await store.delete(created.id);
    expect(await store.list()).toHaveLength(0);
    // Idempotent.
    await store.delete(created.id);

    await db.close();
  });

  it('does not delete a different page row by id', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const now = Date.now();
    await db.savedFilters.upsert(op, {
      operator_id: op,
      id: 'memory-row-1',
      created_at: now,
      updated_at: now,
      page: 'memory',
      name: 'memory filter',
      filter_spec_json: '{}'
    });

    const store = new ArtifactsSavedFilters(db, op);
    await store.delete('memory-row-1');
    // The off-page row is untouched.
    expect(await db.savedFilters.get(op, 'memory-row-1')).not.toBeNull();

    await db.close();
  });

  it('throws on a malformed filter spec — fail loud, no silent drop', async () => {
    const db = await openDB(freshDBName());
    const op = await operatorIdOf('tenant-x', 'user-x');
    const now = Date.now();
    await db.savedFilters.upsert(op, {
      operator_id: op,
      id: 'corrupt-row',
      created_at: now,
      updated_at: now,
      page: ARTIFACTS_SAVED_FILTER_PAGE,
      name: 'corrupt',
      filter_spec_json: '{not json'
    });

    const store = new ArtifactsSavedFilters(db, op);
    await expect(store.list()).rejects.toThrow(/malformed/);

    await db.close();
  });

  it('exports the canonical page discriminator', () => {
    expect(ARTIFACTS_SAVED_FILTER_PAGE).toBe('artifacts');
  });
});
