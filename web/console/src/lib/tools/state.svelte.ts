// Harbor Console — Tools page reactive state controller
// (Phase 108k / D-183). Svelte 5 runes mode (D-092).
//
// This module owns the Tools page's reactive state; the `.svelte` view
// reads it and calls its actions, never touching the Protocol client
// directly (CONVENTIONS.md §6). It is the king-file refactor (the page
// was an ~847-line monolith) — the controller + the pure `derive.ts`
// projections + the focused `ToolCatalogTable` / `ToolDetailRail`
// components replace it, mirroring the Background-Jobs-108j structure.
//
// The page is the registered-tool-catalog browser. It composes ONLY the
// already-shipped seven `tools.*` methods (Phase 73f / D-116):
//   - `tools.list` — the faceted catalog page + the view aggregates.
//   - `tools.describe` — the full manifest for the selected tool.
//   - `tools.metrics` — per-tool error-rate gauges + status pill.
//   - `tools.content_stats` — per-tool result-size histogram.
//   - `tools.set_approval_policy` — the ADMIN approval-gate write (D-079).
//   - `tools.revoke_oauth` — the ADMIN OAuth-revoke write (D-079).
// plus the Console-DB-backed saved filters (D-061). No new Protocol
// method (a pure consumer; §13). `tools.invoke` is NOT shipped at V1
// (D-132), so there is no "try this tool" call here.
//
// Fields an async load assigns AFTER first render (the list response, the
// manifest, the metrics, the content stats) are `$state` so the reactive
// re-read fires — the Events-page D-180 lesson: a plain field leaves the
// UI bound to the initial null and never re-renders when data streams in.

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import { openListPageDB } from '$lib/db/console_db.js';
import { ToolsSavedFilters } from '$lib/db/saved_filters_tools.js';
import { operatorIdOf } from '$lib/db/schema.js';
import { exportToolsCSV, exportToolsJSON, triggerDownload } from '$lib/tools/export.js';
import { toPageError, displayStatus, type PageError } from '$lib/tools/derive.js';
import type {
  Tool,
  ToolFilter,
  ToolManifest,
  ToolMetrics,
  ToolContentStats,
  ToolListResponse,
  ToolMetricsWindow,
  ToolAggregates,
  ToolApprovalPolicy
} from '$lib/protocol/tools.js';

/** The empty aggregate counters — the disconnected / pre-load default. */
const EMPTY_AGGREGATES: ToolAggregates = {
  total: 0,
  active: 0,
  pending_approval: 0,
  awaiting_oauth: 0
};

