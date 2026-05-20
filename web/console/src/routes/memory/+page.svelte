<script lang="ts">
  // Harbor Console — Memory page (`/console/memory`), Phase 73j / D-118.
  //
  // The per-identity inspector for the runtime's memory subsystem. It
  // is a Protocol client (D-091): it talks to the Runtime ONLY through
  // the typed MemoryClient (no hand-rolled `fetch` — §4.5). V1 is
  // VIEW-ONLY: the bulk-action toolbar renders disabled-with-tooltip;
  // the memory mutation surface is deferred to Phase 73 (page-memory.md
  // §10). Svelte 5 runes mode (D-092); design tokens only (§4.5).
  import { MemoryClient, MemoryProtocolError } from '$lib/protocol-memory';
  import type {
    MemoryItem,
    MemoryItemDetail,
    MemoryFilter,
    MemoryHealthAggregate,
    MemoryStrategyName
  } from '$lib/protocol-memory';
  import type { MemorySavedFilter } from '$lib/db/saved_filters_memory';
  import MemoryTable from '$lib/components/memory/MemoryTable.svelte';
  import SelectedItemDetail from '$lib/components/memory/SelectedItemDetail.svelte';
  import MemoryHealthCard from '$lib/components/memory/MemoryHealthCard.svelte';
  import RecentIdentityRejectionsCard from '$lib/components/memory/RecentIdentityRejectionsCard.svelte';
  import type { IdentityRejection } from '$lib/components/memory/RecentIdentityRejectionsCard.svelte';
  import RecoveryDropoutsCard from '$lib/components/memory/RecoveryDropoutsCard.svelte';
  import type { RecoveryDropout } from '$lib/components/memory/RecoveryDropoutsCard.svelte';
  import BulkActionToolbar from '$lib/components/memory/BulkActionToolbar.svelte';
  import StrategyOverlayChipRow from '$lib/components/memory/StrategyOverlayChipRow.svelte';
  import SubHeaderStrip from '$lib/components/memory/SubHeaderStrip.svelte';

  // The storage key the Console reads its session token from (D-091 —
  // mirrors AUTH_STORAGE_KEY in the Playwright harness fixture).
  const AUTH_STORAGE_KEY = 'harbor.console.token';

  // ---- Reactive page state (Svelte 5 runes) ----
  let items = $state<MemoryItem[]>([]);
  let totalRows = $state(0);
  let pageNum = $state(1);
  let pageCount = $state(0);
  let health = $state<MemoryHealthAggregate | null>(null);
  let selectedKey = $state<string | null>(null);
  let selectedDetail = $state<MemoryItemDetail | null>(null);
  let detailLoading = $state(false);
  let detailError = $state<string | null>(null);
  let checkedKeys = $state<Set<string>>(new Set());
  let listError = $state<string | null>(null);
  let listLoading = $state(false);

  // ---- Filter state ----
  let contentSearch = $state('');
  let scopeFacet = $state('');
  let driverFacet = $state('');
  let strategyOverlay = $state<MemoryStrategyName | null>(null);

  // Saved-filter chips + right-rail event cards. These are wired by a
  // future Console phase (the Console DB driver + the events stream
  // subscription); the page renders them today with the empty list so
  // the layout + the verbatim-rendering carve-outs are exercised.
  let savedFilters = $state<MemorySavedFilter[]>([]);
  let rejections = $state<IdentityRejection[]>([]);
  let dropouts = $state<RecoveryDropout[]>([]);

  /** Builds the typed MemoryClient from the runtime origin + token. */
  function buildClient(): MemoryClient | null {
    if (typeof globalThis.localStorage === 'undefined') return null;
    const token = globalThis.localStorage.getItem(AUTH_STORAGE_KEY);
    if (!token) return null;
    return new MemoryClient({
      baseURL: globalThis.location.origin,
      token,
      // The operator identity is decoded from the JWT by a future
      // Console phase; the Memory page does not need it for the read
      // surface (the Runtime resolves identity from the bearer).
      identity: { tenantID: '', userID: '' }
    });
  }

  /** The current MemoryFilter assembled from the filter controls. */
  function currentFilter(): MemoryFilter {
    const f: MemoryFilter = {};
    if (contentSearch) f.content_search = contentSearch;
    if (scopeFacet) f.scopes = [scopeFacet];
    if (driverFacet) f.drivers = [driverFacet];
    if (strategyOverlay) f.strategies = [strategyOverlay];
    return f;
  }

  /** Loads the memory list + health for the current filter. */
  async function loadList(): Promise<void> {
    const client = buildClient();
    if (!client) {
      listError = 'Not connected — attach a Harbor runtime to inspect memory.';
      return;
    }
    listLoading = true;
    listError = null;
    try {
      const [listResp, healthResp] = await Promise.all([
        client.list({ filter: currentFilter(), page: pageNum }),
        client.health()
      ]);
      items = listResp.items;
      totalRows = listResp.total_rows;
      pageCount = listResp.page_count;
      health = healthResp.aggregate;
    } catch (e) {
      items = [];
      health = null;
      listError =
        e instanceof MemoryProtocolError
          ? `${e.code}: ${e.message}`
          : `Failed to load memory: ${String(e)}`;
    } finally {
      listLoading = false;
    }
  }

  /** Loads one record's detail into the right rail. */
  async function loadDetail(key: string): Promise<void> {
    selectedKey = key;
    const client = buildClient();
    if (!client) {
      detailError = 'Not connected.';
      return;
    }
    detailLoading = true;
    detailError = null;
    selectedDetail = null;
    try {
      const resp = await client.get({ key });
      selectedDetail = resp.detail;
    } catch (e) {
      detailError =
        e instanceof MemoryProtocolError
          ? `${e.code}: ${e.message}`
          : `Failed to load detail: ${String(e)}`;
    } finally {
      detailLoading = false;
    }
  }

  function toggleCheck(key: string): void {
    const next = new Set(checkedKeys);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    checkedKeys = next;
  }

  function onContentSearch(v: string): void {
    contentSearch = v;
    pageNum = 1;
    void loadList();
  }

  function onScopeFacet(v: string): void {
    scopeFacet = v;
    pageNum = 1;
    void loadList();
  }

  function onDriverFacet(v: string): void {
    driverFacet = v;
    pageNum = 1;
    void loadList();
  }

  function onStrategyOverlay(s: MemoryStrategyName | null): void {
    strategyOverlay = s;
    pageNum = 1;
    void loadList();
  }

  /** Console-local NDJSON / CSV export of the current filtered page (D-061). */
  function onExport(format: 'ndjson' | 'csv'): void {
    if (typeof globalThis.document === 'undefined') return;
    let body: string;
    if (format === 'ndjson') {
      body = items.map((it) => JSON.stringify(it)).join('\n');
    } else {
      const header = 'key,strategy,scope,driver,size_bytes';
      const rows = items.map(
        (it) =>
          `${it.key},${it.strategy},${it.scope},${it.driver},${it.size_bytes}`
      );
      body = [header, ...rows].join('\n');
    }
    const blob = new globalThis.Blob([body], {
      type: format === 'ndjson' ? 'application/x-ndjson' : 'text/csv'
    });
    const url = globalThis.URL.createObjectURL(blob);
    const a = globalThis.document.createElement('a');
    a.href = url;
    a.download = `memory-export.${format === 'ndjson' ? 'ndjson' : 'csv'}`;
    a.click();
    globalThis.URL.revokeObjectURL(url);
  }

  // Initial load on mount.
  $effect(() => {
    void loadList();
  });
