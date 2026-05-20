/**
 * Console DB driver interface (Phase 72h).
 *
 * The `ConsoleDB` interface is the §4.4 extensibility seam: the V1 default
 * driver is IndexedDB (`drivers/indexeddb.ts`); a future Console-side
 * server-backed driver fits the same shape without reshaping callers.
 * Every method takes `operatorID` first — per-operator scoping is
 * mandatory and cross-operator reads/writes are not exposed (acceptance
 * criterion: "Per-operator scoping is mandatory").
 */
import type { OperatorIdentity } from '../protocol.js';
import type {
  AuthProfile,
  KeybindingRow,
  NotificationRoutingRow,
  PATEntry,
  Profile,
  RuntimeRegistryRow,
  SavedFilter,
  SavedView
} from './schema.js';

/**
 * Per-table CRUD surface, identity-scoped. Every method's first argument
 * is the `operator_id` row-scope key; the driver applies it as the row
 * filter at the storage edge.
 */
export interface TableScope<T> {
  /** Lists every row owned by `operatorID`. */
  list(operatorID: string): Promise<T[]>;
  /** Returns the row `id` owned by `operatorID`, or `null` if absent. */
  get(operatorID: string, id: string): Promise<T | null>;
  /** Inserts or replaces a row; `row.operator_id` must equal `operatorID`. */
  upsert(operatorID: string, row: T): Promise<void>;
  /** Deletes the row `id` owned by `operatorID` (no-op if absent). */
  delete(operatorID: string, id: string): Promise<void>;
}

/**
 * The Console-local datastore. Holds Console-local state only (D-061);
 * never a shadow source of truth for runtime entities.
 */
export interface ConsoleDB {
  /** Opens the underlying store and runs pending migrations. */
  open(): Promise<void>;
  /** Closes the underlying store. */
  close(): Promise<void>;

  readonly savedFilters: TableScope<SavedFilter>;
  readonly savedViews: TableScope<SavedView>;
  readonly profiles: TableScope<Profile>;
  readonly runtimes: TableScope<RuntimeRegistryRow>;
  /** `encrypted_jwt_blob` is opaque AES-GCM ciphertext. */
  readonly authProfiles: TableScope<AuthProfile>;
  /** `encrypted_token_blob` is opaque AES-GCM ciphertext. */
  readonly patStore: TableScope<PATEntry>;
  readonly notifications: TableScope<NotificationRoutingRow>;
  readonly keybindings: TableScope<KeybindingRow>;
}

/** Options for {@link openConsoleDB}. */
export interface ConsoleDBOptions {
  /** Driver name; V1 registers only `"indexeddb"`. */
  driver?: 'indexeddb';
  /** `(tenantID, userID)` from the active Protocol session; hashed to `operator_id`. */
  operatorIdentity: OperatorIdentity;
  /** AES-GCM master key derived from the operator passphrase via `crypto.ts`. */
  masterKey: CryptoKey;
  /**
   * IndexedDB database name. Defaults to `"harbor-console"`. Tests pass a
   * unique name per case so `fake-indexeddb` instances do not collide.
   */
  databaseName?: string;
}

/** Constructor signature every driver self-registers under. */
export type DriverFactory = (opts: ConsoleDBOptions) => ConsoleDB;
