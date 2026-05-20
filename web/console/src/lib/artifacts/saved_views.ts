/**
 * Artifacts page — Console-DB-backed saved-view store wiring (D-121,
 * CONVENTIONS.md §3/§5; D-061).
 *
 * The Artifacts page's `SavedViewChips` are Console-LOCAL state — named,
 * persisted `artifacts.list` facet presets that live in the Console's
 * IndexedDB store, never in the Runtime (D-061). The legacy page rendered
 * the chip labels as a hardcoded string array; this module wires them onto
 * the shipped `saved_filters` Console DB table via the
 * {@link ArtifactsSavedFilters} typed wrapper.
 *
 * # Why the wiring lives here, not in `+page.svelte`
 *
 * A `.svelte` component never opens the Console DB directly — the same
 * discipline that keeps `localStorage` reads out of components
 * (CONVENTIONS.md §6). This `.ts` module is the single seam: the page
 * imports {@link openArtifactsSavedViewStore} and gets back a typed,
 * Promise-returning store, or `null` when the Console DB cannot be opened
 * (a non-browser / test context with no IndexedDB).
 *
 * # The master key
 *
 * `openConsoleDB` requires an AES-GCM master key (it gates the encrypted
 * `auth_profiles` / `pat_store` tables). `saved_filters` itself is NOT an
 * encrypted table, so the key only has to be *stable per operator* — it
 * never has to match a passphrase. This module derives the key from the
 * resolved connection token and a per-operator KDF salt persisted in
 * `localStorage`, so reloads reuse the same key and the same IndexedDB
 * instance. The token is operator-scoped; the derived key never leaves
 * WebCrypto (it is non-extractable).
 */
import { resolveConnection } from '../connection.js';
import {
  openConsoleDB,
  operatorIdOf,
  deriveMasterKey,
  generateKdfSalt
} from '../db/index.js';
import { ArtifactsSavedFilters } from '../db/saved_filters_artifacts.js';

/** `localStorage` key the per-operator KDF salt is persisted under. */
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
 * Reads the persisted per-operator KDF salt, minting and persisting a new
 * one on first use. A stable salt means reloads reuse the same IndexedDB
 * instance.
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
 * Opens the Console-DB-backed Artifacts saved-view store for the active
 * operator. Returns `null` when the Console is not attached to a Runtime
 * (no connection identity) or `localStorage` / IndexedDB is unavailable
 * (SSR / a test context without `fake-indexeddb`) — the page treats a
 * `null` store as "no saved views" rather than failing the whole page.
 * A genuine DB error (a failed migration, a corrupt store) is NOT
 * swallowed — it propagates so the page surfaces it (CLAUDE.md §13).
 */
export async function openArtifactsSavedViewStore(): Promise<ArtifactsSavedFilters | null> {
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
  return new ArtifactsSavedFilters(db, operatorID);
}
