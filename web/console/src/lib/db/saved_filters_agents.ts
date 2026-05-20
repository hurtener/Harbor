/**
 * Agents-page saved-filter chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table (Phase 72h). Phase 73e / D-124.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'agents'` discriminator.
 * `saved_filters` carries a `page` column precisely so each list page's
 * saved filters coexist in one table; `saved_filters_agents.ts` is the
 * Agents page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to an `agents.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `AgentFilter` (the
 * Phase 73e wire facet shape); the wrapper marshals / unmarshals it.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { AgentFilter } from '../protocol/agents.js';

/** The `page` discriminator value Agents-page rows are scoped under. */
export const AGENTS_SAVED_FILTER_PAGE = 'agents' as const;

/**
 * An Agents-page saved filter, decoded. The `filterSpec` is the typed
 * `AgentFilter` the Agents page applies to its `agents.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface AgentsSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: AgentFilter;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `af-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Agents shape. */
function decode(row: SavedFilter): AgentsSavedFilter {
  let spec: AgentFilter = {};
  try {
    spec = JSON.parse(row.filter_spec_json) as AgentFilter;
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
 * The Agents-page saved-filter store — a typed list/get/create/delete
 * surface over the `saved_filters` table, scoped to `page = 'agents'`.
 * Construct one per (operator, DB) pair; the operator ID is the
 * Console DB row-scope key.
 */
export class AgentsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Agents-page saved filter for the operator, name-sorted. */
  async list(): Promise<AgentsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === AGENTS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Agents-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<AgentsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== AGENTS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Agents-page saved filter. Returns the minted record.
   * The `page` column is fixed to `'agents'` — an Agents-page filter
   * never leaks into another page's view.
   */
  async create(name: string, filterSpec: AgentFilter): Promise<AgentsSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: AGENTS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /** Deletes an Agents-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== AGENTS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
