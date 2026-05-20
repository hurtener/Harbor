<script lang="ts">
  // Harbor Console — Agents page (`/agents`), list mode. Phase 73e /
  // D-124, built against the D-121 design-system foundation
  // (docs/design/console/CONVENTIONS.md).
  //
  // Console consistency (CONVENTIONS.md §9):
  //  - routes under `(console)/agents/` with no `/console/` URL prefix
  //    (§1); the detail route is `(console)/agents/[id]/` (§1 — a new
  //    detail route uses the `[id]` segment);
  //  - renders inside the shared app shell (§2);
  //  - composes the `components/ui/` inventory — PageHeader, FilterBar,
  //    SavedViewChips, Pagination, StatusChip, PageState — and never
  //    forks a primitive; Agents-specific pieces (cards grid, metrics
  //    rollup) live in `components/agents/` (§3);
  //  - routes ALL async state through the four-state `<PageState>` (§4)
  //    — Disconnected is its OWN state, never conflated with Error;
  //  - clears the §5 depth bar: PageHeader / FilterBar / a primary
  //    canvas (the cards grid) / a tabbed detail route / Console-DB
  //    SavedViewChips / real prev-next Pagination / ConnectionFooter
  //    (shell) / four-state PageState;
  //  - talks to the Runtime ONLY through `HarborClient` + `connection.ts`
  //    (§6) — zero hand-rolled `fetch`;
  //  - introduces no raw token literals (§7).
  //
  // V1 is INSPECTOR-ONLY for authoring (page-agents.md §10): the Console
  // never creates/edits agents — that is CLI-side (RFC §7.4). The five
  // fleet-control verbs live on the detail route. Svelte 5 runes (D-092).
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import {
    AgentsProtocol,
    type Agent,
    type AgentFilter,
    type AgentListResponse,
    type AgentMetrics,
    type AgentStatus
  } from '$lib/protocol/agents.js';
  import { resolveConnection, type RuntimeConnection } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import { AgentsSavedFilters } from '$lib/db/saved_filters_agents.js';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    Pagination,
    PageState,
    type PageStatus,
    type SavedView
  } from '$lib/components/ui';
  import TopMetricsRollup from '$lib/components/agents/TopMetricsRollup.svelte';
  import CardsGrid from '$lib/components/agents/CardsGrid.svelte';

  // ---- test-injection seam (CONVENTIONS.md §6) ---------------------
  // The Playwright harness MAY inject a deterministic `ProtocolClient`
  // so the page is exercised without a live Runtime. Production resolves
  // its own.
  interface AgentsPageGlobals {
    __HARBOR_PROTOCOL_CLIENT__?: ProtocolClient;
  }
  const injected = globalThis as unknown as AgentsPageGlobals;

  // ---- connection + client ----------------------------------------
  let connection = $state<RuntimeConnection | null>(null);
  let agentsApi = $state<AgentsProtocol | null>(null);

  // ---- primary async state (the four-state contract) --------------
  let status = $state<PageStatus>('loading');
  let listError = $state<ProtocolError | { code: string; message: string } | null>(
    null
  );
  let listResp = $state<AgentListResponse | null>(null);
  let metrics = $state<AgentMetrics | null>(null);

  // ---- pagination -------------------------------------------------
  let pageNum = $state(1);
  let pageSize = $state(50);

  // ---- filters ----------------------------------------------------
  let searchText = $state('');
  let statusFacet = $state('');
  let plannerFacet = $state('');

  // ---- saved views (Console-DB-backed, D-061) ---------------------
  let savedFilters = $state<AgentsSavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let savedFilterSpecs = $state<Map<string, AgentFilter>>(new Map());
  let activeViewId = $state<string | null>(null);

  const agents = $derived<Agent[]>(listResp?.agents ?? []);
  const totalRows = $derived(listResp?.total_rows ?? 0);

  /** Six card indices for the loading skeleton. */
  const SKELETON_CARDS = [0, 1, 2, 3, 4, 5];

  /** Assembles the `AgentFilter` from the live filter controls. */
  function currentFilter(): AgentFilter {
    const f: AgentFilter = {};
    if (searchText) f.search = searchText;
    if (statusFacet) f.status = [statusFacet as AgentStatus];
    if (plannerFacet) f.planner_type = [plannerFacet];
    return f;
  }

  /** Maps a thrown error into `<PageState>`'s Error shape. */
  function asPageError(
    err: unknown
  ): ProtocolError | { code: string; message: string } {
    if (err instanceof ProtocolError) return err;
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : String(err)
    };
  }

  /**
   * Loads the agent catalog + the metrics rollup for the current filter
   * / page. The loader is the single re-invocation target the Error
   * state's Retry button calls (CONVENTIONS.md §4 / §8).
   */
  async function loadAgents(): Promise<void> {
    // Disconnected is its OWN state — NEVER the Error UI (CONVENTIONS.md
    // §4 state 1).
    if (agentsApi === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    listError = null;
    try {
      const [list, metricsResp] = await Promise.all([
        agentsApi.list({
          filter: currentFilter(),
          page: pageNum,
          page_size: pageSize
        }),
        agentsApi.metrics()
      ]);
      listResp = list;
      metrics = metricsResp.metrics;
      status = list.agents.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      // The Error state suppresses any stale primary view (§4 state 3).
      listResp = null;
      metrics = null;
      listError = asPageError(err);
      status = 'error';
    }
  }

  function openAgent(id: string): void {
    void goto(`/agents/${encodeURIComponent(id)}`);
  }

  // ---- filter handlers (each resets to page 1 + reloads) ----------
  function applySearch(value: string): void {
    searchText = value;
    pageNum = 1;
    void loadAgents();
  }

  function applyStatusFacet(value: string): void {
    statusFacet = value;
    pageNum = 1;
    void loadAgents();
  }

  function applyPlannerFacet(value: string): void {
    plannerFacet = value;
    pageNum = 1;
    void loadAgents();
  }

  function clearFilters(): void {
    searchText = '';
    statusFacet = '';
    plannerFacet = '';
    activeViewId = null;
    pageNum = 1;
    void loadAgents();
  }

  // ---- pagination handlers ----------------------------------------
  function onPage(p: number): void {
    pageNum = p;
    void loadAgents();
  }

  function onPageSize(size: number): void {
    pageSize = size;
    pageNum = 1;
    void loadAgents();
  }

  // ---- saved views (Console-DB-backed, D-061) ---------------------
  async function refreshSavedViews(): Promise<void> {
    if (savedFilters === null) return;
    try {
      const records = await savedFilters.list();
      savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      savedFilterSpecs = new Map(records.map((r) => [r.id, r.filterSpec]));
    } catch {
      // A Console-DB read failure leaves the chips empty; the page still
      // works (CLAUDE.md §13 — honest, not a fake set).
      savedViews = [];
      savedFilterSpecs = new Map();
    }
  }

  function applySavedView(id: string): void {
    const spec = savedFilterSpecs.get(id);
    if (spec === undefined) return;
    activeViewId = id;
    searchText = spec.search ?? '';
    statusFacet = spec.status?.[0] ?? '';
    plannerFacet = spec.planner_type?.[0] ?? '';
    pageNum = 1;
    void loadAgents();
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) return;
    await savedFilters.delete(id);
    if (activeViewId === id) activeViewId = null;
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    if (savedFilters === null) return;
    const name = globalThis.prompt?.('Name this saved view');
    if (!name) return;
    const record = await savedFilters.create(name, currentFilter());
    activeViewId = record.id;
    await refreshSavedViews();
  }

  // ---- boot --------------------------------------------------------
  onMount(() => {
    connection = resolveConnection();
    if (injected.__HARBOR_PROTOCOL_CLIENT__) {
      agentsApi = new AgentsProtocol(injected.__HARBOR_PROTOCOL_CLIENT__);
    } else if (connection !== null) {
      agentsApi = new AgentsProtocol(new HarborClient({ connection }));
    }
    void loadAgents();

    // Wire the Console-DB-backed saved-view store (D-061). Best-effort:
    // a failure leaves the chips empty but the page works.
    if (connection !== null) {
      void (async () => {
        try {
          const db = await openListPageDB(connection!);
          const operator = await operatorIdOf(
            connection!.identity.tenant,
            connection!.identity.user
          );
          savedFilters = new AgentsSavedFilters(db, operator);
          await refreshSavedViews();
        } catch {
          savedFilters = null;
        }
      })();
    }
  });
