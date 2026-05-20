/**
 * Overview page saved-view chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 73a / D-127).
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'overview'` discriminator.
 * `saved_filters` carries a `page` column precisely so each page's
 * saved presets coexist in one table.
 *
 * # Console-local only (D-061)
 *
 * An Overview saved view is a Console-local convenience — a named
 * preset of the operator's hub layout: the counter-card aggregation
 * window and the activity-feed type filter. It is NEVER a runtime
 * entity (D-061 forbids a Console DB shadowing runtime entities). The
 * `filter_spec_json` column stores a JSON-encoded {@link OverviewViewSpec}.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { CounterWindow } from '../overview/aggregations.js';

/** The `page` discriminator value Overview rows are scoped under. */
export const OVERVIEW_SAVED_FILTER_PAGE: SavedFilter['page'] = 'overview';

/**
 * An Overview saved-view spec — Console-local hub-layout UI state the
 * operator names and returns to. NOT a runtime query.
 */
export interface OverviewViewSpec {
	/** The counter-card aggregation window the preset selects. */
	window: CounterWindow;
	/** The activity-feed event-type filter; empty = all activity types. */
	activityTypes: string[];
}

/** An Overview saved view, decoded. */
export interface OverviewSavedFilter {
	/** Table-local primary key. */
	id: string;
	/** Operator-facing chip label. */
	name: string;
	/** The decoded view preset this chip applies. */
	viewSpec: OverviewViewSpec;
}

/** Unix-epoch-millis clock; injectable for deterministic tests. */
type Clock = () => number;

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
	const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
	return `ovf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/**
 * Decodes a `saved_filters` row into a typed {@link OverviewSavedFilter}.
 * A malformed `filter_spec_json` throws — never a silently dropped chip
 * (CLAUDE.md §13, fail loudly).
 */
function decode(row: SavedFilter): OverviewSavedFilter {
	let spec: OverviewViewSpec;
	try {
		spec = JSON.parse(row.filter_spec_json) as OverviewViewSpec;
	} catch (e) {
		throw new Error(
			`saved_filters_overview: row ${row.id} has malformed filter_spec_json: ${String(e)}`
		);
	}
	return { id: row.id, name: row.name, viewSpec: spec };
}

/**
 * The Overview-page saved-view store — a typed list / create / delete
 * surface over the `saved_filters` table, scoped to `page = 'overview'`.
 * Construct one per (operator, DB) pair; the operator ID is the Console
 * DB row-scope key. Cross-page rows are never returned or mutated.
 */
export class OverviewSavedFilters {
	readonly #db: ConsoleDB;
	readonly #operatorID: string;
	readonly #now: Clock;

	constructor(db: ConsoleDB, operatorID: string, now: Clock = () => Date.now()) {
		this.#db = db;
		this.#operatorID = operatorID;
		this.#now = now;
	}

	/** Lists every Overview-page saved view for the operator, name-sorted. */
	async list(): Promise<OverviewSavedFilter[]> {
		const rows = await this.#db.savedFilters.list(this.#operatorID);
		return rows
			.filter((r) => r.page === OVERVIEW_SAVED_FILTER_PAGE)
			.map(decode)
			.sort((a, b) => a.name.localeCompare(b.name));
	}

	/** Returns one Overview-page saved view by id, or `null` when absent / off-page. */
	async get(id: string): Promise<OverviewSavedFilter | null> {
		const row = await this.#db.savedFilters.get(this.#operatorID, id);
		if (row === null || row.page !== OVERVIEW_SAVED_FILTER_PAGE) {
			return null;
		}
		return decode(row);
	}

	/**
	 * Creates a new Overview-page saved view. Returns the minted record.
	 * The `page` column is fixed to `'overview'` — an Overview-page view
	 * never leaks into another page's chip list.
	 */
	async create(name: string, viewSpec: OverviewViewSpec): Promise<OverviewSavedFilter> {
		const ts = this.#now();
		const row: SavedFilter = {
			operator_id: this.#operatorID,
			id: mintID(),
			created_at: ts,
			updated_at: ts,
			page: OVERVIEW_SAVED_FILTER_PAGE,
			name,
			filter_spec_json: JSON.stringify(viewSpec)
		};
		await this.#db.savedFilters.upsert(this.#operatorID, row);
		return decode(row);
	}

	/**
	 * Deletes an Overview-page saved view. Guarded so an Overview-page
	 * caller cannot delete another page's chip by id; a no-op when the
	 * row is absent.
	 */
	async delete(id: string): Promise<void> {
		const existing = await this.#db.savedFilters.get(this.#operatorID, id);
		if (existing === null || existing.page !== OVERVIEW_SAVED_FILTER_PAGE) {
			return;
		}
		await this.#db.savedFilters.delete(this.#operatorID, id);
	}
}
