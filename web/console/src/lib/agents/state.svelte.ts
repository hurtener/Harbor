// Harbor Console — Agents list page reactive state controller
// (Phase 108l / D-184). Svelte 5 runes mode (D-092).
//
// This module owns the Agents LIST page's reactive state; the `.svelte` view
// reads it and calls its actions, never touching the Protocol client directly
// (CONVENTIONS.md §6). It is the king-file refactor — the controller + the
// pure `derive.ts` projections replace the ~503-line monolith, mirroring the
// Tools-108k / Background-Jobs-108j structure.
//
// The page is the registered-agent catalog browser. It composes the
// already-shipped read methods (Phase 73e / D-124): `agents.list` (the
// faceted catalog page) + `agents.metrics` (the hero rollup), plus the
// Console-DB-backed saved filters (D-061). V1 is INSPECTOR-ONLY for authoring
// (page-agents.md §10) — the five fleet-control verbs live on the detail
// route. No new Protocol method here (a pure consumer; §13).
//
// Fields an async load assigns AFTER first render (the list response, the
// metrics) are `$state` so the reactive re-read fires — the Events-page D-180
// lesson: a plain field leaves the UI bound to the initial null.

import {
  resolveConnection,
  type RuntimeConnection
} from '$lib/connection.js';
import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import type { SavedView } from '$lib/components/ui/SavedViewChips.svelte';
import { openListPageDB } from '$lib/db/console_db.js';
import { AgentsSavedFilters } from '$lib/db/saved_filters_agents.js';
import { operatorIdOf } from '$lib/db/schema.js';
import { toPageError, displayStatus, type PageError } from '$lib/agents/derive.js';
import type {
  Agent,
  AgentFilter,
  AgentListResponse,
  AgentMetrics,
  AgentStatus
} from '$lib/protocol/agents.js';

export class AgentsListPageState {
  /* ---- connection + client (CONVENTIONS.md §6) ------------------- */
  connection = $state<RuntimeConnection | null>(null);
  /** Phase 83r disconnected predicate — drives the disabled-with-tooltip. */
  disconnected = $derived(this.connection === null);

  /* ---- page-level async state (the four-state contract) ---------- */
  status = $state<PageStatus>('loading');
  pageError = $state<PageError | null>(null);
  listResp = $state<AgentListResponse | null>(null);
  metrics = $state<AgentMetrics | null>(null);

  /* ---- pagination ------------------------------------------------ */
  page = $state(1);
  pageSize = $state(50);

  /* ---- filters --------------------------------------------------- */
  searchText = $state('');
  statusFacet = $state('');
  plannerFacet = $state('');

  /* ---- saved views (Console-DB-backed, D-061) -------------------- */
  savedFilters = $state<AgentsSavedFilters | null>(null);
  savedViews = $state<SavedView[]>([]);
  savedFilterSpecs = $state<Map<string, AgentFilter>>(new Map());
  activeSavedId = $state<string | null>(null);
  saveName = $state('');

  #client: ProtocolClient | null = null;

  /* ================================================================ */
  /* Derived projections                                               */
  /* ================================================================ */

  /** The catalog rows from the last `agents.list` page. */
  get agents(): Agent[] {
    return this.listResp?.agents ?? [];
  }

  /** The filtered-view total (for pagination). */
  get totalRows(): number {
    return this.listResp?.total_rows ?? 0;
  }

  /**
   * The status the `<PageState>` boundary renders — derived LIVE from the
   * loaded-row count so a catalog whose rows all dropped under a tight facet
   * still reads "empty" (D-180; see `derive.displayStatus`).
   */
  get displayStatus(): PageStatus {
    return displayStatus(this.status, this.agents.length);
  }

  /** Assembles the `AgentFilter` from the live filter controls. */
  currentFilter(): AgentFilter {
    const f: AgentFilter = {};
    if (this.searchText) f.search = this.searchText;
    if (this.statusFacet) f.status = [this.statusFacet as AgentStatus];
    if (this.plannerFacet) f.planner_type = [this.plannerFacet];
    return f;
  }

