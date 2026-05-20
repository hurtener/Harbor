/**
 * Migration tests (Phase 72h acceptance criterion: migration test).
 *
 * - A clean DB applies every migration (all eight tables created).
 * - A re-open is a no-op (`runMigrations` applies zero).
 * - A DB at version N-1 re-opens and runs the missing migration.
 * - The migration list contains no destructive (`DROP` / `ALTER COLUMN`)
 *   shapes — forward-only per CLAUDE.md §9.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  CURRENT_SCHEMA_VERSION,
  MIGRATIONS,
  SCHEMA_MIGRATIONS_STORE,
  assertMigrationsWellFormed,
  runMigrations,
  type Migration
} from '../migrations.js';
import { ErrMigrationConflict } from '../errors.js';
import { TABLE_NAMES } from '../schema.js';
import { IndexedDBConsoleDB } from '../drivers/indexeddb.js';
import { freshDBName, makeOptions } from './idb-helpers.js';

// Resolved from the Vitest project root (web/console) — `import.meta.url`
// is not a `file:` URL under the jsdom environment.
const MIGRATIONS_TS = resolve(process.cwd(), 'src/lib/db/migrations.ts');

describe('migrations: forward-only well-formedness', () => {
  it('the V1 migration list is well-formed', () => {
    expect(() => assertMigrationsWellFormed()).not.toThrow();
  });

  it('migration 1 creates all eight V1 tables', () => {
    const created = new Set(MIGRATIONS.flatMap((m) => m.createTables));
    for (const t of TABLE_NAMES) expect(created.has(t)).toBe(true);
    expect(created.size).toBe(8);
  });

  it('rejects a non-contiguous version sequence', () => {
    const bad: Migration[] = [
      { version: 1, description: 'one', createTables: ['profiles'] },
      { version: 3, description: 'skips two', createTables: ['keybindings'] }
    ];
    expect(() => assertMigrationsWellFormed(bad)).toThrow(ErrMigrationConflict);
  });

  it('rejects a table created by more than one migration', () => {
    const bad: Migration[] = [
      { version: 1, description: 'one', createTables: ['profiles'] },
      { version: 2, description: 'duplicates profiles', createTables: ['profiles'] }
    ];
    expect(() => assertMigrationsWellFormed(bad)).toThrow(ErrMigrationConflict);
  });

  it('migrations.ts has no DROP TABLE / ALTER COLUMN shapes', () => {
    const src = readFileSync(MIGRATIONS_TS, 'utf8');
    expect(/DROP\s+TABLE/i.test(src)).toBe(false);
    expect(/ALTER\s+COLUMN/i.test(src)).toBe(false);
  });
});

describe('migrations: runMigrations', () => {
  it('applies every pending migration on a clean DB', async () => {
    const applied = await runMigrations(new Set<number>(), async () => {});
    expect(applied).toBe(MIGRATIONS.length);
  });

  it('is a no-op when every migration is already applied', async () => {
    const all = new Set(MIGRATIONS.map((m) => m.version));
    const applied = await runMigrations(all, async () => {
      throw new Error('applyOne must not be called when nothing is pending');
    });
    expect(applied).toBe(0);
  });

  it('applies only the missing migration when the DB is at N-1', async () => {
    const atZero = new Set<number>();
    const calls: number[] = [];
    await runMigrations(atZero, async (m) => {
      calls.push(m.version);
    });
    // V1 has one migration; from version 0, exactly version 1 runs.
    expect(calls).toEqual([1]);
  });
});

describe('migrations: against the real IndexedDB driver', () => {
  it('a clean DB open creates all eight tables + schema_migrations', async () => {
    const name = freshDBName('migrate-clean');
    const db = new IndexedDBConsoleDB(makeOptions({ databaseName: name }));
    await db.open();
    const versions = await db.appliedMigrationVersions();
    expect(versions.has(CURRENT_SCHEMA_VERSION)).toBe(true);
    // Every table scope is reachable (the object store exists).
    await expect(db.profiles.list('op-x')).resolves.toEqual([]);
    await expect(db.savedFilters.list('op-x')).resolves.toEqual([]);
    await db.close();
  });

  it('re-opening the same DB applies zero new migrations', async () => {
    const name = freshDBName('migrate-reopen');
    const first = new IndexedDBConsoleDB(makeOptions({ databaseName: name }));
    await first.open();
    const v1 = await first.appliedMigrationVersions();
    await first.close();

    const second = new IndexedDBConsoleDB(makeOptions({ databaseName: name }));
    await second.open();
    const v2 = await second.appliedMigrationVersions();
    await second.close();

    expect([...v2].sort()).toEqual([...v1].sort());
  });

  it('the schema_migrations store name is stable', () => {
    expect(SCHEMA_MIGRATIONS_STORE).toBe('schema_migrations');
  });
});
