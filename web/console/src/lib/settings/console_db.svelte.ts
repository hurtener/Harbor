// Console Settings — Console-DB-backed local state controller
// (Phase 73m / D-129 — Svelte 5 runes mode, D-092).
//
// The Settings page is the operator-facing surface for the Console DB's
// Console-local tables (72h / D-113): `runtime_registry` (the runtime
// address book), `profiles` (theme / density / motion / tz / locale),
// `keybindings`, `notifications_routing`, plus the encrypted
// `auth_profiles` / `pat_store` metadata. Phase 73m CONSUMES 72h's
// schema + driver — it ships NO new table and re-implements NO crypto
// (the AES-GCM helpers are 72h's, called via the Console DB driver).
//
// D-061: every table here is Console-local state — never a runtime
// entity. The runtime is unaware it is in the `runtime_registry`.

import { attachConnection, resolveConnection } from '$lib/connection.js';
import { openListPageDB } from '$lib/db/console_db.js';
import { operatorIdOf } from '$lib/db/index.js';
import type {
	ConsoleDB,
	RuntimeRegistryRow,
	Profile,
	KeybindingRow,
	NotificationRoutingRow,
	AuthProfile,
	PATEntry
} from '$lib/db/index.js';

/** A ULID-ish id for new Console-DB rows. Console-local; not a runtime id. */
function newRowID(): string {
	const rnd = globalThis.crypto.getRandomValues(new Uint8Array(10));
	let hex = '';
	for (const b of rnd) hex += b.toString(16).padStart(2, '0');
	return Date.now().toString(36) + '-' + hex;
}

/**
 * SettingsDBController owns the Settings page's Console-DB-backed local
 * state. It opens the Console DB lazily on first use; when the Console
 * is not attached to a Runtime (`connection.ts` → null) it stays empty.
 */
export class SettingsDBController {
	/** The attached-runtime address book (72h `runtime_registry`). */
	runtimes = $state<RuntimeRegistryRow[]>([]);
	/** The operator preference record (72h `profiles`). */
	profile = $state<Profile | null>(null);
	/** The operator's keybinding overrides (72h `keybindings`). */
	keybindings = $state<KeybindingRow[]>([]);
	/** The notification-routing matrix rows (72h `notifications_routing`). */
	routing = $state<NotificationRoutingRow[]>([]);
	/** The per-runtime encrypted auth-profile metadata (72h `auth_profiles`). */
	authProfiles = $state<AuthProfile[]>([]);
	/** The Console-local PAT metadata (72h `pat_store`). */
	pats = $state<PATEntry[]>([]);
	/** True once `load()` has resolved against the Console DB. */
	loaded = $state(false);
	/** A non-fatal load error message; null when clean. */
	loadError = $state<string | null>(null);
	/**
	 * A non-fatal add-runtime warning surfaced by {@link addRuntime} when
	 * `localStorage` was updated successfully (the operator's primary
	 * intent — "make the Console talk to this Runtime" — landed) but the
	 * Console DB write could not complete (e.g. the DB is not yet open
	 * because no Runtime had been attached when the page loaded). The
	 * Settings page surfaces this as an info banner, NOT a red error;
	 * the address-book catch-up routine in {@link load} closes the loop
	 * on the next page reload. Phase 83u / D-163.
	 */
	addWarning = $state<string | null>(null);

	#db: ConsoleDB | null = null;
	#operatorID = '';

	/** True when the Console is attached to a Runtime. */
	get connected(): boolean {
		return resolveConnection() !== null;
	}

	/**
	 * load opens the Console DB for the active operator and reads every
	 * Settings-relevant table. A failure surfaces in `loadError` — never
	 * a silent empty result (CLAUDE.md §13).
	 *
	 * Phase 83u / D-163 — address-book catch-up. After the DB opens,
	 * if the active connection's `base_url` is not already in the
	 * `runtime_registry`, it is upserted with `is_default: 1`. This
	 * closes the loop for the F3 "chicken-and-egg" first-attach: the
	 * Settings page's add-runtime form writes `localStorage` first
	 * (always works, no DB needed) and reloads; the catch-up then
	 * promotes the active connection into the address book.
	 */
	async load(): Promise<void> {
		const conn = resolveConnection();
		if (conn === null) {
			this.loaded = true;
			return;
		}
		try {
			this.#db = await openListPageDB(conn);
			this.#operatorID = await operatorIdOf(conn.identity.tenant, conn.identity.user);
			const [runtimes, profiles, keybindings, routing, authProfiles, pats] =
				await Promise.all([
					this.#db.runtimes.list(this.#operatorID),
					this.#db.profiles.list(this.#operatorID),
					this.#db.keybindings.list(this.#operatorID),
					this.#db.notifications.list(this.#operatorID),
					this.#db.authProfiles.list(this.#operatorID),
					this.#db.patStore.list(this.#operatorID)
				]);
			this.runtimes = runtimes;
			this.profile = profiles[0] ?? null;
			this.keybindings = keybindings;
			this.routing = routing;
			this.authProfiles = authProfiles;
			this.pats = pats;
			this.loaded = true;
			this.loadError = null;
			await this.#catchUpAddressBook(conn.baseURL);
		} catch (e) {
			this.loadError = e instanceof Error ? e.message : 'Console DB open failed';
			this.loaded = true;
		}
	}