export class ToolsPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the elevated `admin` claim (D-079). */
  canAdmin = $state(false);
  /** Phase 83r disconnected predicate — drives the disabled-with-tooltip. */
  disconnected = $derived(this.connection === null);

  /* ---- page-level async state (the four-state contract) ---------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);

  /* ---- catalog list state ---------------------------------------- */
  filter = $state<ToolFilter>({});
  page = $state(1);
  pageSize = $state(50);
  listResp = $state<ToolListResponse | null>(null);
  searchText = $state('');

  /* ---- selection + bulk actions ---------------------------------- */
  selected = $state<Set<string>>(new Set<string>());
  bulkPending = $state(false);
  bulkResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- detail (the right-rail) ----------------------------------- */
  selectedId = $state<string | null>(null);
  detailStatus = $state<PageStatus>('empty');
  detailError = $state<PageError | null>(null);
  manifest = $state<ToolManifest | null>(null);
  metrics = $state<ToolMetrics | null>(null);
  contentStats = $state<ToolContentStats | null>(null);
  metricsWindow = $state<ToolMetricsWindow>('1h');

  /* ---- approval (real Protocol calls — the §13 contract) --------- */
  approvalPending = $state(false);
  approvalResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedFilters = $state<ToolsSavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  savedFilterSpecs = $state<Map<string, ToolFilter>>(new Map());
  activeSavedId = $state<string | null>(null);
  saveName = $state('');

  #client: ProtocolClient | null = null;

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  /** The catalog rows from the last `tools.list` page. */
  get tools(): Tool[] {
    return this.listResp?.tools ?? [];
  }

  /** The four view-level aggregate counters (the idle-rail overview). */
  get aggregates(): ToolAggregates {
    return this.listResp?.aggregates ?? EMPTY_AGGREGATES;
  }

  /** The filtered-view total (for pagination). */
  get totalRows(): number {
    return this.listResp?.total_rows ?? 0;
  }

  /** The currently-open tool row (from the loaded page), or null. */
  get selectedTool(): Tool | null {
    return this.tools.find((t) => t.id === this.selectedId) ?? null;
  }

  /**
   * The status the `<PageState>` boundary renders — derived LIVE from the
   * loaded-row count so a catalog whose rows all dropped under a tight
   * facet still reads "empty" (D-180; see `derive.displayStatus`).
   */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.tools.length);
  }

  /* ================================================================ */
  /* Boot + loading                                                    */
  /* ================================================================ */

  /**
   * Resolves the connection + Console-DB saved filters, then loads page 1.
   * `injected` is an optional in-page client the harness supplies (read
   * inside the page's onMount closure — never captured at construction).
   */
  load(injected?: ProtocolClient): void {
    const connection = resolveConnection();
    this.connection = connection;
    // The single sanctioned read of the persisted scope set (D-132 / F5) —
    // a `.svelte`/controller never reads `localStorage` directly.
    this.canAdmin = hasScope(connection, 'admin');

    if (connection === null) {
      // Disconnected — NOT an error (CONVENTIONS.md §4 state 1).
      this.#client = null;
      this.status = 'disconnected';
      return;
    }
    this.#client = injected ?? new HarborClient({ connection });

    // Wire the Console-DB-backed saved-view store (D-061). Best-effort: a
    // failure leaves the chips empty but the page works.
    void (async () => {
      try {
        const db = await openListPageDB(connection);
        const operator = await operatorIdOf(connection.identity.tenant, connection.identity.user);
        this.savedFilters = new ToolsSavedFilters(db, operator);
        await this.refreshSavedViews();
      } catch {
        this.savedFilters = null;
      }
    })();

    void this.loadCatalog();
  }

  /** Symmetry with the BG controller — the page calls it on unmount. */
  close(): void {
    /* no long-lived subscription to tear down (the Tools catalog is
       slow-moving, polled on demand — there is no run-scoped SSE here). */
  }

  async loadCatalog(): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    try {
      const resp = await this.#client.tools.list<ToolListResponse>(
        this.filter as Record<string, unknown>,
        this.page,
        this.pageSize
      );
      this.listResp = resp;
      // ready/empty is derived live via `displayStatus`; set a
      // non-terminal status here so it flips out of `loading`.
      this.status = 'ready';
    } catch (err) {
      // The Error state suppresses any stale table — drop last-good data.
      this.listResp = null;
      this.pageError = toPageError(err);
      this.status = 'error';
    }
  }

  /** The Refresh button + the post-write reload re-fetch the current page. */
  refresh(): void {
    void this.loadCatalog();
  }

  async #loadDetail(id: string): Promise<void> {
    if (this.#client === null) return;
    this.detailStatus = 'loading';
    this.detailError = null;
    try {
      const [m, met, cs] = await Promise.all([
        this.#client.tools.describe<ToolManifest>(id),
        this.#client.tools.metrics<ToolMetrics>(id, this.metricsWindow),
        this.#client.tools.contentStats<ToolContentStats>(id)
      ]);
      this.manifest = m;
      this.metrics = met;
      this.contentStats = cs;
      this.detailStatus = 'ready';
    } catch (err) {
      this.manifest = null;
      this.metrics = null;
      this.contentStats = null;
      this.detailError = toPageError(err);
      this.detailStatus = 'error';
    }
  }

  /* ================================================================ */
  /* Selection + detail (the right-rail)                               */
  /* ================================================================ */

  /** Opens the per-tool rail: `tools.describe` + `metrics` + `content_stats`. */
  async selectTool(id: string): Promise<void> {
    this.selectedId = id;
    this.approvalResult = null;
    await this.#loadDetail(id);
  }

  /** Closes the rail back to the idle catalog-overview hint. */
  clearSelection(): void {
    this.selectedId = null;
    this.detailStatus = 'empty';
    this.detailError = null;
    this.manifest = null;
    this.metrics = null;
    this.contentStats = null;
    this.approvalResult = null;
  }

  /** The metrics-window toggle re-fetches just the per-tool metrics. */
  async changeMetricsWindow(w: ToolMetricsWindow): Promise<void> {
    this.metricsWindow = w;
    if (this.selectedId !== null && this.#client !== null) {
      try {
        this.metrics = await this.#client.tools.metrics<ToolMetrics>(this.selectedId, w);
      } catch (err) {
        this.detailError = toPageError(err);
        this.detailStatus = 'error';
      }
    }
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  applyFilter(next: ToolFilter): void {
    this.filter = next;
    this.page = 1;
    this.activeSavedId = null;
    void this.loadCatalog();
  }

  submitSearch(): void {
    this.applyFilter({ ...this.filter, search: this.searchText.trim() });
  }

  setSearch(term: string): void {
    this.searchText = term;
  }

  clearFilters(): void {
    this.searchText = '';
    this.activeSavedId = null;
    this.applyFilter({});
  }

  changePage(next: number): void {
    this.page = next;
    void this.loadCatalog();
  }

  changePageSize(size: number): void {
    this.pageSize = size;
    this.page = 1;
    void this.loadCatalog();
  }

  setSelection(next: Set<string>): void {
    this.selected = next;
  }

  /* ================================================================ */
  /* Export (Console-local — no Protocol mutation, D-061)              */
  /* ================================================================ */

  handleExport(format: 'csv' | 'json'): void {
    if (format === 'csv') {
      triggerDownload('tools.csv', 'text/csv', exportToolsCSV(this.tools));
    } else {
      triggerDownload('tools.json', 'application/json', exportToolsJSON(this.tools));
    }
  }

  bulkExport(): void {
    const rows = this.tools.filter((t) => this.selected.has(t.id));
    triggerDownload('tools-selection.csv', 'text/csv', exportToolsCSV(rows));
  }

  /* ================================================================ */
  /* Admin writes — the SHIPPED Phase 73f surface (§13 / D-079)        */
  /* ================================================================ */

  /**
   * Approve / Reject call the REAL `tools.set_approval_policy` admin
   * method (Approve → `auto`, Reject → `denied`). No optimistic fiction —
   * the catalog + detail re-load so the rendered policy reflects the
   * Runtime's new state (CLAUDE.md §13). Gated on `canAdmin`.
   */
  async setApprovalPolicy(toolID: string, policy: ToolApprovalPolicy): Promise<void> {
    if (this.#client === null || !this.canAdmin) return;
    this.approvalPending = true;
    this.approvalResult = null;
    try {
      await this.#client.tools.setApprovalPolicy(toolID, policy);
      this.approvalResult = {
        ok: true,
        message: `Approval policy for ${toolID} set to "${policy}".`
      };
      await this.loadCatalog();
      if (this.selectedId === toolID) {
        await this.#loadDetail(toolID);
      }
    } catch (err) {
      const e = toPageError(err);
      this.approvalResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      this.approvalPending = false;
    }
  }

  /** Bulk: revoke OAuth bindings on the selected rows (one call per row). */
  async bulkRevokeOAuth(): Promise<void> {
    if (this.#client === null || !this.canAdmin || this.selected.size === 0) return;
    this.bulkPending = true;
    this.bulkResult = null;
    const ids = [...this.selected];
    let ok = 0;
    let failed = 0;
    for (const id of ids) {
      try {
        await this.#client.tools.revokeOAuth(id);
        ok += 1;
      } catch {
        failed += 1;
      }
    }
    this.bulkResult = {
      ok: failed === 0,
      message: `Revoke OAuth: ${ok} succeeded, ${failed} failed (of ${ids.length}).`
    };
    this.selected = new Set<string>();
    this.bulkPending = false;
    await this.loadCatalog();
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async refreshSavedViews(): Promise<void> {
    if (this.savedFilters === null) return;
    try {
      const records = await this.savedFilters.list();
      this.savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      this.savedFilterSpecs = new Map(records.map((r) => [r.id, r.filterSpec]));
    } catch {
      this.savedViews = [];
      this.savedFilterSpecs = new Map();
    }
  }

  applySavedView(id: string): void {
    const spec = this.savedFilterSpecs.get(id);
    if (spec === undefined) return;
    this.filter = { ...spec };
    this.searchText = spec.search ?? '';
    this.page = 1;
    this.activeSavedId = id;
    void this.loadCatalog();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedFilters === null) return;
    await this.savedFilters.delete(id);
    if (this.activeSavedId === id) this.activeSavedId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentFilter(): Promise<void> {
    const name = this.saveName.trim();
    if (name.length === 0 || this.savedFilters === null) return;
    const created = await this.savedFilters.create(name, { ...this.filter });
    this.saveName = '';
    await this.refreshSavedViews();
    this.activeSavedId = created.id;
  }
}
