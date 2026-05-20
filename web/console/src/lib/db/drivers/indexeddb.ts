/**
 * Console DB IndexedDB driver (Phase 72h) — the default V1 driver.
 *
 * Native IndexedDB API (no Dexie dependency — keeping the Console
 * dependency surface minimal; the `ConsoleDB` interface is the seam, and a
 * thin native wrapper is sufficient for the eight-table CRUD shape).
 *
 * Each table is an IndexedDB object store keyed on the compound
 * `[operator_id, id]` so per-operator scoping is a structural property of
 * the key, not an application-layer filter — operator B's rows are
 * unreachable from an operator-A-scoped query. The IDB database `version`
 * tracks the Console DB migration version: opening at version N triggers
 * `onupgradeneeded`, which creates the object stores for every migration
 * up to N and records the applied versions in `schema_migrations`.
 */
import { ErrMissingOperator } from '../errors.js';
import {
  CURRENT_SCHEMA_VERSION,
  MIGRATIONS,
  SCHEMA_MIGRATIONS_STORE,
  assertMigrationsWellFormed
} from '../migrations.js';
import { TABLE_NAMES, validateRow, type TableName, type TableRowMap } from '../schema.js';
import type { ConsoleDB, ConsoleDBOptions, TableScope } from '../driver.js';

const DEFAULT_DB_NAME = 'harbor-console';

/** Wraps an `IDBRequest` as a promise. */
function reqToPromise<T>(req: IDBRequest<T>): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error ?? new Error('console-db: IndexedDB request failed'));
  });
}

/** Wraps an `IDBTransaction` completion as a promise. */
function txToPromise(tx: IDBTransaction): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error('console-db: IndexedDB transaction failed'));
    tx.onabort = () => reject(tx.error ?? new Error('console-db: IndexedDB transaction aborted'));
  });
}

/** Per-table CRUD scope backed by one IndexedDB object store. */
class IDBTableScope<K extends TableName> implements TableScope<TableRowMap[K]> {
  constructor(
    private readonly getDB: () => IDBDatabase,
    private readonly table: K
  ) {}

  private assertOperator(operatorID: string): void {
    if (!operatorID) throw new ErrMissingOperator();
  }

  async list(operatorID: string): Promise<TableRowMap[K][]> {
    this.assertOperator(operatorID);
    const tx = this.getDB().transaction(this.table, 'readonly');
    const store = tx.objectStore(this.table);
    // Compound key [operator_id, id]: a bound range on operator_id alone
    // returns exactly that operator's rows — cross-operator rows are
    // structurally unreachable.
    const range = IDBKeyRange.bound([operatorID], [operatorID, []]);
    const rows = await reqToPromise(store.getAll(range));
    return rows as TableRowMap[K][];
  }

  async get(operatorID: string, id: string): Promise<TableRowMap[K] | null> {
    this.assertOperator(operatorID);
    const tx = this.getDB().transaction(this.table, 'readonly');
    const store = tx.objectStore(this.table);
    const row = await reqToPromise(store.get([operatorID, id]));
    return (row as TableRowMap[K] | undefined) ?? null;
  }

  async upsert(operatorID: string, row: TableRowMap[K]): Promise<void> {
    this.assertOperator(operatorID);
    // The row's own operator_id MUST match the scope it is written under —
    // a mismatch is a fail-loud bug, never a silent re-key.
    if (row.operator_id !== operatorID) {
      throw new ErrMissingOperator(
        `console-db: row.operator_id (${row.operator_id}) does not match scope (${operatorID})`
      );
    }
    validateRow(this.table, row);
    const tx = this.getDB().transaction(this.table, 'readwrite');
    tx.objectStore(this.table).put(row);
    await txToPromise(tx);
  }

  async delete(operatorID: string, id: string): Promise<void> {
    this.assertOperator(operatorID);
    const tx = this.getDB().transaction(this.table, 'readwrite');
    tx.objectStore(this.table).delete([operatorID, id]);
    await txToPromise(tx);
  }
}

