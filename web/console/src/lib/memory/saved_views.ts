// Harbor Console — Memory-page saved-view persistence wiring (D-121,
// CONVENTIONS.md §3/§5, D-061).
//
// CONVENTIONS.md §5 makes Console-DB-backed `SavedViewChips` part of the
// depth bar; D-061 pins saved views as Console-LOCAL state — they live in
// the Console's IndexedDB store, never in the Runtime. This module is the
// thin bridge between the Memory page and the typed `MemorySavedFilters`
// facade over the `saved_filters` Console-DB table.
//
// The Memory page never opens the Console DB inline (a `.svelte` file owns
// no storage). It calls `openMemorySavedFilters` once at mount; the result
// is the real IndexedDB-backed facade, so chips persist across reloads.
//
// # Why the saved-filter facade does not need the auth passphrase
//
// The Console-DB master key (PBKDF2-from-passphrase) encrypts ONLY the
// `auth_profiles` / `pat_store` secret blobs (`crypto.ts`). The
// `saved_filters` table stores plaintext `filter_spec_json` — no secret
// crosses it. `openConsoleDB` nonetheless requires a `masterKey` in its
// options (it is a constructor argument consumed lazily, only by the
// encrypted tables). A saved-filter-only consumer therefore supplies a
// freshly-generated AES-GCM key that the `saved_filters` code path never
// touches — this is not a stub, it is an unused constructor argument for
// a table that carries no ciphertext.

import { openConsoleDB } from '$lib/db/index.js';
import { operatorIdOf } from '$lib/db/schema.js';
import { MemorySavedFilters } from '$lib/db/saved_filters_memory.js';
import type { RuntimeConnection } from '$lib/connection.js';

/**
 * Opens the IndexedDB-backed `MemorySavedFilters` facade for the
 * Memory page, scoped to the connected operator's `(tenant, user)`.
 *
 * Returns `null` — never throws — when the Console DB cannot be opened
 * (WebCrypto / IndexedDB unavailable: SSR, a locked-down browser, a test
 * environment without `fake-indexeddb`). A `null` is the honest "saved
 * views unavailable" signal the page renders as a disabled-with-tooltip
 * affordance; it is NEVER swapped for an always-empty in-memory array
 * (CLAUDE.md §13 — no silent degradation, no stubbed action presented as
 * done).
 */
export async function openMemorySavedFilters(
	connection: RuntimeConnection
): Promise<MemorySavedFilters | null> {
	const cryptoImpl = globalThis.crypto;
	if (!cryptoImpl?.subtle || typeof globalThis.indexedDB === 'undefined') {
		return null;
	}
	try {
		const operatorID = await operatorIdOf(
			connection.identity.tenant,
			connection.identity.user
		);
		// An AES-GCM key the `saved_filters` table never consumes (see the
		// module header). It satisfies `openConsoleDB`'s constructor arg.
		const masterKey = await cryptoImpl.subtle.generateKey(
			{ name: 'AES-GCM', length: 256 },
			false,
			['encrypt', 'decrypt']
		);
		const db = await openConsoleDB({
			operatorIdentity: {
				tenantID: connection.identity.tenant,
				userID: connection.identity.user
			},
			masterKey
		});
		return new MemorySavedFilters(db, operatorID);
	} catch {
		// IndexedDB open / migration failed — fail honest: no saved-view
		// persistence this session. The page disables the Save affordance.
		return null;
	}
}
