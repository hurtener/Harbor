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

import { resolveConnection } from '$lib/connection.js';
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
		} catch (e) {
			this.loadError = e instanceof Error ? e.message : 'Console DB open failed';
			this.loaded = true;
		}
	}

	/**
	 * addRuntime appends a runtime to the address book (72h
	 * `runtime_registry`). The runtime is unaware it is in this list —
	 * it is Console-local state (D-061). Returns the new row's id.
	 */
	async addRuntime(name: string, baseURL: string): Promise<void> {
		if (this.#db === null) {
			throw new Error('Console DB not open — attach to a Runtime first');
		}
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