/** IndexedDB-backed {@link ConsoleDB}. */
export class IndexedDBConsoleDB implements ConsoleDB {
  private db: IDBDatabase | null = null;
  private readonly dbName: string;

  readonly savedFilters: TableScope<TableRowMap['saved_filters']>;
  readonly savedViews: TableScope<TableRowMap['saved_views']>;
  readonly profiles: TableScope<TableRowMap['profiles']>;
  readonly runtimes: TableScope<TableRowMap['runtime_registry']>;
  readonly authProfiles: TableScope<TableRowMap['auth_profiles']>;
  readonly patStore: TableScope<TableRowMap['pat_store']>;
  readonly notifications: TableScope<TableRowMap['notifications_routing']>;
  readonly keybindings: TableScope<TableRowMap['keybindings']>;

  constructor(opts: ConsoleDBOptions) {
    this.dbName = opts.databaseName ?? DEFAULT_DB_NAME;
    const db = () => {
      if (!this.db) throw new Error('console-db: open() must be called before any table access');
      return this.db;
    };
    this.savedFilters = new IDBTableScope(db, 'saved_filters');
    this.savedViews = new IDBTableScope(db, 'saved_views');
    this.profiles = new IDBTableScope(db, 'profiles');
    this.runtimes = new IDBTableScope(db, 'runtime_registry');
    this.authProfiles = new IDBTableScope(db, 'auth_profiles');
    this.patStore = new IDBTableScope(db, 'pat_store');
    this.notifications = new IDBTableScope(db, 'notifications_routing');
    this.keybindings = new IDBTableScope(db, 'keybindings');
  }

  async open(): Promise<void> {
    assertMigrationsWellFormed();
    const open = indexedDB.open(this.dbName, CURRENT_SCHEMA_VERSION);

    open.onupgradeneeded = (ev) => {
      const upgradeDB = open.result;
      const tx = open.transaction;
      if (!tx) throw new Error('console-db: upgrade transaction unavailable');

      // schema_migrations: records every applied migration version.
      if (!upgradeDB.objectStoreNames.contains(SCHEMA_MIGRATIONS_STORE)) {
        upgradeDB.createObjectStore(SCHEMA_MIGRATIONS_STORE, { keyPath: 'version' });
      }
      const oldVersion = ev.oldVersion;
      const migStore = tx.objectStore(SCHEMA_MIGRATIONS_STORE);

      // Apply every migration whose version is newer than oldVersion.
      for (const m of MIGRATIONS) {
        if (m.version <= oldVersion) continue;
        for (const table of m.createTables) {
          if (!upgradeDB.objectStoreNames.contains(table)) {
            // Compound [operator_id, id] key: per-operator scoping is
            // structural — see IDBTableScope.list().
            upgradeDB.createObjectStore(table, { keyPath: ['operator_id', 'id'] });
          }
        }
        migStore.put({ version: m.version, description: m.description, applied_at: Date.now() });
      }
    };

    this.db = await reqToPromise(open);
  }

  async close(): Promise<void> {
    if (this.db) {
      this.db.close();
      this.db = null;
    }
  }

  /** Returns the set of applied migration versions (used by `migrations.spec.ts`). */
  async appliedMigrationVersions(): Promise<Set<number>> {
    if (!this.db) throw new Error('console-db: open() must be called first');
    const tx = this.db.transaction(SCHEMA_MIGRATIONS_STORE, 'readonly');
    const rows = await reqToPromise(
      tx.objectStore(SCHEMA_MIGRATIONS_STORE).getAll()
    );
    return new Set((rows as { version: number }[]).map((r) => r.version));
  }
}

/** The driver name this module self-registers under (V1: the only driver). */
export const DRIVER_NAME = 'indexeddb' as const;

/** Factory the registry dispatches to. */
export function createIndexedDBDriver(opts: ConsoleDBOptions): ConsoleDB {
  return new IndexedDBConsoleDB(opts);
}

/** Re-export so callers can assert all eight stores are accounted for. */
export const DRIVER_TABLES: readonly TableName[] = TABLE_NAMES;