</script>

<svelte:head>
  <title>Memory · Harbor Console</title>
</svelte:head>

<div class="memory-page" data-testid="memory-page">
  <header class="page-header">
    <h1>Memory</h1>
    <p class="subtitle">
      Per-identity inspector for the runtime's memory subsystem.
    </p>
  </header>

  <SubHeaderStrip
    {savedFilters}
    {contentSearch}
    {scopeFacet}
    {driverFacet}
    {onContentSearch}
    {onScopeFacet}
    {onDriverFacet}
    onApplySaved={() => void loadList()}
    onRefresh={() => void loadList()}
    {onExport}
  />

  <StrategyOverlayChipRow selected={strategyOverlay} onSelect={onStrategyOverlay} />

  <BulkActionToolbar selectedCount={checkedKeys.size} />

  <div class="layout">
    <main class="main">
      {#if listLoading}
        <p class="status" data-testid="memory-loading">Loading memory items…</p>
      {:else if listError}
        <p class="status error" role="alert" data-testid="memory-error">
          {listError}
        </p>
      {/if}
      <MemoryTable
        {items}
        {selectedKey}
        {checkedKeys}
        onSelect={(key) => void loadDetail(key)}
        onToggleCheck={toggleCheck}
      />
      <footer class="pager">
        <span>{totalRows} items · page {pageNum} of {Math.max(pageCount, 1)}</span>
      </footer>
    </main>

    <aside class="rail">
      <MemoryHealthCard aggregate={health} />
      <RecentIdentityRejectionsCard {rejections} />
      <RecoveryDropoutsCard {dropouts} />
      <SelectedItemDetail
        detail={selectedDetail}
        loading={detailLoading}
        error={detailError}
      />
    </aside>
  </div>

  <footer class="page-footer">
    <span>Harbor Console · Memory · Protocol-client (D-091)</span>
  </footer>
</div>

<style>
  .memory-page {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-6);
  }

  .page-header h1 {
    font-size: var(--text-xl);
    margin: var(--space-0) var(--space-0) var(--space-1);
  }

  .subtitle {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    margin: var(--space-0);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main {
    background: var(--color-surface);
    border: var(--border-width-hairline) solid var(--color-border);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .rail {
    display: grid;
    gap: var(--space-4);
  }

  .status {
    padding: var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .status.error {
    color: var(--color-danger);
  }

  .pager {
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border-top: var(--border-width-hairline) solid var(--color-border);
  }

  .page-footer {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
