/**
 * Tasks-page saved-filter chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 72h). Phase 73d / D-123.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'tasks'` discriminator.
 * `saved_filters` carries a `page` column precisely so each list page's
 * saved filters coexist in one table; `saved_filters_tasks.ts` is the
 * Tasks page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to a `tasks.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `TaskFilter` (the
 * Phase 73d wire facet shape); the wrapper marshals / unmarshals it.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { TaskFilter } from '../protocol/tasks.js';

/** The `page` discriminator value Tasks-page rows are scoped under. */
export const TASKS_SAVED_FILTER_PAGE = 'tasks' as const;

/**
 * A Tasks-page saved filter, decoded. The `filterSpec` is the typed
 * `TaskFilter` the Tasks page applies to its `tasks.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface TasksSavedFilter {
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
  return `tkf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Tasks shape. */
function decode(row: SavedFilter): TasksSavedFilter {
  let spec: TaskFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as TaskFilter;
  } catch {
    // A corrupt spec degrades to "no filter" rather than throwing — the
    // chip still renders; applying it just yields the full task list.
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
 * The Tasks-page saved-filter store — a typed get/put/list/delete
 * surface over the `saved_filters` table, scoped to `page = 'tasks'`.
 * Construct one per (operator, DB) pair; the operator ID is the
 * Console DB row-scope key.
 */
export class TasksSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Tasks-page saved filter for the operator, name-sorted. */
  async list(): Promise<TasksSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === TASKS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Tasks-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<TasksSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== TASKS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Tasks-page saved filter. Returns the minted record.
   * The `page` column is fixed to `'tasks'` — a Tasks-page filter never
   * leaks into another page's view.
   */
  async create(name: string, filterSpec: TaskFilter): Promise<TasksSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: TASKS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Updates an existing Tasks-page saved filter. Throws if `id` does not
   * name a Tasks-page row owned by the operator — a fail-loud guard,
   * never a silent no-op.
   */
  async update(
    id: string,
    patch: { name?: string; filterSpec?: TaskFilter }
  ): Promise<TasksSavedFilter> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== TASKS_SAVED_FILTER_PAGE) {
      throw new Error(`saved_filters_tasks: no tasks-page filter with id ${id}`);
    }
    const row: SavedFilter = {
      ...existing,
      updated_at: Date.now(),
      name: patch.name ?? existing.name,
      filter_spec_json:
        patch.filterSpec !== undefined
          ? JSON.stringify(patch.filterSpec)
          : existing.filter_spec_json
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /** Deletes a Tasks-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== TASKS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
