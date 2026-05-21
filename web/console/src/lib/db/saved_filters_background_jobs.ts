/**
 * Background Jobs page saved-filter chips — a TYPED WRAPPER over the
 * existing `saved_filters` Console DB table (Phase 72h). Phase 73h /
 * D-128.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'background_jobs'`
 * discriminator. `saved_filters` carries a `page` column precisely so
 * each list page's saved filters coexist in one table;
 * `saved_filters_background_jobs.ts` is the Background Jobs page's typed
 * view onto it — the same shape as the Tasks page's
 * `saved_filters_tasks.ts`.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to a `tasks.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `TaskFilter` (the
 * Phase 73d/73h wire facet shape); the wrapper marshals / unmarshals it.
 * The page's `Stuck > 1h` derived chip is likewise Console-local — a
 * pure client-side rule over the row `last_activity_at` field, not a
 * persisted Protocol filter.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { TaskFilter } from '../protocol/tasks.js';

/** The `page` discriminator value Background-Jobs-page rows scope under. */
export const BACKGROUND_JOBS_SAVED_FILTER_PAGE = 'background_jobs' as const;

/**
 * A Background-Jobs-page saved filter, decoded. The `filterSpec` is the
 * typed `TaskFilter` the page applies to its `tasks.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface BackgroundJobsSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: TaskFilter;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `bjf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Background-Jobs shape. */
function decode(row: SavedFilter): BackgroundJobsSavedFilter {
  let spec: TaskFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as TaskFilter;
  } catch {
    // A corrupt spec degrades to "no filter" rather than throwing — the
    // chip still renders; applying it just yields the full job queue.
    // The corruption is Console-local and self-healing on the next save.
    spec = {};
  }
  return {
    id: row.id,
    name: row.name,
    filterSpec: spec,
    createdAt: row.created_at,
    updatedAt: row.updated_at
  };
}

/**
 * The Background-Jobs-page saved-filter store — a typed
 * get/put/list/delete surface over the `saved_filters` table, scoped to
 * `page = 'background_jobs'`. Construct one per (operator, DB) pair; the
 * operator ID is the Console DB row-scope key.
 */
export class BackgroundJobsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Background-Jobs-page saved filter, name-sorted. */
  async list(): Promise<BackgroundJobsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === BACKGROUND_JOBS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one saved filter by id, or `null` if absent. */
  async get(id: string): Promise<BackgroundJobsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== BACKGROUND_JOBS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Background-Jobs-page saved filter. Returns the minted
   * record. The `page` column is fixed to `'background_jobs'` — a
   * Background-Jobs-page filter never leaks into another page's view.
   */
  async create(name: string, filterSpec: TaskFilter): Promise<BackgroundJobsSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: BACKGROUND_JOBS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Updates an existing saved filter. Throws if `id` does not name a
   * Background-Jobs-page row owned by the operator — a fail-loud guard,
   * never a silent no-op.
   */
  async update(
    id: string,
    patch: { name?: string; filterSpec?: TaskFilter }
  ): Promise<BackgroundJobsSavedFilter> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== BACKGROUND_JOBS_SAVED_FILTER_PAGE) {
      throw new Error(`saved_filters_background_jobs: no row with id ${id}`);
    }
    const updated: SavedFilter = {
      ...existing,
      updated_at: Date.now(),
      name: patch.name ?? existing.name,
      filter_spec_json:
        patch.filterSpec !== undefined
          ? JSON.stringify(patch.filterSpec)
          : existing.filter_spec_json
    };
    await this.#db.savedFilters.upsert(this.#operatorID, updated);
    return decode(updated);
  }

  /** Deletes a saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== BACKGROUND_JOBS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
