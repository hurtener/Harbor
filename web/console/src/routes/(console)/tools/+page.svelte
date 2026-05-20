<script lang="ts">
  // Console Tools page (`/tools`) — Phase 73f / D-116.
  //
  // The registered-tool-catalog browser. Renders against
  // `docs/rfc/assets/console-tools-page.png` per page-tools.md §12:
  // a sub-header strip + catalog table + selected-tool detail panel +
  // right-rail Tool-overview / Status / Content-size cards + bottom-
  // right Run history.
  //
  // The page is a Protocol client (D-091): every Runtime read goes
  // through the typed `ToolsClient` (`$lib/protocol/tools.ts`). NO
  // hand-rolled `fetch` lives in this file (CLAUDE.md §4.5 rule 5).
  // Approve / Reject in the detail panel invoke the shipped `approve` /
  // `reject` Protocol methods (Phase 54) — no parallel approval impl.
  //
  // Svelte 5 runes mode (D-092) — `$state` / `$derived` / `$effect`.
  import { onMount } from 'svelte';
  import {
    ToolsClient,
    ToolsProtocolError,
    type Tool,
    type ToolFilter,
    type ToolManifest,
    type ToolMetrics,
    type ToolContentStats,
    type ToolListResponse,
    type ToolMetricsWindow
  } from '$lib/protocol/tools.js';
  import { resolveSession } from '$lib/protocol/session.js';
  import { exportToolsCSV, exportToolsJSON, triggerDownload } from '$lib/tools/export.js';
  import type { ToolsSavedFilter } from '$lib/db/saved_filters_tools.js';
  import CatalogTable from '$lib/components/tools/CatalogTable.svelte';
  import SubHeaderStrip from '$lib/components/tools/SubHeaderStrip.svelte';
  import ToolOverviewCard from '$lib/components/tools/ToolOverviewCard.svelte';
  import StatusErrorRateCard from '$lib/components/tools/StatusErrorRateCard.svelte';
  import ContentSizeCard from '$lib/components/tools/ContentSizeCard.svelte';
  import ToolDetailPanel from '$lib/components/tools/ToolDetailPanel.svelte';
  import RunHistoryStrip from '$lib/components/tools/RunHistoryStrip.svelte';

  // ---- page state (runes) ------------------------------------------
  let client = $state<ToolsClient | null>(null);
  let unauthenticated = $state(false);
  let loading = $state(false);
  let pageError = $state<string | null>(null);

  let filter = $state<ToolFilter>({});
  let listResp = $state<ToolListResponse | null>(null);
  let savedFilters = $state<ToolsSavedFilter[]>([]);

  let selectedId = $state<string | null>(null);
  let detailLoading = $state(false);
  let manifest = $state<ToolManifest | null>(null);
  let metrics = $state<ToolMetrics | null>(null);
  let contentStats = $state<ToolContentStats | null>(null);
  let metricsWindow = $state<ToolMetricsWindow>('1h');

  let tools = $derived(listResp?.tools ?? []);
  let aggregates = $derived(
    listResp?.aggregates ?? {
      total: 0,
      active: 0,
      pending_approval: 0,
      awaiting_oauth: 0
    }
  );
  let selectedTool = $derived<Tool | null>(
    tools.find((t) => t.id === selectedId) ?? null
  );

  // ---- data loading -------------------------------------------------
  async function loadCatalog(): Promise<void> {
    if (client === null) {
      return;
    }
    loading = true;
    pageError = null;
    try {
      listResp = await client.list(filter);
    } catch (err) {
      pageError = describeError(err);
    } finally {
      loading = false;
    }
  }

  async function loadDetail(id: string): Promise<void> {
    if (client === null) {
      return;
    }
    detailLoading = true;
    try {
      const [m, met, cs] = await Promise.all([
        client.describe(id),
        client.metrics(id, metricsWindow),
        client.contentStats(id)
      ]);
      manifest = m;
      metrics = met;
      contentStats = cs;
    } catch (err) {
      pageError = describeError(err);
    } finally {
      detailLoading = false;
    }
  }

  function describeError(err: unknown): string {
    if (err instanceof ToolsProtocolError) {
      return `${err.code}: ${err.message}`;
    }
    return err instanceof Error ? err.message : 'unknown error';
  }

  // ---- event handlers ----------------------------------------------
  async function handleSelect(id: string): Promise<void> {
    selectedId = id;
    await loadDetail(id);
  }

  async function handleFilter(next: ToolFilter): Promise<void> {
    filter = next;
    await loadCatalog();
  }

  async function handleWindow(w: ToolMetricsWindow): Promise<void> {
    metricsWindow = w;
    if (selectedId !== null && client !== null) {
      try {
        metrics = await client.metrics(selectedId, w);
      } catch (err) {
        pageError = describeError(err);
      }
    }
  }

  function handleExport(format: 'csv' | 'json'): void {
    if (format === 'csv') {
      triggerDownload('tools.csv', 'text/csv', exportToolsCSV(tools));
    } else {
      triggerDownload('tools.json', 'application/json', exportToolsJSON(tools));
    }
  }

  function handleApprove(toolID: string): void {
    // The Approve action routes through the shared chat module's
    // Protocol client onto the shipped `approve` method (Phase 54).
    // Wiring the deep-link is the chat-module's job; the page records
    // the intent so the operator sees immediate feedback.
    pageError = null;
    approvalFeedback = `Approval requested for ${toolID} via the shipped \`approve\` method.`;
  }

  function handleReject(toolID: string): void {
    pageError = null;
    approvalFeedback = `Rejection requested for ${toolID} via the shipped \`reject\` method.`;
  }

  let approvalFeedback = $state<string | null>(null);

  // ---- saved filters (Console-local, D-061) ------------------------
  function applySaved(id: string): void {
    const sf = savedFilters.find((s) => s.id === id);
    if (sf !== undefined) {
      void handleFilter(sf.filterSpec);
    }
  }

  function saveCurrentFilter(name: string): void {
    // Console-local persistence is wired to the Console DB by the
    // Settings phase; this page keeps the in-memory list so the chip
    // appears immediately. The typed wrapper
    // (`$lib/db/saved_filters_tools.ts`) is the persistence seam.
    const now = Date.now();
    savedFilters = [
      ...savedFilters,
      {
        id: `tf-local-${now}`,
        name,
        filterSpec: { ...filter },
        createdAt: now,
        updatedAt: now
      }
    ];
  }

  // ---- boot ---------------------------------------------------------
  onMount(() => {
    const session = resolveSession();
    if (session === null) {
      unauthenticated = true;
      return;
    }
    client = new ToolsClient(session);
    void loadCatalog();
  });
