/**
 * Artifacts-page saved-filter chips — a TYPED WRAPPER over the Phase 72h
 * `saved_filters` Console DB table (D-121, refactor onto the design-system
 * foundation; CONVENTIONS.md §3/§5).
 *
 * # No new table (CLAUDE.md §13 / D-061)
 *
 * This module adds NO table. It wraps the shipped `saved_filters` table,
 * scoping every read / write to the `page = 'artifacts'` discriminator.
 * `saved_filters` carries a `page` column precisely so each list page's
 * saved filters coexist in one table; `saved_filters_artifacts.ts` is the
 * Artifacts page's typed view onto it.
 *
 * The legacy Artifacts page rendered its saved-view chips ("Large > 10 MB",
 * "Stale > 7d", …) as a HARDCODED string array — they neither persisted
 * nor applied. This wrapper makes them Console-DB-backed: a saved view is a
 * named, persisted `ArtifactsFilterSpec` the operator *applies* to an
 * `artifacts.list` Protocol call.
 *
 * # Console-local only (D-061)
 *
 * A saved filter is a Console-local convenience — the Runtime does not know
 * these presets exist. The `filter_spec_json` column stores a JSON-encoded
 * {@link ArtifactsFilterSpec}; the wrapper marshals / unmarshals it.
 */
import type { ConsoleDB, SavedFilter } from './index.js';
import type { ArtifactSource } from '../protocol.js';

/** The `saved_filters.page` discriminator value for the Artifacts page. */
export const ARTIFACTS_SAVED_FILTER_PAGE: SavedFilter['page'] = 'artifacts';

/**
 * The Artifacts-page facet filter a saved view persists. It is the subset
 * of `ArtifactsListRequest` the page exposes as faceted chips — MIME type
 * and producer source. Both are optional (an empty field is a wildcard).
 */
export interface ArtifactsFilterSpec {
  /** Selected MIME type, or empty for any. */
  mimeType?: string;
  /** Selected producer source, or empty for any. */
  source?: ArtifactSource | '';
}

/**
 * An Artifacts-page saved filter, decoded. The `filterSpec` is the typed
 * facet filter the page applies to its `artifacts.list` call; the
 * underlying row stores it JSON-encoded in `filter_spec_json`.
 */
export interface ArtifactsSavedFilter {
  /** Table-local primary key. */
  id: string;
  /** Operator-facing chip label. */
  name: string;
  /** The decoded facet filter this preset applies. */
  filterSpec: ArtifactsFilterSpec;
}

/** Unix-epoch-millis clock; injectable for deterministic tests. */
type Clock = () => number;

/** Mints a sortable-ish unique id without pulling a ULID dependency. */
function mintID(): string {
  const rand = globalThis.crypto.randomUUID().replace(/-/g, '');
  return `af-${Date.now().toString(36)}-${rand.slice(0, 12)}`;
}

/**
 * Decodes a `saved_filters` row into the typed Artifacts shape. A row
 * whose `filter_spec_json` is malformed throws — never a silently dropped
 * chip (CLAUDE.md §13 fail-loudly).
 */
function decode(row: SavedFilter): ArtifactsSavedFilter {
  let spec: ArtifactsFilterSpec;
  try {
    spec = JSON.parse(row.filter_spec_json) as ArtifactsFilterSpec;
  } catch (e) {
    throw new Error(
      `saved_filters_artifacts: row ${row.id} has malformed filter_spec_json: ${String(e)}`
    );
  }
  return { id: row.id, name: row.name, filterSpec: spec };
}

/**
 * The Artifacts-page saved-filter store — a typed get/put/list/delete
 * surface over the `saved_filters` table, scoped to `page = 'artifacts'`.
 * Construct one per (operator, DB) pair; the operator ID is the Console DB
 * row-scope key.
 */
export class ArtifactsSavedFilters {
  readonly #db: ConsoleDB;
  readonly #operatorID: string;
  readonly #now: Clock;

  constructor(db: ConsoleDB, operatorID: string, now: Clock = () => Date.now()) {
    this.#db = db;
    this.#operatorID = operatorID;
    this.#now = now;
  }

  /** Lists every Artifacts-page saved filter for the operator, name-sorted. */
  async list(): Promise<ArtifactsSavedFilter[]> {
    const rows = await this.#db.savedFilters.list(this.#operatorID);
    return rows
      .filter((r) => r.page === ARTIFACTS_SAVED_FILTER_PAGE)
      .map(decode)
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  /** Returns one Artifacts-page saved filter by id, or `null` if absent. */
  async get(id: string): Promise<ArtifactsSavedFilter | null> {
    const row = await this.#db.savedFilters.get(this.#operatorID, id);
    if (row === null || row.page !== ARTIFACTS_SAVED_FILTER_PAGE) {
      return null;
    }
    return decode(row);
  }

  /**
   * Creates a new Artifacts-page saved filter. Returns the minted record.
   * The `page` column is fixed to `'artifacts'` — an Artifacts-page filter
   * never leaks into another page's view.
   */
  async create(name: string, filterSpec: ArtifactsFilterSpec): Promise<ArtifactsSavedFilter> {
    const ts = this.#now();
    const row: SavedFilter = {
      operator_id: this.#operatorID,
      id: mintID(),
      created_at: ts,
      updated_at: ts,
      page: ARTIFACTS_SAVED_FILTER_PAGE,
      name,
      filter_spec_json: JSON.stringify(filterSpec)
    };
    await this.#db.savedFilters.upsert(this.#operatorID, row);
    return decode(row);
  }

  /** Deletes an Artifacts-page saved filter (no-op when absent / off-page). */
  async delete(id: string): Promise<void> {
    const existing = await this.#db.savedFilters.get(this.#operatorID, id);
    if (existing === null || existing.page !== ARTIFACTS_SAVED_FILTER_PAGE) {
      return;
    }
    await this.#db.savedFilters.delete(this.#operatorID, id);
  }
}