</script>

<svelte:head>
  <title>Agents · Harbor Console</title>
</svelte:head>

<div class="agents-page" data-testid="agents-page">
  <PageHeader
    title="Agents"
    subtitle="Fleet management for the runtime's registered agents."
  />

  <TopMetricsRollup {metrics} />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeViewId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <button
        type="button"
        class="action"
        data-testid="agents-save-view"
        disabled={savedFilters === null}
        title={savedFilters === null
          ? 'Saved views need a Console profile'
          : 'Save the current filter as a view'}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}

    {#snippet facets()}
      <label class="facet">
        <span>Status</span>
        <select
          value={statusFacet}
          data-testid="agents-status-facet"
          onchange={(e) => applyStatusFacet(e.currentTarget.value)}
        >
          <option value="">All</option>
          <option value="active">active</option>
          <option value="paused">paused</option>
          <option value="drained">drained</option>
          <option value="force_stopped">force_stopped</option>
        </select>
      </label>
      <label class="facet">
        <span>Planner</span>
        <select
          value={plannerFacet}
          data-testid="agents-planner-facet"
          onchange={(e) => applyPlannerFacet(e.currentTarget.value)}
        >
          <option value="">All</option>
          <option value="react">react</option>
          <option value="deterministic">deterministic</option>
        </select>
      </label>
    {/snippet}

    {#snippet search()}
      <input
        class="search-input"
        type="search"
        placeholder="Search agents by name…"
        data-testid="agents-search"
        value={searchText}
        onchange={(e) => applySearch(e.currentTarget.value)}
      />
    {/snippet}

    {#snippet actions()}
      <button
        type="button"
        class="action"
        data-testid="agents-clear-filters"
        onclick={clearFilters}
      >
        Clear
      </button>
      <button type="button" class="action" onclick={() => void loadAgents()}>
        Refresh
      </button>
    {/snippet}
  </FilterBar>

  <PageState {status} error={listError} onretry={() => void loadAgents()}>
    {#snippet skeleton()}
      <div class="cards-skeleton" aria-hidden="true">
        {#each SKELETON_CARDS as i (i)}
          <span class="skeleton-card"></span>
        {/each}
      </div>
    {/snippet}
    {#snippet empty()}
      <p class="empty-headline">No agents match these filters</p>
      <p class="empty-detail">
        No agents registered — scaffold one with <code>harbor scaffold</code>
        and run it with <code>harbor dev</code>.
      </p>
    {/snippet}

    <CardsGrid {agents} onopen={openAgent} />
  </PageState>

  <Pagination
    page={pageNum}
    {pageSize}
    total={totalRows}
    onpage={onPage}
    onpagesize={onPageSize}
  />
</div>

<style>
  .agents-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .facet {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .facet select {
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
  }

  .search-input {
    width: 100%;
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
  }

  .action {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .cards-skeleton {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(var(--size-card-min), 1fr));
    gap: var(--space-3);
  }

  .skeleton-card {
    height: var(--size-sparkline-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
