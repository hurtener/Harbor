/**
 * Playground page saved-view chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table. Phase 73n / D-130.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'playground'` discriminator.
 *
 * # Console-local only (D-061)
 *
 * A Playground saved view is a Console-local convenience — a named
 * preset of the Controls-card override values (reasoning effort /
 * temperature / max tokens / system prompt) the operator returns to. It
 * is NEVER a runtime entity: the Runtime does not know these presets
 * exist. The `runs.set_overrides` Protocol method is what carries an
 * override to the Runtime; this store only remembers what the operator
 * typed, Console-side.
 */

import type { ConsoleDB, SavedFilter } from './index.js';

/** The `page` discriminator value Playground rows are scoped under. */
export const PLAYGROUND_SAVED_FILTER_PAGE = 'playground' as const;

/**
 * A Playground saved-view spec — Console-local UI state the operator
 * names and returns to. It mirrors the Controls-card override inputs;
 * every field is optional (an absent field is "leave the default").
 */
export interface PlaygroundViewSpec {
	/** The reasoning-effort preset (`low` / `medium` / `high`). */
	reasoningEffort?: string;
	/** The sampling-temperature preset. */
	temperature?: number;
	/** The max-tokens preset. */
	maxTokens?: number;
	/** The system-prompt-override preset. */
	systemPromptOverride?: string;
	/** Whether the trace toggle is on in this preset. */
	traceOn?: boolean;
}

/** A Playground saved view, decoded. */
export interface PlaygroundSavedFilter {
	/** Table-local primary key. */
	id: string;
	/** Operator-facing chip label. */
	name: string;
	/** The decoded view preset. */
	viewSpec: PlaygroundViewSpec;
	/** Unix epoch millis. */
	createdAt: number;
	/** Unix epoch millis. */
	updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
	const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
	return `pgv-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Playground shape. */
function decode(row: SavedFilter): PlaygroundSavedFilter {
	let spec: PlaygroundViewSpec = {};
	try {
		spec = JSON.parse(row.filter_spec_json) as PlaygroundViewSpec;
	} catch {
		// A corrupt spec degrades to an empty preset — the chip still
		// renders; the corruption is Console-local and self-healing.
		spec = {};
	}
	return {
		id: row.id,
		name: row.name,
		viewSpec: spec,
		createdAt: row.created_at,
		updatedAt: row.updated_at
	};
}

/**
 * The Playground saved-view store — a typed get/list/create/delete
 * surface over the `saved_filters` table, scoped to
 * `page = 'playground'`. Construct one per (operator, DB) pair.
 */
export class PlaygroundSavedFilters {
	readonly #db: ConsoleDB;
	readonly #operatorID: string;

	constructor(db: ConsoleDB, operatorID: string) {
		this.#db = db;
		this.#operatorID = operatorID;
	}

	/** Lists every Playground saved view for the operator, name-sorted. */
	async list(): Promise<PlaygroundSavedFilter[]> {
		const rows = await this.#db.savedFilters.list(this.#operatorID);
		return rows
			.filter((r) => r.page === PLAYGROUND_SAVED_FILTER_PAGE)
			.map(decode)
			.sort((a, b) => a.name.localeCompare(b.name));
	}

	/** Returns one Playground saved view by id, or `null` if absent. */
	async get(id: string): Promise<PlaygroundSavedFilter | null> {
		const row = await this.#db.savedFilters.get(this.#operatorID, id);
		if (row === null || row.page !== PLAYGROUND_SAVED_FILTER_PAGE) {
			return null;
		}
		return decode(row);
	}

	/** Creates a new Playground saved view. Returns the minted record. */
	async create(name: string, viewSpec: PlaygroundViewSpec): Promise<PlaygroundSavedFilter> {
		const now = Date.now();
		const row: SavedFilter = {
			operator_id: this.#operatorID,
			id: mintID(),
			created_at: now,
			updated_at: now,
			page: PLAYGROUND_SAVED_FILTER_PAGE,
			name,
			filter_spec_json: JSON.stringify(viewSpec)
		};
		await this.#db.savedFilters.upsert(this.#operatorID, row);
		return decode(row);
	}

	/** Deletes a Playground saved view (no-op when absent). */
	async delete(id: string): Promise<void> {
		const existing = await this.#db.savedFilters.get(this.#operatorID, id);
		if (existing === null || existing.page !== PLAYGROUND_SAVED_FILTER_PAGE) {
			return;
		}
		await this.#db.savedFilters.delete(this.#operatorID, id);
	}
}
