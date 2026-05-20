/**
 * Events-page saved-filter chips — a TYPED WRAPPER over the Phase 72h
 * `saved_filters` Console DB table (Phase 73g / D-125).
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'events'` discriminator.
 * `saved_filters` already carries a `page` column precisely so each list
 * page's saved filters coexist in one table; `saved_filters_events.ts`
 * is the Events page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to an `events.subscribe` / `events.aggregate`
 * Protocol call. It is NEVER a runtime entity: the Runtime does not know
 * these presets exist. The `filter_spec_json` column stores a
 * JSON-encoded `EventFacetState` (the page's Console-local facet shape);
 * the wrapper marshals / unmarshals it. Selecting a chip rewrites the
 * filter chips and re-opens the subscription — no Protocol method
 * mutates Console state.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { EventFacetState } from '../events/filters.js';

/** The `saved_filters.page` discriminator value for the Events page. */
export const EVENTS_SAVED_FILTER_PAGE: SavedFilter['page'] = 'events';

/**
 * An Events-page saved filter, decoded. The `filterSpec` is the typed
 * {@link EventFacetState} the page applies to its faceted chips; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface EventsSavedFilter {
	/** Table-local primary key. */
	id: string;
	/** Operator-facing chip label. */
	name: string;
	/** The decoded facet-filter preset this chip applies. */
	filterSpec: EventFacetState;
}

/** Unix-epoch-millis clock; injectable for deterministic tests. */
type Clock = () => number;

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
	const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
	return `evf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/**
 * Decodes a `saved_filters` row into a typed {@link EventsSavedFilter}.
 * A malformed `filter_spec_json` throws — never a silently dropped chip
 * (CLAUDE.md §13, fail loudly).
 */
function decode(row: SavedFilter): EventsSavedFilter {
	let spec: EventFacetState;
	try {
		spec = JSON.parse(row.filter_spec_json) as EventFacetState;
	} catch (e) {
		throw new Error(
			`saved_filters_events: row ${row.id} has malformed filter_spec_json: ${String(e)}`
		);
	}
	return { id: row.id, name: row.name, filterSpec: spec };
}

/**
 * The Events-page saved-filter store — a typed list / create / delete
 * surface over the `saved_filters` table, scoped to `page = 'events'`.
 * Construct one per (operator, DB) pair; the operator ID is the Console
 * DB row-scope key. Cross-page rows are never returned or mutated.
 */
export class EventsSavedFilters {
	readonly #db: ConsoleDB;
	readonly #operatorID: string;
	readonly #now: Clock;

	constructor(db: ConsoleDB, operatorID: string, now: Clock = () => Date.now()) {
		this.#db = db;
		this.#operatorID = operatorID;
		this.#now = now;
	}

	/** Lists every Events-page saved filter for the operator, name-sorted. */
	async list(): Promise<EventsSavedFilter[]> {
		const rows = await this.#db.savedFilters.list(this.#operatorID);
		return rows
			.filter((r) => r.page === EVENTS_SAVED_FILTER_PAGE)
			.map(decode)
			.sort((a, b) => a.name.localeCompare(b.name));
	}

	/** Returns one Events-page saved filter by id, or `null` when absent / off-page. */
	async get(id: string): Promise<EventsSavedFilter | null> {
		const row = await this.#db.savedFilters.get(this.#operatorID, id);
		if (row === null || row.page !== EVENTS_SAVED_FILTER_PAGE) {
			return null;
		}
		return decode(row);
	}

	/**
	 * Creates a new Events-page saved filter. Returns the minted record.
	 * The `page` column is fixed to `'events'` — an Events-page filter
	 * never leaks into another page's view.
	 */
	async create(name: string, filterSpec: EventFacetState): Promise<EventsSavedFilter> {
		const ts = this.#now();
		const row: SavedFilter = {
			operator_id: this.#operatorID,
			id: mintID(),
			created_at: ts,
			updated_at: ts,
			page: EVENTS_SAVED_FILTER_PAGE,
			name,
			filter_spec_json: JSON.stringify(filterSpec)
		};
		await this.#db.savedFilters.upsert(this.#operatorID, row);
		return decode(row);
	}

	/**
	 * Deletes an Events-page saved filter. Guarded so an Events-page
	 * caller cannot delete another page's chip by id; a no-op when the
	 * row is absent.
	 */
	async delete(id: string): Promise<void> {
		const existing = await this.#db.savedFilters.get(this.#operatorID, id);
		if (existing === null || existing.page !== EVENTS_SAVED_FILTER_PAGE) {
			return;
		}
		await this.#db.savedFilters.delete(this.#operatorID, id);
	}
}
