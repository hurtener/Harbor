/**
 * Console DB bootstrap for Console-local list-page state (D-121,
 * CONVENTIONS.md §3/§5; D-061).
 *
 * # Why this module exists
 *
 * The shared `SavedViewChips` (CONVENTIONS.md §3) is Console-DB-backed:
 * a saved filter is Console-local state, never a runtime entity (D-061).
 * `openConsoleDB` requires an `operatorIdentity` and a `masterKey`. The
 * master key is the KEK that encrypts the `auth_profiles` / `pat_store`
 * secret blobs (`crypto.ts`) — list-page tables (`saved_filters`,
 * `saved_views`) store plaintext JSON and never touch it. A list page
 * therefore needs the DB *open*, but does not need (and must not prompt
 * for) the operator's auth passphrase.
 *
 * This module is the single seam that opens the Console DB for the
 * active Runtime connection. It derives a deterministic, per-operator
 * AES key from the identity triple so the IndexedDB store is stable
 * across page reloads without an interactive passphrase. The derived key
 * guards nothing security-sensitive on the list-page tables — those rows
 * are non-secret Console-local presets (D-061). A page that needs the
 * real auth-passphrase KEK (Settings) opens the DB with the operator key
 * instead; this bootstrap is exclusively for list-page saved-view state.
 *
 * The module is reusable: every Console list page that backs its
 * `SavedViewChips` onto the DB resolves through `openListPageDB`.
 */

import { openConsoleDB, type ConsoleDB } from './index.js';
import { deriveMasterKey } from './crypto.js';
import { KDF_SALT_LENGTH } from './crypto.js';
import type { RuntimeConnection } from '../connection.js';

/**
 * Derives a deterministic 16-byte PBKDF2 salt from the operator identity.
 * `deriveMasterKey` mandates a {@link KDF_SALT_LENGTH}-byte salt; a random
 * salt would yield a different key (and thus a fresh, empty store) on
 * every reload. The list-page tables are non-secret, so a deterministic
 * salt is correct here — it makes the saved-view store stable per
 * operator. (Secret-bearing tables use the random per-operator salt
 * persisted on `profiles.kdf_salt`; this bootstrap never touches them.)
 */
function deterministicSalt(connection: RuntimeConnection): Uint8Array {
	const enc = new TextEncoder();
	const seed = enc.encode(
		`harbor-console-listpage:${connection.identity.tenant}:${connection.identity.user}`
	);
	const salt = new Uint8Array(KDF_SALT_LENGTH);
	// Fold the seed into the fixed-length salt. A non-cryptographic fold
	// is sufficient — the salt only needs to be stable + identity-derived.
	for (let i = 0; i < seed.length; i += 1) {
		salt[i % KDF_SALT_LENGTH] ^= seed[i];
	}
	// Guarantee no all-zero byte run by mixing in a fixed nonce.
	const nonce = enc.encode('listpage');
	for (let i = 0; i < KDF_SALT_LENGTH; i += 1) {
		salt[i] ^= nonce[i % nonce.length];
	}
	return salt;
}

/**
 * Opens the Console DB for the active Runtime connection's operator,
 * scoped for Console-local list-page state (saved-view chips).
 *
 * Fails loudly if the DB cannot be opened (CLAUDE.md §5 — no silent
 * degradation); the caller surfaces the failure in the rail's nested
 * `<PageState>` or as a non-fatal warning rather than swallowing it.
 */
export async function openListPageDB(connection: RuntimeConnection): Promise<ConsoleDB> {
	const masterKey = await deriveMasterKey(
		`harbor-console-listpage:${connection.identity.tenant}:${connection.identity.user}`,
		deterministicSalt(connection)
	);
	return openConsoleDB({
		operatorIdentity: {
			tenantID: connection.identity.tenant,
			userID: connection.identity.user
		},
		masterKey
	});
}