	/**
	 * #catchUpAddressBook upserts the active connection's `base_url`
	 * into the `runtime_registry` if it isn't already there. Phase 83u
	 * / D-163. Best-effort: a failure here does not break `load` (the
	 * page already rendered with the listed runtimes — the catch-up is
	 * a convenience).
	 */
	async #catchUpAddressBook(activeBaseURL: string): Promise<void> {
		if (this.#db === null) {
			return;
		}
		const normalised = activeBaseURL.replace(/\/$/, '');
		const present = this.runtimes.some((r) => r.base_url.replace(/\/$/, '') === normalised);
		if (present) {
			return;
		}
		try {
			const now = Date.now();
			const row: RuntimeRegistryRow = {
				operator_id: this.#operatorID,
				id: newRowID(),
				created_at: now,
				updated_at: now,
				name: normalised,
				base_url: normalised,
				transport: 'sse_rest',
				is_default: 1,
				last_connected_at: now,
				protocol_version: null
			};
			await this.#db.runtimes.upsert(this.#operatorID, row);
			this.runtimes = [...this.runtimes, row];
		} catch {
			// Catch-up is best-effort; the page already rendered. The next
			// load will retry. We deliberately do not surface this as a
			// loadError — the operator's connection is live regardless.
		}
	}

	/**
	 * addRuntime is the operator's primary "make the Console talk to
	 * this Runtime" gesture. Phase 83u / D-163 splits its two effects:
	 *
	 *   1. **Active connection.** Write `localStorage`'s
	 *      `harbor.runtime.base_url` (via {@link attachConnection}).
	 *      Always works — no Console DB dependency. This is what makes
	 *      the next page load actually attach.
	 *   2. **Address book persistence.** Best-effort upsert into the
	 *      Console DB `runtime_registry` table. When the DB is not yet
	 *      open (the operator's first attach — F3 chicken-and-egg),
	 *      this step is deferred: the next page reload opens the DB
	 *      with the now-live connection, and the address-book catch-up
	 *      in {@link load} promotes the active URL into the registry.
	 *
	 * A DB-write failure surfaces in `addWarning` and is NOT thrown —
	 * the operator's primary intent landed regardless. The Settings
	 * page's form reload-on-success closes the deferred DB write the
	 * next time the page mounts.
	 */
	async addRuntime(name: string, baseURL: string): Promise<void> {
		this.addWarning = null;
		// (1) Active connection — the primary effect. Always succeeds.
		attachConnection(baseURL);
		// (2) Address book persistence — best-effort.
		if (this.#db === null) {
			this.addWarning =
				'Saved as the active runtime; address-book persistence will happen after first page reload.';
			return;
		}
		try {
			const now = Date.now();
			const row: RuntimeRegistryRow = {
				operator_id: this.#operatorID,
				id: newRowID(),
				created_at: now,
				updated_at: now,
				name,
				base_url: baseURL,
				transport: 'sse_rest',
				is_default: this.runtimes.length === 0 ? 1 : 0,
				last_connected_at: null,
				protocol_version: null
			};
			await this.#db.runtimes.upsert(this.#operatorID, row);
			this.runtimes = [...this.runtimes, row];
		} catch (e) {
			this.addWarning = `Saved as the active runtime; address-book write failed (${
				e instanceof Error ? e.message : 'unknown error'
			}). The next page reload will retry.`;
		}
	}

	/** removeRuntime deletes a runtime from the address book. */
	async removeRuntime(id: string): Promise<void> {
		if (this.#db === null) {
			throw new Error('Console DB not open');
		}
		await this.#db.runtimes.delete(this.#operatorID, id);
		this.runtimes = this.runtimes.filter((r) => r.id !== id);
	}
}
