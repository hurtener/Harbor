/**
 * MCP-Connections-page saved-filter chips — a TYPED WRAPPER over the
 * Phase 72h `saved_filters` Console DB table (D-121, MCP refactor).
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'mcp_connections'`
 * discriminator. `saved_filters` already carries a `page` column precisely
 * so each list page's saved filters coexist in one table;
 * `saved_filters_mcp.ts` is the MCP Connections page's typed view onto it.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — a named preset the
 * operator *applies* to an `mcp.servers.list` Protocol call. It is NEVER a
 * runtime entity: the Runtime does not know these presets exist. The
 * `filter_spec_json` column stores a JSON-encoded `MCPListFilter` (the MCP
 * wire facet shape); the wrapper marshals / unmarshals it.
 */

import type { ConsoleDB, SavedFilter } from './index.js';
import type { MCPListFilter } from '../protocol/mcp.js';

/** The `saved_filters.page` discriminator value for the MCP Connections page. */
export const MCP_SAVED_FILTER_PAGE: SavedFilter['page'] = 'mcp_connections';

/**
 * An MCP-Connections-page saved filter, decoded. The `filterSpec` is the
 * typed `MCPListFilter` the page applies to its `mcp.servers.list` call;
 * the underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface MCPSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: MCPListFilter;
}

/** Unix-epoch-millis clock; injectable for deterministic tests. */
type Clock = () => number;

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `mcpf-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/**
 * Decodes a `saved_filters` row into a typed {@link MCPSavedFilter}. A
 * malformed `filter_spec_json` throws — never a silently dropped chip
 * (CLAUDE.md §13, fail loudly).
 */
function decode(row: SavedFilter): MCPSavedFilter {
  let spec: MCPListFilter;
  try {
    spec = JSON.parse(row.filter_spec_json) as MCPListFilter;
  } catch (e) {
    throw new Error(
      `saved_filters_mcp: row ${row.id} has malformed filter_spec_json: ${String(e)}`
    );
  }
  return { id: row.id, name: row.name, filterSpec: spec };
}

/**
 * The MCP-Connections-page saved-filter store — a typed
 * list/create/delete surface over the `saved_filters` table, scoped to
 * `page = 'mcp_connections'`. Construct one per (operator, DB) pair; the
 * operator ID is the Console DB row-scope key. Cross-page rows are never
 * returned or mutated.
 */
export class MCPSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;
  readonly #now: Clock;

  constructor(db: ConsoleDB, operatorID: string, now: Clock = () => Date.now()) {
    this.#db = db;
    this.#operatorID = operatorID;
    this.#now = now;
  }

  /** Lists every MCP-page saved filter for the operator, name-sorted. */
  async list(): Promise<MCPSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === MCP_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one MCP-page saved filter by id, or `null` when absent / off-page. */
  async get(id: string): Promise<MCPSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== MCP_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new MCP-page saved filter. Returns the minted record. The
   * `page` column is fixed to `'mcp_connections'` — an MCP-page filter
   * never leaks into another page's view.
   */
  async create(name: string, filterSpec: MCPListFilter): Promise<MCPSavedFilter> {
    const ts = this.#now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: ts,
      updated_at: ts,
      page: MCP_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /**
   * Deletes an MCP-page saved filter. Guarded so an MCP-page caller cannot
   * delete another page's chip by id; a no-op when the row is absent.
   */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== MCP_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