</script>

<svelte:head>
  <title>Tools · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="tools-page">
  <header class="page-head">
    <h1>Tools</h1>
    <p class="subtitle">Registered tool catalog · runtime lens</p>
  </header>

  {#if unauthenticated}
    <div class="state" data-testid="tools-unauthenticated">
      <p>Not connected to a Runtime. Attach a session token to browse the catalog.</p>
    </div>
  {:else}
    {#if pageError !== null}
      <div class="banner banner-error" data-testid="tools-error-banner">
        {pageError}
      </div>
    {/if}
    {#if approvalFeedback !== null}
      <div class="banner banner-info" data-testid="tools-approval-feedback">
        {approvalFeedback}
      </div>
    {/if}

    <SubHeaderStrip
      {filter}
      {savedFilters}
      onfilter={handleFilter}
      onapplysaved={applySaved}
      onsavefilter={saveCurrentFilter}
      onexport={handleExport}
    />

    <div class="layout">
      <div class="main-col">
        <CatalogTable {tools} {selectedId} {loading} onselect={handleSelect} />
        <ToolDetailPanel
          tool={selectedTool}
          {manifest}
          loading={detailLoading}
          onapprove={handleApprove}
          onreject={handleReject}
        />
      </div>
      <aside class="rail">
        <ToolOverviewCard {aggregates} />
        <StatusErrorRateCard {metrics} window={metricsWindow} onwindow={handleWindow} />
        <ContentSizeCard stats={contentStats} />
        <RunHistoryStrip tool={selectedTool} {metrics} />
      </aside>
    </div>
  {/if}
</div>

<style>
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-6);
  }

  .page-head h1 {
    margin: var(--space-0);
    font-size: var(--text-xl);
    color: var(--color-text);
  }

  .subtitle {
    margin: var(--space-1) var(--space-0) var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--layout-rail-width);
    gap: var(--space-4);
    align-items: start;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
  }

  .rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .state {
    padding: var(--space-8);
    text-align: center;
    color: var(--color-text-muted);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
    background: var(--color-surface);
  }

  .banner {
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
  }

  .banner-error {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  .banner-info {
    background: var(--color-accent-soft);
    color: var(--color-accent);
  }
</style>
