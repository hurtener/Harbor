// Console Settings — Console-DB-backed saved-view controller
// (Phase 73m / D-129 — Svelte 5 runes mode, D-092).
//
// CONVENTIONS.md §5 depth bar: a Console page carries Console-DB-backed
// `SavedViewChips`. For the Settings page a "saved view" is a
// section-bookmark preset — a named jump to one of the 12 Settings
// sections. The controller wraps the shipped `saved_filters` Console DB
// table (D-061: Console-local state only — NO new table; the `page`
// discriminator column is scoped to `'settings'`).

import { resolveConnection } from '$lib/connection.js';
import { openListPageDB } from '$lib/db/console_db.js';
import { operatorIdOf } from '$lib/db/index.js';
import type { ConsoleDB, SavedFilter } from '$lib/db/index.js';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import type { SettingsSectionId } from './state.svelte.js';

/** The `page` discriminator value Settings-page saved-view rows use. */
const SETTINGS_SAVED_PAGE = 'settings';

/** A ULID-ish id for new rows. */
function newRowID(): string {
	const rnd = globalThis.crypto.getRandomValues(new Uint8Array(8));
	let hex = '';
	for (const b of rnd) hex += b.toString(16).padStart(2, '0');
	return Date.now().toString(36) + '-' + hex;
}

/**
 * SettingsSavedViews owns the Settings page's saved-view chip state. A
 * saved view records a section bookmark; selecting a chip jumps the
 * sub-nav rail to that section. It opens the Console DB lazily; when
 * the Console is not attached it stays empty.
 */
export class SettingsSavedViews {
	/** The saved-view chips, in the shared `<SavedViewChips>` shape. */
	views = $state<SavedView[]>([]);

	#db: ConsoleDB | null = null;
	#operatorID = '';
	/** section id keyed by saved-view row id, for `sectionFor`. */
	#sectionByID = new Map<string, SettingsSectionId>();

	/**
	 * load opens the Console DB and reads the `page='settings'` saved
	 * filters. A failure leaves `views` empty — the chips degrade
	 * gracefully (the rail still navigates).
	 */
	async load(): Promise<void> {
		const conn = resolveConnection();
		if (conn === null) {
			return;
		}
		try {
			this.#db = await openListPageDB(conn);
			this.#operatorID = await operatorIdOf(conn.identity.tenant, conn.identity.user);
			const rows = await this.#db.savedFilters.list(this.#operatorID);
			this.#ingest(rows.filter((r) => r.page === SETTINGS_SAVED_PAGE));
		} catch {
			// Console DB open failed — the chips stay empty; the sub-nav
			// rail still works. No silent runtime degradation (the rail's
			// navigation does not depend on the DB).
			this.views = [];
		}
	}

	#ingest(rows: SavedFilter[]): void {
		this.#sectionByID.clear();
		this.views = rows.map((r) => {
			let section: SettingsSectionId = 'connected-runtimes';
			try {
				const spec = JSON.parse(r.filter_spec_json) as { section?: SettingsSectionId };
				if (spec.section) {
					section = spec.section;
				}
			} catch {
				// Malformed preset JSON — default to the first section.
			}
			this.#sectionByID.set(r.id, section);
			return { id: r.id, name: r.name };
		});
	}

	/** sectionFor returns the section a saved-view chip jumps to. */
	sectionFor(id: string): SettingsSectionId | null {
		return this.#sectionByID.get(id) ?? null;
	}

	/** create persists a section bookmark as a new saved-view chip. */
	async create(name: string, section: SettingsSectionId): Promise<void> {
		if (this.#db === null) {
			return;
		}
		const now = Date.now();
		const row: SavedFilter = {
			operator_id: this.#operatorID,
			id: newRowID(),
			created_at: now,
			updated_at: now,
			page: SETTINGS_SAVED_PAGE,
			name,
			filter_spec_json: JSON.stringify({ section })
		};
		await this.#db.savedFilters.upsert(this.#operatorID, row);
		this.#sectionByID.set(row.id, section);
		this.views = [...this.views, { id: row.id, name: row.name }];
	}

	/** remove deletes a saved-view chip. */
	async remove(id: string): Promise<void> {
		if (this.#db === null) {
			return;
		}
		await this.#db.savedFilters.delete(this.#operatorID, id);
		this.#sectionByID.delete(id);
		this.views = this.views.filter((v) => v.id !== id);
	}
}
