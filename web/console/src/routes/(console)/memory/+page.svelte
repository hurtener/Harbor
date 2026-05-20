<script lang="ts">
  // Harbor Console — Memory page (`/memory`), refactored onto the D-121
  // design-system foundation (docs/design/console/CONVENTIONS.md).
  //
  // Console consistency (CONVENTIONS.md §9):
  //  - routes under `(console)/` with no `/console/` URL prefix (§1);
  //  - renders inside the shared app shell (§2);
  //  - composes the `components/ui/` inventory — PageHeader, FilterBar,
  //    SavedViewChips, DataTable, BulkActionBar, DetailRail, RailCard,
  //    StatusChip, Pagination, ConnectionFooter, PageState (§3);
  //  - routes ALL async state through the four-state `<PageState>` (§4) —
  //    Disconnected is its OWN state, never conflated with Error;
  //  - clears the §5 depth bar (header / filter bar / table / detail rail /
  //    Console-DB-backed saved views / real prev-next pagination / footer /
  //    four-state PageState);
  //  - talks to the Runtime ONLY through `HarborClient` + `connection.ts`
  //    (§6) — zero hand-rolled `fetch`;
  //  - introduces no raw token literals (§7).
  //
  // V1 is VIEW-ONLY: the bulk-action bar renders disabled-with-tooltip;
  // the memory mutation surface is deferred to Phase 73 (page-memory.md
  // §10). Svelte 5 runes mode (D-092).
  import { onMount } from 'svelte';
  import {
    HarborClient,
    ProtocolError,
    DEFAULT_MEMORY_LIST_PAGE_SIZE,
    type ProtocolClient,
    type MemoryItem,
    type MemoryItemDetail,
    type MemoryFilter,
    type MemoryHealthAggregate,
    type MemoryStrategyName,
    type MemoryListResponse
  } from '$lib/protocol/harbor.js';
  import { resolveConnection, type RuntimeConnection } from '$lib/connection.js';
  import { openMemorySavedFilters } from '$lib/memory/saved_views.js';
  import type { MemorySavedFilters } from '$lib/db/saved_filters_memory.js';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    BulkActionBar,
    DetailRail,
    RailCard,
    StatusChip,
    Pagination,
    PageState,
    type DataTableColumn,
    type PageStatus,
    type SavedView,
    type StatusKind
  } from '$lib/components/ui';
  import SelectedItemDetail from '$lib/components/memory/SelectedItemDetail.svelte';
  import MemoryHealthCard from '$lib/components/memory/MemoryHealthCard.svelte';
  import RecentIdentityRejectionsCard from '$lib/components/memory/RecentIdentityRejectionsCard.svelte';
  import type { IdentityRejection } from '$lib/components/memory/RecentIdentityRejectionsCard.svelte';
  import RecoveryDropoutsCard from '$lib/components/memory/RecoveryDropoutsCard.svelte';
  import type { RecoveryDropout } from '$lib/components/memory/RecoveryDropoutsCard.svelte';
  import StrategyOverlayChipRow from '$lib/components/memory/StrategyOverlayChipRow.svelte';

  // ---- test-injection seam (CONVENTIONS.md §6) ---------------------
  // A host harness MAY inject a deterministic `ProtocolClient` and a
  // `MemorySavedFilters` facade so the page is exercised without a live
  // Runtime / a real IndexedDB. Production resolves both itself.
  interface MemoryPageGlobals {
    __HARBOR_PROTOCOL_CLIENT__?: ProtocolClient;
    __HARBOR_MEMORY_SAVED_FILTERS__?: MemorySavedFilters;
  }
  const injected = globalThis as unknown as MemoryPageGlobals;

  // ---- connection + client ----------------------------------------
  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);

  // ---- primary-table async state (the four-state contract) --------
  let status = $state<PageStatus>('loading');
  let listError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let listResp = $state<MemoryListResponse | null>(null);

  // ---- detail-rail async state (its OWN nested PageState) ---------
  let detailStatus = $state<PageStatus>('empty');
  let detailError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let selectedKey = $state<string | null>(null);
  let selectedDetail = $state<MemoryItemDetail | null>(null);

  let health = $state<MemoryHealthAggregate | null>(null);
  let checkedKeys = $state<Set<string>>(new Set());

  // ---- pagination -------------------------------------------------
  let pageNum = $state(1);
  let pageSize = $state(DEFAULT_MEMORY_LIST_PAGE_SIZE);

  // ---- filters ----------------------------------------------------
  let contentSearch = $state('');
  let scopeFacet = $state('');
  let driverFacet = $state('');
  let strategyOverlay = $state<MemoryStrategyName | null>(null);

  // ---- saved views (Console-DB-backed, D-061) ---------------------
  let savedFiltersDB = $state<MemorySavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let activeViewId = $state<string | null>(null);

  // ---- right-rail event cards (events stream — a later phase) -----
  const rejections = $state<IdentityRejection[]>([]);
  const dropouts = $state<RecoveryDropout[]>([]);

  const items = $derived<MemoryItem[]>(listResp?.items ?? []);
  const totalRows = $derived(listResp?.total_rows ?? 0);

  /** Six row indices for the loading skeleton. */
  const SKELETON_ROWS = [0, 1, 2, 3, 4, 5];

  const columns: DataTableColumn[] = [
    { key: 'key', label: 'Memory key' },
    { key: 'strategy', label: 'Strategy' },
    { key: 'scope', label: 'Scope' },
    { key: 'owner', label: 'Owner' },
    { key: 'created', label: 'Created' },
    { key: 'updated', label: 'Last updated' },
    { key: 'ttl', label: 'TTL / Expires' },
    { key: 'size', label: 'Size', numeric: true },
    { key: 'driver', label: 'Driver' }
  ];

  /** Maps a memory strategy onto a `StatusChip` kind. */
  function strategyKind(strategy: string): StatusKind {
    if (strategy === 'rolling_summary') return 'success';
    if (strategy === 'truncation') return 'accent';
    return 'neutral';
  }

  /** Short, stable timestamp label (no priority dimension — D-065). */
  function shortTime(iso: string | undefined): string {
    if (!iso) return '—';
    const d = new Date(iso);
    return Number.isNaN(d.getTime())
      ? '—'
      : d.toISOString().slice(0, 19).replace('T', ' ');
  }

  /** Assembles the `MemoryFilter` from the live filter controls. */
  function currentFilter(): MemoryFilter {
    const f: MemoryFilter = {};
    if (contentSearch) f.content_search = contentSearch;
    if (scopeFacet) f.scopes = [scopeFacet];
    if (driverFacet) f.drivers = [driverFacet];
    if (strategyOverlay) f.strategies = [strategyOverlay];
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
   * Loads the memory list + health for the current filter / page. The
   * loader is the single re-invocation target the Error-state Retry
   * button calls (CONVENTIONS.md §4 / §8).
   */
  async function loadList(): Promise<void> {
    // Disconnected is its OWN state — NEVER the Error UI (CONVENTIONS.md
    // §4 state 1). connection.ts returning null is the honest "no
    // Runtime" signal, not a request failure.
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    listError = null;
    try {
      const [list, healthResp] = await Promise.all([
        client.memory.list({
          filter: currentFilter(),
          page: pageNum,
          page_size: pageSize
        }),
        client.memory.health()
      ]);
      listResp = list;
      health = healthResp.aggregate;
      status = list.items.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      // The Error state suppresses any stale primary view (§4 state 3).
      listResp = null;
      health = null;
      listError = asPageError(err);
      status = 'error';
    }
  }

  /** Loads one record's detail into the rail's nested `<PageState>`. */
  async function loadDetail(key: string): Promise<void> {
    selectedKey = key;
    if (client === null) {
      detailStatus = 'disconnected';
      return;
    }
    detailStatus = 'loading';
    detailError = null;
    selectedDetail = null;
    try {
      const resp = await client.memory.get(key);
      selectedDetail = resp.detail;
      detailStatus = 'ready';
    } catch (err) {
      detailError = asPageError(err);
      detailStatus = 'error';
    }
  }

  function onSelectionChange(next: Set<string>): void {
    checkedKeys = next;
  }

  function onRowClick(row: unknown): void {
    void loadDetail((row as MemoryItem).key);
  }

  // ---- filter handlers (each resets to page 1 + reloads) ----------
  function applyContentSearch(value: string): void {
    contentSearch = value;
    pageNum = 1;
    void loadList();
  }

  function applyScopeFacet(value: string): void {
    scopeFacet = value;
    pageNum = 1;
    void loadList();
  }

  function applyDriverFacet(value: string): void {
    driverFacet = value;
    pageNum = 1;
    void loadList();
  }

  function applyStrategyOverlay(s: MemoryStrategyName | null): void {
    strategyOverlay = s;
    pageNum = 1;
    void loadList();
  }

  // ---- pagination handlers ----------------------------------------
  function onPage(p: number): void {
    pageNum = p;
    void loadList();
  }

  function onPageSize(size: number): void {
    pageSize = size;
    pageNum = 1;
    void loadList();
  }

  // ---- saved views (Console-DB-backed, D-061) ---------------------
  async function refreshSavedViews(): Promise<void> {
    if (savedFiltersDB === null) return;
    const rows = await savedFiltersDB.list();
    savedViews = rows.map((r) => ({ id: r.id, name: r.name }));
  }

  async function applySavedView(id: string): Promise<void> {
    if (savedFiltersDB === null) return;
    const saved = await savedFiltersDB.get(id);
    if (saved === null) return;
    activeViewId = id;
    contentSearch = saved.filter.content_search ?? '';
    scopeFacet = saved.filter.scopes?.[0] ?? '';
    driverFacet = saved.filter.drivers?.[0] ?? '';
    strategyOverlay = (saved.filter.strategies?.[0] as MemoryStrategyName) ?? null;
    pageNum = 1;
    void loadList();
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFiltersDB === null) return;
    await savedFiltersDB.delete(id);
    if (activeViewId === id) activeViewId = null;
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    if (savedFiltersDB === null) return;
    const name = globalThis.prompt?.('Name this saved view');
    if (!name) return;
    const id = `mem-view-${Date.now()}`;
    await savedFiltersDB.put({ id, name, filter: currentFilter() });
    await refreshSavedViews();
    activeViewId = id;
  }

  /** Console-local NDJSON / CSV export of the current page (D-061). */
  function exportSnapshot(format: 'ndjson' | 'csv'): void {
    if (typeof globalThis.document === 'undefined') return;
    let body: string;
    if (format === 'ndjson') {
      body = items.map((it) => JSON.stringify(it)).join('\n');
    } else {
      const header = 'key,strategy,scope,driver,size_bytes';
      const rows = items.map(
        (it) => `${it.key},${it.strategy},${it.scope},${it.driver},${it.size_bytes}`
      );
      body = [header, ...rows].join('\n');
    }
    const blob = new globalThis.Blob([body], {
      type: format === 'ndjson' ? 'application/x-ndjson' : 'text/csv'
    });
    const url = globalThis.URL.createObjectURL(blob);
    const a = globalThis.document.createElement('a');
    a.href = url;
    a.download = `memory-export.${format}`;
    a.click();
    globalThis.URL.revokeObjectURL(url);
  }

  // ---- boot --------------------------------------------------------
  onMount(() => {
    connection = resolveConnection();
    if (injected.__HARBOR_PROTOCOL_CLIENT__) {
      client = injected.__HARBOR_PROTOCOL_CLIENT__;
    } else if (connection !== null) {
      client = new HarborClient({ connection });
    }
    void loadList();

    // Resolve the Console-DB-backed saved-view facade. An injected
    // facade wins (tests); otherwise open the real IndexedDB store.
    if (injected.__HARBOR_MEMORY_SAVED_FILTERS__) {
      savedFiltersDB = injected.__HARBOR_MEMORY_SAVED_FILTERS__;
      void refreshSavedViews();
    } else if (connection !== null) {
      void openMemorySavedFilters(connection).then((facade) => {
        savedFiltersDB = facade;
        void refreshSavedViews();
      });
    }
  });
