/**
 * Live Runtime page saved-view chips — a TYPED WRAPPER over the existing
 * `saved_filters` Console DB table. Phase 73b / D-126.
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'live_runtime'`
 * discriminator. `saved_filters` carries a `page` column precisely so
 * each page's saved presets coexist in one table.
 *
 * # Console-local only (D-061)
 *
 * A Live Runtime saved view is a Console-local convenience — a named
 * preset of the topology/timeline tab + the trace-toggle state the
 * operator returns to. It is NEVER a runtime entity: the Runtime does
 * not know these presets exist (D-061 forbids a Console DB shadowing
 * runtime entities). The `filter_spec_json` column stores a JSON-encoded
 * {@link LiveRuntimeViewSpec}.
 */

import type { ConsoleDB, SavedFilter } from './index.js';

/** The `page` discriminator value Live Runtime rows are scoped under. */
export const LIVE_RUNTIME_SAVED_FILTER_PAGE = 'live_runtime' as const;

/** The closed set of the page's main canvas tabs. */
export type LiveRuntimeTab = 'topology' | 'timeline' | 'metrics' | 'health';

/**
 * A Live Runtime saved-view spec — Console-local UI state the operator
 * names and returns to. It carries the active main-canvas tab and
 * whether the bottom-dock trace overlay is on. NOT a runtime query.
 */
export interface LiveRuntimeViewSpec {
  /** The main-canvas tab the preset selects. */
  tab: LiveRuntimeTab;
  /** Whether the trace overlay (per-run event correlation) is on. */
  traceOn: boolean;
}

/** A Live Runtime saved view, decoded. */
export interface LiveRuntimeSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded view preset. */
  viewSpec: LiveRuntimeViewSpec;
  /** Unix epoch millis. */
  createdAt: number;
  /** Unix epoch millis. */
  updatedAt: number;
}

/** The default view spec when a stored preset is missing / corrupt. */
const DEFAULT_VIEW_SPEC: LiveRuntimeViewSpec = { tab: 'topology', traceOn: false };

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `lrv-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/** Decodes a `saved_filters` row into the typed Live Runtime shape. */
function decode(row: SavedFilter): LiveRuntimeSavedFilter {
  let spec: LiveRuntimeViewSpec = { ...DEFAULT_VIEW_SPEC };
  try {
    const parsed = JSON.parse(row.filter_spec_json) as Partial<LiveRuntimeViewSpec>;
    spec = {
      tab: parsed.tab ?? DEFAULT_VIEW_SPEC.tab,
      traceOn: parsed.traceOn ?? DEFAULT_VIEW_SPEC.traceOn
    };
  } catch {
    // A corrupt spec degrades to the default — the chip still renders;
    // the corruption is Console-local and self-healing on the next save.
    spec = { ...DEFAULT_VIEW_SPEC };
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
 * The Live Runtime saved-view store — a typed get/list/create/delete
 * surface over the `saved_filters` table, scoped to
 * `page = 'live_runtime'`. Construct one per (operator, DB) pair.
 */
export class LiveRuntimeSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;

  constructor(db: ConsoleDB, operatorID: string) {
    this.#db = db;
    this.#operatorID = operatorID;
  }

  /** Lists every Live Runtime saved view for the operator, name-sorted. */
  async list(): Promise<LiveRuntimeSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === LIVE_RUNTIME_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Live Runtime saved view by id, or `null` if absent. */
  async get(id: string): Promise<LiveRuntimeSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== LIVE_RUNTIME_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /** Creates a new Live Runtime saved view. Returns the minted record. */
  async create(name: string, viewSpec: LiveRuntimeViewSpec): Promise<LiveRuntimeSavedFilter> {
    const now = Date.now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: now,
      updated_at: now,
      page: LIVE_RUNTIME_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(viewSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /** Deletes a Live Runtime saved view (no-op when absent). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== LIVE_RUNTIME_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
