/**
 * Console DB public API (Phase 72h).
 *
 * `openConsoleDB` is the single entry point the Console SvelteKit pages
 * use. It dispatches by driver name via a registry — V1 registers only
 * `"indexeddb"`; the seam is ready for a post-V1 `"server"` driver without
 * reshaping callers.
 *
 * §13 / D-061: the Console DB holds Console-local state only. See
 * `schema.ts` for the per-table carve-out disclaimers.
 */
import { ErrUnknownDriver } from './errors.js';
import type { ConsoleDB, ConsoleDBOptions, DriverFactory } from './driver.js';
import { DRIVER_NAME, createIndexedDBDriver } from './drivers/indexeddb.js';

/** Driver registry — write-once at module init, read-many (§4.4 seam pattern). */
const DRIVERS: Map<string, DriverFactory> = new Map([
  [DRIVER_NAME, createIndexedDBDriver]
]);

/** The default driver name when `opts.driver` is omitted. */
export const DEFAULT_DRIVER = DRIVER_NAME;

/**
 * Opens a Console DB, running any pending forward-only migrations.
 *
 * Fails loudly with {@link ErrUnknownDriver} if `opts.driver` names a
 * driver that is not registered (the error lists the registered drivers).
 */
export async function openConsoleDB(opts: ConsoleDBOptions): Promise<ConsoleDB> {
  const name = opts.driver ?? DEFAULT_DRIVER;
  const factory = DRIVERS.get(name);
  if (!factory) {
    throw new ErrUnknownDriver(name, [...DRIVERS.keys()]);
  }
  const db = factory(opts);
  await db.open();
  return db;
}

/** Names of every registered driver (V1: `["indexeddb"]`). */
export function registeredDrivers(): string[] {
  return [...DRIVERS.keys()];
}

/* ---- Re-exports: the Console DB module's public surface ---- */
export type { ConsoleDB, ConsoleDBOptions, TableScope } from './driver.js';
export type {
  AuthProfile,
  KeybindingRow,
  ListPage,
  NotificationClass,
  NotificationRoutingRow,
  NotificationTransport,
  PATEntry,
  Profile,
  RuntimeRegistryRow,
  SavedFilter,
  SavedView,
  TableName,
  TableRowMap,
  Transport
} from './schema.js';
export {
  FORBIDDEN_TABLE_NAMES,
  JWT_ALGORITHMS,
  LIST_PAGES,
  NOTIFICATION_CLASSES,
  NOTIFICATION_TRANSPORTS,
  TABLE_NAMES,
  operatorIdOf,
  validateRow
} from './schema.js';
export {
  DEFAULT_PBKDF2_ITERATIONS,
  decrypt,
  deriveMasterKey,
  encrypt,
  generateKdfSalt,
  ivOf
} from './crypto.js';
export {
  CURRENT_SCHEMA_VERSION,
  MIGRATIONS,
  assertMigrationsWellFormed
} from './migrations.js';
export {
  ErrAuthDecryption,
  ErrMigrationConflict,
  ErrMissingOperator,
  ErrSchemaValidation,
  ErrUnknownDriver
} from './errors.js';