  /* ================================================================ */
  /* Boot + loading                                                    */
  /* ================================================================ */

  /**
   * Resolves the connection + Console-DB saved filters, then loads page 1.
   * `injected` is an optional in-page client the harness supplies.
   */
  load(injected?: ProtocolClient): void {
    const connection = resolveConnection();
    this.connection = connection;

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
        this.savedFilters = new AgentsSavedFilters(db, operator);
        await this.refreshSavedViews();
      } catch {
        this.savedFilters = null;
      }
    })();

    void this.loadAgents();
  }

  /** Symmetry with the other controllers — the page calls it on unmount. */
  close(): void {
    /* no long-lived subscription on the list page (the agent catalog is
       slow-moving, polled on demand — there is no SSE here). */
  }

  /**
   * Loads the agent catalog + the metrics rollup for the current filter /
   * page. The single re-invocation target the Error state's Retry calls.
   */
  async loadAgents(): Promise<void> {
    if (this.#client === null) {
      this.status = 'disconnected';
      return;
    }
    this.status = 'loading';
    this.pageError = null;
    try {
      const [list, metricsResp] = await Promise.all([
        this.#client.agents.list<AgentListResponse>({
          filter: this.currentFilter(),
          page: this.page,
          page_size: this.pageSize
        }),
        this.#client.agents.metrics<{ metrics: AgentMetrics }>()
      ]);
      this.listResp = list;
      this.metrics = metricsResp.metrics;
      // ready/empty is derived live via `displayStatus`; set a non-terminal
      // status here so it flips out of `loading`.
      this.status = 'ready';
    } catch (err) {
      // The Error state suppresses any stale view — drop last-good data.
      this.listResp = null;
      this.metrics = null;
      this.pageError = toPageError(err);
      this.status = 'error';
    }
  }

  /** The Refresh button re-fetches the current page. */
  refresh(): void {
    void this.loadAgents();
  }

  /* ================================================================ */
  /* Filtering + pagination                                            */
  /* ================================================================ */

  setSearch(term: string): void {
    this.searchText = term;
  }

  submitSearch(): void {
    this.page = 1;
    this.activeSavedId = null;
    void this.loadAgents();
  }

  applyStatusFacet(value: string): void {
    this.statusFacet = value;
    this.page = 1;
    this.activeSavedId = null;
    void this.loadAgents();
  }

  applyPlannerFacet(value: string): void {
    this.plannerFacet = value;
    this.page = 1;
    this.activeSavedId = null;
    void this.loadAgents();
  }

  clearFilters(): void {
    this.searchText = '';
    this.statusFacet = '';
    this.plannerFacet = '';
    this.activeSavedId = null;
    this.page = 1;
    void this.loadAgents();
  }

  changePage(next: number): void {
    this.page = next;
    void this.loadAgents();
  }

  changePageSize(size: number): void {
    this.pageSize = size;
    this.page = 1;
    void this.loadAgents();
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
    this.activeSavedId = id;
    this.searchText = spec.search ?? '';
    this.statusFacet = spec.status?.[0] ?? '';
    this.plannerFacet = spec.planner_type?.[0] ?? '';
    this.page = 1;
    void this.loadAgents();
  }

  async deleteSavedView(id: string): Promise<void> {
    if (this.savedFilters === null) return;
    await this.savedFilters.delete(id);
    if (this.activeSavedId === id) this.activeSavedId = null;
    await this.refreshSavedViews();
  }

  async saveCurrentView(): Promise<void> {
    const name = this.saveName.trim();
    if (name.length === 0 || this.savedFilters === null) return;
    const created = await this.savedFilters.create(name, this.currentFilter());
    this.saveName = '';
    await this.refreshSavedViews();
    this.activeSavedId = created.id;
  }
}
