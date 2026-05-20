/**
 * Console DB forward-only migrations (Phase 72h).
 *
 * Migrations are forward-only (CLAUDE.md §9): each carries a monotonic
 * `version` integer and a description; the driver runs pending migrations
 * on open and records each into a `schema_migrations` store. A migration
 * that DROPs or ALTERs an existing table is rejected — `migrations.spec.ts`
 * parses this list and asserts no destructive shapes appear.
 *
 * Because IndexedDB is schemaless, a "migration" here creates an object
 * store (a table) with the per-operator + id key shape. Adding a column to
 * an existing table needs no migration (IDB records are schemaless); the
 * `validateRow` pass in `schema.ts` enforces the column set at the driver
 * edge. Adding a NEW table is a new forward migration appended here.
 */
import { ErrMigrationConflict } from './errors.js';
import { TABLE_NAMES, type TableName } from './schema.js';

/** The IndexedDB object store that records applied migration versions. */
export const SCHEMA_MIGRATIONS_STORE = 'schema_migrations';

/**
 * A single forward-only migration. `createTables` names the object stores
 * this migration introduces; the driver creates each with the
 * `[operator_id, id]` compound shape (operator-scoped key + local id).
 */
export interface Migration {
  /** Monotonic version integer; the first migration is version 1. */
  readonly version: number;
  /** Human-readable description (recorded for audit / debugging). */
  readonly description: string;
  /** Object stores (tables) this migration creates. */
  readonly createTables: readonly TableName[];
}

/**
 * The forward-only migration list. Migration 1 creates all eight V1
 * tables. Future tables land as migration 2, 3, ... — never by editing
 * migration 1 (CLAUDE.md §9 append-only rule).
 */
export const MIGRATIONS: readonly Migration[] = [
  {
    version: 1,
    description: 'Phase 72h — create the eight V1 Console DB tables',
    createTables: TABLE_NAMES
  }
];

/**
 * The schema version a freshly-migrated DB reports — the highest migration
 * version in {@link MIGRATIONS}.
 */
export const CURRENT_SCHEMA_VERSION: number = MIGRATIONS.reduce(
  (max, m) => Math.max(max, m.version),
  0
);

/**
 * Validates the migration list shape, failing loudly with
 * {@link ErrMigrationConflict} on a non-monotonic / duplicate version or a
 * destructive table operation. Called by the driver on open and by
 * `migrations.spec.ts`.
 */
export function assertMigrationsWellFormed(migrations: readonly Migration[] = MIGRATIONS): void {
  let prev = 0;
  const seenTables = new Set<string>();
  for (const m of migrations) {
    if (m.version !== prev + 1) {
      throw new ErrMigrationConflict(
        `console-db: migration versions must be contiguous and 1-based; ` +
          `expected ${prev + 1}, got ${m.version}`
      );
    }
    prev = m.version;
    for (const t of m.createTables) {
      if (seenTables.has(t)) {
        throw new ErrMigrationConflict(
          `console-db: table "${t}" is created by more than one migration ` +
            `(forward-only: each table is created exactly once)`
        );
      }
      seenTables.add(t);
    }
  }
}

/**
 * The driver-facing apply hook. `applyOne` is the storage-specific callback
 * that materialises one migration (creates its object stores). `runMigrations`
 * returns the count of migrations newly applied.
 *
 * @param appliedVersions  versions already recorded in `schema_migrations`
 * @param applyOne         storage-specific create-stores callback
 */
export async function runMigrations(
  appliedVersions: ReadonlySet<number>,
  applyOne: (m: Migration) => Promise<void>
): Promise<number> {
  assertMigrationsWellFormed();
  let applied = 0;
  for (const m of MIGRATIONS) {
    if (appliedVersions.has(m.version)) continue;
    await applyOne(m);
    applied += 1;
  }
  return applied;
}
