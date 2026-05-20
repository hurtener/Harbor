/**
 * Sessions-page saved-filter chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 72h). Phase 73c / D-122.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters`
 * table, scoping every read / write to the `page = 'sessions'`
 * discriminator. `saved_filters` carries a `page` column precisely so
 * each list page's saved filters coexist in one table.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to a `sessions.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist, and
 * NO `sessions.saved_filter.*` Protocol method exists. The
 * `filter_spec_json` column stores a JSON-encoded `SessionFilter` (the
 * Sessions-page wire facet shape); the wrapper marshals / unmarshals
 * it. The wire shape carries only the inflated filter, NEVER a
 * saved-filter ID.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { SessionFilter } from '../sessions/types.js';

/** The `page` discriminator value Sessions-page rows are scoped under. */
export const SESSIONS_SAVED_FILTER_PAGE = 'sessions' as const;

/**
 * A Sessions-page saved filter, decoded. The `filterSpec` is the typed
 * `SessionFilter` the Sessions page applies to its `sessions.list`
 * call; the underlying row stores it JSON-encoded.
 */
export interface SessionsSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: SessionFilter;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `sf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Sessions shape. */
function decode(row: SavedFilter): SessionsSavedFilter {
  let spec: SessionFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as SessionFilter;
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
 * The Sessions-page saved-filter store — a typed get/put/list/delete
 * surface over the `saved_filters` table, scoped to `page = 'sessions'`.
 * Construct one per (operator, DB) pair; the operator ID is the
 * Console DB row-scope key.
 */
export class SessionsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Sessions-page saved filter for the operator, name-sorted. */
  async list(): Promise<SessionsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === SESSIONS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Sessions-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<SessionsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== SESSIONS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Sessions-page saved filter. Returns the minted
   * record. The `page` column is fixed to `'sessions'` — a Sessions-page
   * filter never leaks into another page's view.
   */
  async create(name: string, filterSpec: SessionFilter): Promise<SessionsSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: SESSIONS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Updates an existing Sessions-page saved filter. Throws if `id`
   * does not name a Sessions-page row owned by the operator — a
   * fail-loud guard, never a silent no-op.
   */
  async update(
    id: string,
    patch: { name?: string; filterSpec?: SessionFilter }
  ): Promise<SessionsSavedFilter> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== SESSIONS_SAVED_FILTER_PAGE) {
      throw new Error(`saved_filters_sessions: no sessions-page filter with id ${id}`);
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

  /** Deletes a Sessions-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== SESSIONS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