</script>

<svelte:head>
  <title>Memory · Harbor Console</title>
</svelte:head>

<div class="memory-page" data-testid="memory-page">
  <PageHeader
    title="Memory"
    subtitle="Per-identity inspector for the runtime's memory subsystem."
  >
    {#snippet actions()}
      <button
        type="button"
        class="action"
        data-testid="memory-export-ndjson"
        disabled={items.length === 0}
        onclick={() => exportSnapshot('ndjson')}
      >
        Export NDJSON
      </button>
      <button
        type="button"
        class="action"
        data-testid="memory-export-csv"
        disabled={items.length === 0}
        onclick={() => exportSnapshot('csv')}
      >
        Export CSV
      </button>
    {/snippet}
  </PageHeader>

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeViewId}
        onselect={(id) => void applySavedView(id)}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <button
        type="button"
        class="action save-view"
        data-testid="memory-save-view"
        disabled={savedFiltersDB === null}
        title={savedFiltersDB === null
          ? 'Saved views need a profile unlocked in Settings'
          : 'Save the current filter as a view'}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}

    {#snippet facets()}
      <label class="facet">
        <span>Scope</span>
        <select
          value={scopeFacet}
          data-testid="memory-scope-facet"
          onchange={(e) => applyScopeFacet(e.currentTarget.value)}
        >
          <option value="">All</option>
          <option value="session">session</option>
          <option value="user">user</option>
          <option value="tenant">tenant</option>
        </select>
      </label>
      <label class="facet">
        <span>Driver</span>
        <select
          value={driverFacet}
          data-testid="memory-driver-facet"
          onchange={(e) => applyDriverFacet(e.currentTarget.value)}
        >
          <option value="">All</option>
          <option value="inmem">inmem</option>
          <option value="sqlite">sqlite</option>
          <option value="postgres">postgres</option>
        </select>
      </label>
    {/snippet}

    {#snippet search()}
      <input
        class="search-input"
        type="search"
        placeholder="Search memory content…"
        data-testid="memory-content-search"
        value={contentSearch}
        onchange={(e) => applyContentSearch(e.currentTarget.value)}
      />
    {/snippet}

    {#snippet actions()}
      <button type="button" class="action" onclick={() => void loadList()}>
        Refresh
      </button>
    {/snippet}
  </FilterBar>

  <StrategyOverlayChipRow
    selected={strategyOverlay}
    onSelect={applyStrategyOverlay}
  />

  <BulkActionBar count={checkedKeys.size} onclear={() => (checkedKeys = new Set())}>
    {#snippet actions()}
      <!-- V1 is read-only: the memory mutation surface lands in Phase 73
           (page-memory.md §10). Each action is disabled-with-tooltip,
           NOT hidden — a screen-reader user hears the deferral, and the
           button wires to no Protocol call (CONVENTIONS.md §5, §13). -->
      {#each ['Delete selected', 'Refresh TTL', 'Pin'] as label (label)}
        <button
          type="button"
          class="action"
          disabled
          aria-disabled="true"
          title="Memory mutation surface deferred — Phase 73"
        >
          {label}
        </button>
      {/each}
    {/snippet}
  </BulkActionBar>

  <div class="layout">
    <main class="main">
      <PageState status={status} error={listError} onretry={() => void loadList()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each SKELETON_ROWS as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <p class="empty-headline">No memory items in this scope</p>
          <p class="empty-detail">
            Memory builds up during runs — open
            <a href="/live-runtime">Live Runtime</a> to start one.
          </p>
        {/snippet}

        <DataTable
          {columns}
          rows={items}
          rowKey={(r) => (r as MemoryItem).key}
          selectable
          selected={checkedKeys}
          onselectionchange={onSelectionChange}
          onrowclick={onRowClick}
        >
          {#snippet row(r)}
            {@const item = r as MemoryItem}
            <td class="mono key">{item.key}</td>
            <td>
              <StatusChip kind={strategyKind(item.strategy)} label={item.strategy} />
            </td>
            <td><StatusChip kind="neutral" label={item.scope} /></td>
            <td class="owner mono">
              {item.identity.tenant} / {item.identity.user} / {item.identity.session}
            </td>
            <td>{shortTime(item.created_at)}</td>
            <td>{shortTime(item.last_updated_at)}</td>
            <td>{shortTime(item.expires_at)}</td>
            <td class="numeric">{item.size_bytes}</td>
            <td><StatusChip kind="neutral" label={item.driver} /></td>
          {/snippet}
        </DataTable>
      </PageState>

      <Pagination
        page={pageNum}
        {pageSize}
        total={totalRows}
        onpage={onPage}
        onpagesize={onPageSize}
      />
    </main>

    <DetailRail>
      <RailCard title="Memory health">
        <MemoryHealthCard aggregate={health} />
      </RailCard>
      <RailCard title="Recent identity rejections">
        <RecentIdentityRejectionsCard {rejections} />
      </RailCard>
      <RailCard title="Recovery dropouts">
        <RecoveryDropoutsCard {dropouts} />
      </RailCard>
      <RailCard title="Selected item">
        <!-- The rail gets its OWN nested <PageState> (CONVENTIONS.md §4):
             a rail-load failure surfaces in the rail, not the whole page. -->
        <PageState status={detailStatus} error={detailError}
          onretry={() => selectedKey && void loadDetail(selectedKey)}>
          {#snippet empty()}
            <p class="rail-empty">Select a memory row to inspect its detail.</p>
          {/snippet}
          {#if selectedDetail}
            <SelectedItemDetail detail={selectedDetail} />
          {/if}
        </PageState>
      </RailCard>
    </DetailRail>
  </div>
</div>

<style>
  .memory-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .layout {
    display: flex;
    gap: var(--space-4);
    align-items: flex-start;
  }

  .main {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
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

  .save-view {
    font-size: var(--text-xs);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .key {
    overflow-wrap: anywhere;
  }

  .owner {
    color: var(--color-text-muted);
  }

  td {
    padding: var(--space-2) var(--space-3);
  }

  td.numeric {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4) var(--space-0);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
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

  .empty-detail a {
    color: var(--color-accent);
  }

  .rail-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
