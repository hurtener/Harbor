/**
 * Tools-page saved-filter chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 72h). Phase 73f / D-116.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'tools'` discriminator.
 * `saved_filters` carries a `page` column precisely so each list page's
 * saved filters coexist in one table; `saved_filters_tools.ts` is the
 * Tools page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to a `tools.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `ToolFilter` (the
 * Phase 73f wire facet shape); the wrapper marshals / unmarshals it.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { ToolFilter } from '../protocol/tools.js';

/** The `page` discriminator value Tools-page rows are scoped under. */
export const TOOLS_SAVED_FILTER_PAGE = 'tools' as const;

/**
 * A Tools-page saved filter, decoded. The `filterSpec` is the typed
 * `ToolFilter` the Tools page applies to its `tools.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface ToolsSavedFilter {
  /** Table-local primary key (ULID). */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: ToolFilter;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `tf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Tools shape. */
function decode(row: SavedFilter): ToolsSavedFilter {
  let spec: ToolFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as ToolFilter;
  } catch {
    // A corrupt spec degrades to "no filter" rather than throwing — the
    // chip still renders; applying it just yields the full catalog. The
    // corruption is Console-local and self-healing on the next save.
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
 * The Tools-page saved-filter store — a typed get/put/list/delete
 * surface over the `saved_filters` table, scoped to `page = 'tools'`.
 * Construct one per (operator, DB) pair; the operator ID is the
 * Console DB row-scope key.
 */
export class ToolsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Tools-page saved filter for the operator, name-sorted. */
  async list(): Promise<ToolsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === TOOLS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Tools-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<ToolsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== TOOLS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Tools-page saved filter. Returns the minted record.
   * The `page` column is fixed to `'tools'` — a Tools-page filter never
   * leaks into another page's view.
   */
  async create(name: string, filterSpec: ToolFilter): Promise<ToolsSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: TOOLS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Updates an existing Tools-page saved filter's name and/or filter
   * spec. Throws if `id` does not name a Tools-page row owned by the
   * operator — a fail-loud guard, never a silent no-op.
   */
  async update(
    id: string,
    patch: { name?: string; filterSpec?: ToolFilter }
  ): Promise<ToolsSavedFilter> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== TOOLS_SAVED_FILTER_PAGE) {
      throw new Error(`saved_filters_tools: no tools-page filter with id ${id}`);
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

  /** Deletes a Tools-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== TOOLS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
