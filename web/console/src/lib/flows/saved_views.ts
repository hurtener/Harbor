/**
 * Flows page — Console-DB-backed saved-view store wiring (D-188 / D-121,
 * CONVENTIONS.md §3/§5; D-061).
 *
 * The Flows page's `SavedViewChips` are Console-LOCAL state — named, persisted
 * `flows.list` facet presets that live in the Console's IndexedDB store, never
 * in the Runtime (D-061). This module is the single seam that opens the typed
 * {@link FlowsSavedFilters} wrapper over the shipped `saved_filters` Console DB
 * table (scoped to `page = 'flows'`), mirroring `artifacts/saved_views.ts`.
 *
 * # Why the wiring lives here, not in `+page.svelte`
 *
 * A `.svelte` component never opens the Console DB directly — the same
 * discipline that keeps `localStorage` reads out of components (CONVENTIONS.md
 * §6). The page imports {@link openFlowsSavedViewStore} and gets back a typed,
 * Promise-returning store, or `null` when the Console DB cannot be opened (a
 * non-browser / test context with no IndexedDB).
 *
 * # The master key
 *
 * `openConsoleDB` requires an AES-GCM master key (it gates the encrypted
 * tables). `saved_filters` is NOT encrypted, so the key only has to be *stable
 * per operator*. It is derived from the resolved connection token + a
 * per-operator KDF salt persisted in `localStorage` under the SAME key the
 * other pages use, so every list page reuses one IndexedDB instance. The
 * derived key never leaves WebCrypto (it is non-extractable).
 */
import { resolveConnection } from '../connection.js';
import {
  openConsoleDB,
  operatorIdOf,
  deriveMasterKey,
  generateKdfSalt
} from '../db/index.js';
import { FlowsSavedFilters } from '../db/saved_filters_flows.js';

/** `localStorage` key the per-operator KDF salt is persisted under (shared). */
const SALT_STORAGE_KEY = 'harbor.console.kdf_salt';

/** Decodes a base64 string into a byte array. */
function fromBase64(b64: string): Uint8Array {
  const binary = atob(b64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
}

/** Encodes a byte array into a base64 string. */
function toBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const b of bytes) {
    binary += String.fromCharCode(b);
  }
  return btoa(binary);
}

/**
 * Reads the persisted per-operator KDF salt, minting and persisting a new one
 * on first use. A stable salt means reloads reuse the same IndexedDB instance.
 */
function resolveSalt(): Uint8Array {
  const stored = localStorage.getItem(SALT_STORAGE_KEY);
  if (stored) {
    return fromBase64(stored);
  }
  const salt = generateKdfSalt();
  localStorage.setItem(SALT_STORAGE_KEY, toBase64(salt));
  return salt;
}

/**
 * Opens the Console-DB-backed Flows saved-view store for the active operator.
 * Returns `null` when the Console is not attached to a Runtime (no connection
 * identity) or `localStorage` / IndexedDB is unavailable (SSR / a test context
 * without `fake-indexeddb`) — the page treats a `null` store as "no saved
 * views" rather than failing the whole page. A genuine DB error (a failed
 * migration, a corrupt store) is NOT swallowed — it propagates so the page
 * surfaces it (CLAUDE.md §13).
 */
export async function openFlowsSavedViewStore(): Promise<FlowsSavedFilters | null> {
  const connection = resolveConnection();
  if (connection === null || typeof localStorage === 'undefined') {
    return null;
  }
  const { tenant, user } = connection.identity;
  const masterKey = await deriveMasterKey(connection.token, resolveSalt());
  const db = await openConsoleDB({
    operatorIdentity: { tenantID: tenant, userID: user },
    masterKey
  });
  const operatorID = await operatorIdOf(tenant, user);
  return new FlowsSavedFilters(db, operatorID);
}
