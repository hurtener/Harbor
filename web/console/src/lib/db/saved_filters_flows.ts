/**
 * Flows-page saved-filter chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 72h). Phase 73i / D-117,
 * refactor onto the design-system foundation (D-121).
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'flows'` discriminator.
 * `saved_filters` carries a `page` column precisely so each list page's
 * saved filters coexist in one table; `saved_filters_flows.ts` is the
 * Flows page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to a `flows.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `FlowFilter` (the
 * Flows-page wire facet shape); the wrapper marshals / unmarshals it.
 *
 * The Flows-page "Save snapshot" affordance persists through this
 * wrapper — it is no longer an in-memory var (the pre-refactor page
 * pinned the snapshot in a `$state` that did not survive a reload).
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { FlowFilter } from '../flows/types.js';

/** The `page` discriminator value Flows-page rows are scoped under. */
export const FLOWS_SAVED_FILTER_PAGE = 'flows' as const;

/**
 * A Flows-page saved filter, decoded. The `filterSpec` is the typed
 * `FlowFilter` the Flows page applies to its `flows.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface FlowsSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: FlowFilter;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `ff-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Flows shape. */
function decode(row: SavedFilter): FlowsSavedFilter {
  let spec: FlowFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as FlowFilter;
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
 * The Flows-page saved-filter store — a typed get/put/list/delete
 * surface over the `saved_filters` table, scoped to `page = 'flows'`.
 * Construct one per (operator, DB) pair; the operator ID is the
 * Console DB row-scope key.
 */
export class FlowsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Flows-page saved filter for the operator, name-sorted. */
  async list(): Promise<FlowsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === FLOWS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Flows-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<FlowsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== FLOWS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Flows-page saved filter. Returns the minted record.
   * The `page` column is fixed to `'flows'` — a Flows-page filter never
   * leaks into another page's view.
   */
  async create(name: string, filterSpec: FlowFilter): Promise<FlowsSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: FLOWS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Updates an existing Flows-page saved filter's name and/or filter
   * spec. Throws if `id` does not name a Flows-page row owned by the
   * operator — a fail-loud guard, never a silent no-op.
   */
  async update(
    id: string,
    patch: { name?: string; filterSpec?: FlowFilter }
  ): Promise<FlowsSavedFilter> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== FLOWS_SAVED_FILTER_PAGE) {
      throw new Error(`saved_filters_flows: no flows-page filter with id ${id}`);
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

  /** Deletes a Flows-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== FLOWS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
