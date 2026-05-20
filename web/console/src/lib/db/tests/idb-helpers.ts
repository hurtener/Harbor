/**
 * IndexedDB test helpers (Phase 72h).
 *
 * `fake-indexeddb` instances are keyed by database name; tests use a
 * unique name per case so a prior case's stores never bleed into the next.
 */
import { deriveMasterKey, generateKdfSalt } from '../crypto.js';
import type { ConsoleDBOptions } from '../driver.js';

let dbCounter = 0;

/** Returns a process-unique IndexedDB database name for a test case. */
export function freshDBName(prefix = 'test'): string {
  dbCounter += 1;
  return `harbor-console-${prefix}-${dbCounter}`;
}

/**
 * Builds `ConsoleDBOptions` with a real PBKDF2-derived master key. The
 * key is needed by the options shape; the encrypted-blob round-trip in
 * the integration test exercises it end-to-end.
 */
export async function makeOptionsAsync(
  overrides: Partial<ConsoleDBOptions> = {}
): Promise<ConsoleDBOptions> {
  const masterKey = await deriveMasterKey('test-passphrase', generateKdfSalt());
  return {
    operatorIdentity: { tenantID: 'tenant-test', userID: 'user-test' },
    masterKey,
    databaseName: freshDBName(),
    ...overrides
  };
}

/**
 * Synchronous options builder for tests that do not exercise the master
 * key. Uses a placeholder `CryptoKey` cast — never used for crypto here.
 */
export function makeOptions(overrides: Partial<ConsoleDBOptions> = {}): ConsoleDBOptions {
  return {
    operatorIdentity: { tenantID: 'tenant-test', userID: 'user-test' },
    // The migration / CRUD path never touches masterKey; encryption is
    // exercised via the real key in the integration test.
    masterKey: {} as CryptoKey,
    databaseName: freshDBName(),
    ...overrides
  };
}
