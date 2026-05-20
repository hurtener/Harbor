/**
 * Memory-page saved-filter chips — a TYPED WRAPPER over the Phase 72h
 * `saved_filters` Console DB table (Phase 73j / D-118).
 *
 * This module adds NO new table (CLAUDE.md §13 / D-061 — the Console DB
 * is Console-local state only, never a shadow of a runtime entity). It
 * is a thin typed facade over the existing `saved_filters` table, which
 * already carries a `page` discriminator column; this wrapper scopes
 * every read / write to `page = "memory"` and gives callers a
 * memory-page-shaped filter type instead of the generic
 * `filter_spec_json` blob.
 *
 * The saved filter SPEC is a `MemoryFilter` (the same shape the
 * `memory.list` Protocol request takes) — the chip persists the filter
 * the operator applied; applying it re-issues a `memory.list` call.
 * The filter is NEVER a server-side saved query; it lives Console-side
 * only (D-061).
 */
import type { ConsoleDB, SavedFilter } from './index.js';
import type { MemoryFilter } from '../protocol/memory-types.js';

/** The `saved_filters.page` discriminator value for the Memory page. */
export const MEMORY_PAGE: SavedFilter['page'] = 'memory';

/**
 * A memory-page saved filter — the typed view over a `saved_filters`
 * row whose `page` is `"memory"`. `filter` is the decoded
 * `MemoryFilter`; the underlying row stores it as `filter_spec_json`.
 */
export interface MemorySavedFilter {
  id: string;
  name: string;
  filter: MemoryFilter;
}

/** Unix-epoch-millis clock; injectable for deterministic tests. */
type Clock = () => number;

/**
 * Decodes a `saved_filters` row into a {@link MemorySavedFilter}. A row
 * whose `filter_spec_json` is malformed throws — never a silently
 * dropped chip (CLAUDE.md §13).
 */
function decodeRow(row: SavedFilter): MemorySavedFilter {
  let filter: MemoryFilter;
  try {
    filter = JSON.parse(row.filter_spec_json) as MemoryFilter;
  } catch (e) {
    throw new Error(
      `saved_filters_memory: row ${row.id} has malformed filter_spec_json: ${String(e)}`
    );
  }
  return { id: row.id, name: row.name, filter };
}

/**
 * MemorySavedFilters is the typed get/put/list/delete facade over the
 * `saved_filters` table for the Memory page. Every method scopes to
 * `page = "memory"`; cross-page rows are never returned or mutated.
 */
export class MemorySavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;
  readonly #now: Clock;

  constructor(db: ConsoleDB, operatorID: string, now: Clock = () => Date.now()) {
    this.#db = db;
    this.#operatorID = operatorID;
    this.#now = now;
  }

  /** Lists every memory-page saved filter for the operator. */
  async list(): Promise<MemorySavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows.filter((r) => r.page === MEMORY_PAGE).map(decodeRow);
  }

  /** Returns one memory-page saved filter, or null when absent / off-page. */
  async get(id: string): Promise<MemorySavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== MEMORY_PAGE) return null;
    return decodeRow(row);
  }

  /** Inserts or replaces a memory-page saved filter. */
  async put(saved: MemorySavedFilter): Promise<void> {
    const ts = this.#now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: saved.id,
      created_at: ts,
      updated_at: ts,
      page: MEMORY_PAGE,
      name: saved.name,
      filter_spec_json: JSON.stringify(saved.filter)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
  }

  /** Deletes a memory-page saved filter (no-op when absent). */
  async delete(id: string): Promise<void> {
    // Guard: only delete when the row is genuinely a memory-page row,
    // so a Memory-page caller cannot delete another page's chip by id.
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== MEMORY_PAGE) return;
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
