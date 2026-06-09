// Harbor Console — Flows catalog (list) reactive state controller
// (Phase 108p / D-188). Svelte 5 runes mode (D-092).
//
// This module owns the Flows catalog page's reactive state; the `.svelte` view
// reads it and calls its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor — the controller + the pure
// `derive.ts` projections + the focused `FlowsTable` component replace the
// inline page logic, mirroring the Artifacts-108o structure.
//
// The page composes the shipped read surface (`flows.list`, `flows.metrics`)
// plus the one mutating action (`flows.run`, admin-gated — D-079). All six
// `flows.*` methods route through the one typed `FlowsProtocol` wrapper over the
// shared `HarborClient` transport choke point (no hand-rolled fetch — §13).
//
// Fields an async load assigns AFTER first render are `$state` so the reactive
// re-read fires (the Events-page D-180 lesson).

import {
  resolveConnection,
  hasScope,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient } from '$lib/protocol/harbor.js';
import { FlowsProtocol } from '$lib/protocol/flows.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import { openFlowsSavedViewStore } from '$lib/flows/saved_views.js';
import type { FlowsSavedFilters } from '$lib/db/saved_filters_flows.js';
import { toPageError, displayStatus, type PageError } from '$lib/flows/derive.js';
import type { Flow, FlowFilter, FlowMetrics } from '$lib/flows/types.js';

export class FlowsListState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** True when the connection carries the `admin` claim (D-079) — gates the
   * `Run flow` mutation (disabled-with-tooltip without it). */
  canRun = $state(false);
  disconnected = $derived(this.connection === null);

  /* ---- catalog async state (four-state contract) ----------------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);
  flows = $state<Flow[]>([]);
  totalRows = $state(0);

  /* ---- pagination + filters -------------------------------------- */
  page = $state(1);
  pageSize = $state(50);
  query = $state('');

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedViewStore = $state<FlowsSavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  activeViewId = $state<string | null>(null);

  /* ---- metrics rail (nested PageState) --------------------------- */
  selectedId = $state<string | null>(null);
  metrics = $state<FlowMetrics | null>(null);
  railStatus = $state<PageStatus>('empty');
  railError = $state<PageError | null>(null);

  /* ---- run-flow modal (flows.run — D-079) ------------------------ */
  runFlowId = $state<string | null>(null);
  runPending = $state(false);
  runError = $state<string | null>(null);

  #flows: FlowsProtocol | null = null;

  /** The display status — ready/empty derived live from the row count (D-180). */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.flows.length);
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  load(injectedFlows?: FlowsProtocol, injectedStore?: FlowsSavedFilters): void {
    const connection = resolveConnection();
    this.connection = connection;
    this.canRun = hasScope(connection, 'admin');
    if (injectedFlows !== undefined) {
      this.#flows = injectedFlows;
    } else if (connection !== null) {
      this.#flows = new FlowsProtocol(new HarborClient({ connection }));
    } else {
      this.#flows = null;
    }

    void (async () => {
      if (injectedStore !== undefined) {
        this.savedViewStore = injectedStore;
      } else {
        try {
          this.savedViewStore = await openFlowsSavedViewStore();
        } catch {
          this.savedViewStore = null;
        }
      }
      await this.refreshSavedViews();
      await this.loadCatalog();
    })();
  }

  /** The `FlowFilter` assembled from the live filter controls. */
  #filter(): FlowFilter {
    const f: FlowFilter = {};
    const q = this.query.trim();
    if (q.length > 0) f.query = q;
    return f;
  }

  /* ================================================================ */
  /* Catalog (flows.list)                                              */
  /* ================================================================ */

  async loadCatalog(): Promise<void> {
    if (this.#flows === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    try {
      const resp = await this.#flows.list({
        filter: this.#filter(),
        page: this.page,
        page_size: this.pageSize
      });
      this.flows = resp.flows ?? [];
      this.totalRows = resp.total_rows ?? this.flows.length;
      this.status = 'ready';
    } catch (e) {
      this.flows = [];
      this.totalRows = 0;
      this.pageError = toPageError(e);
      this.status = 'error';
    }
  }

  refresh(): void {
    void this.loadCatalog();
  }

  applySearch(): void {
    this.activeViewId = null;
    this.page = 1;
    void this.loadCatalog();
  }

  clearFilters(): void {
    this.query = '';
    this.activeViewId = null;
    this.page = 1;
    void this.loadCatalog();
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

  /* ================================================================ */
  /* Metrics rail (flows.metrics)                                      */
  /* ================================================================ */

  async selectFlow(id: string): Promise<void> {
    this.selectedId = id;
    if (this.#flows === null) {
      this.railStatus = 'disconnected';
      return;
    }
    this.railStatus = 'loading';
    this.railError = null;
    try {
      this.metrics = await this.#flows.metrics({ flow_id: id });
      this.railStatus = 'ready';
    } catch (e) {
      this.metrics = null;
      this.railError = toPageError(e);
      this.railStatus = 'error';
    }
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async refreshSavedViews(): Promise<void> {
    if (this.savedViewStore === null) {
      this.savedViews = [];
      return;
    }
    const stored = await this.savedViewStore.list();
    this.savedViews = stored.map((s) => ({ id: s.id, name: s.name }));
  }

  async applySavedView(id: string): Promise<void> {
    if (this.savedViewStore === null) return;
    const view = await this.savedViewStore.get(id);
    if (view === null) return;
    this.activeViewId = id;
    this.query = view.filterSpec.query ?? '';
    this.page = 1;
    await this.loadCatalog();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedViewStore === null) return;
    await this.savedViewStore.delete(id);
    if (this.activeViewId === id) this.activeViewId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentView(): Promise<void> {
    if (this.savedViewStore === null) return;
    const label = (this.query.trim() || 'All flows').slice(0, 60);
    const created = await this.savedViewStore.create(label, this.#filter());
    this.activeViewId = created.id;
    await this.refreshSavedViews();
  }

  /* ================================================================ */
  /* Run flow (flows.run — the one mutating action, §13 / D-079)       */
  /* ================================================================ */

  openRun(id: string): void {
    this.runFlowId = id;
    this.runError = null;
  }

  cancelRun(): void {
    this.runFlowId = null;
    this.runError = null;
  }

  /** Submits a real `flows.run` invocation from the runner modal. No-op
   * without the admin claim (the UI gates it; the runtime is authoritative). */
  async submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (this.#flows === null || this.runFlowId === null) return;
    this.runPending = true;
    this.runError = null;
    try {
      await this.#flows.run({ flow_id: this.runFlowId, inputs });
      this.runFlowId = null;
    } catch (e) {
      const err = toPageError(e);
      this.runError = `${err.code}: ${err.message}`;
    } finally {
      this.runPending = false;
    }
  }
}
