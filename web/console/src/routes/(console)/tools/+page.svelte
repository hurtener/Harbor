<script lang="ts">
  // Harbor Console — Tools page (`/tools`), refactored onto the D-121
  // design-system foundation (CONVENTIONS.md; refactor of Phase 73f).
  //
  // The registered-tool-catalog browser. This page is built entirely
  // against the shared foundation:
  //   - the four-state `<PageState>` async contract (CONVENTIONS.md §4):
  //     Disconnected / Loading / Error / Empty, mutually exclusive; the
  //     Error state has a working Retry and suppresses any stale table.
  //   - the shared `ui/` inventory (CONVENTIONS.md §3): `PageHeader`,
  //     `FilterBar`, `SavedViewChips`, `DataTable`, `BulkActionBar`,
  //     `DetailRail`/`RailCard`, `Pagination`, `StatusChip`. Genuinely
  //     Tools-specific pieces (`ToolDetailTabs`, the rail-card bodies,
  //     `ToolFacetChips`) live in `components/tools/` and compose `ui/`
  //     primitives underneath.
  //   - the unified `HarborClient` + `connection.ts` (CONVENTIONS.md §6):
  //     every Runtime read goes through `client.tools.*`; no hand-rolled
  //     `fetch`, no page-local Protocol client, no direct `localStorage`.
  //   - Console-DB-backed `SavedViewChips` (D-061): the saved filters
  //     persist in the Console's IndexedDB store via `ToolsSavedFilters`.
  //
  // §13 fix — no stubbed action presented as done. The pre-refactor page
  // set a FAKE `approvalFeedback` string instead of calling the Protocol.
  // The Approve / Reject controls now invoke the REAL admin Protocol
  // method `tools.set_approval_policy` (Approve → `auto`, Reject →
  // `denied`); when the connection lacks the `admin` scope claim the
  // controls render disabled-with-tooltip (CONVENTIONS.md §5).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount } from 'svelte';
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import BulkActionBar from '$lib/components/ui/BulkActionBar.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import ToolFacetChips from '$lib/components/tools/ToolFacetChips.svelte';
  import ToolOverviewCard from '$lib/components/tools/ToolOverviewCard.svelte';
  import StatusErrorRateCard from '$lib/components/tools/StatusErrorRateCard.svelte';
  import ContentSizeCard from '$lib/components/tools/ContentSizeCard.svelte';
  import ToolDetailTabs from '$lib/components/tools/ToolDetailTabs.svelte';
  import RunHistoryStrip from '$lib/components/tools/RunHistoryStrip.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import {
    resolveConnection,
    hasScope,
    DISCONNECTED_TOOLTIP,
    type RuntimeConnection
  } from '$lib/connection.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { ToolsSavedFilters } from '$lib/db/saved_filters_tools.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import { exportToolsCSV, exportToolsJSON, triggerDownload } from '$lib/tools/export.js';
  import type {
    Tool,
    ToolFilter,
    ToolManifest,
    ToolMetrics,
    ToolContentStats,
    ToolListResponse,
    ToolMetricsWindow,
    ToolAggregates,
    ToolApprovalPolicy,
    ToolOAuthStatus
  } from '$lib/protocol/tools.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  // The page is constructed with an injectable `ProtocolClient` so the
  // Playwright harness can swap in a deterministic in-page client. In
  // production the page resolves its own connection via `connection.ts`.
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // The Phase 83r W3 disconnected predicate — drives Refresh / Apply /
  // Clear / Export / Save-view + collapses the secondary
  // `ToolDetailTabs` empty state so the disconnected page renders ONE
  // empty message (N5), not two stacked ones.
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- catalog list state ----------------------------------------- */
  let filter = $state<ToolFilter>({});
  let page = $state(1);
  let pageSize = $state(50);
  let listResp = $state<ToolListResponse | null>(null);
  let searchText = $state('');

  let tools = $derived<Tool[]>(listResp?.tools ?? []);
  let aggregates = $derived<ToolAggregates>(
    listResp?.aggregates ?? {
      total: 0,
      active: 0,
      pending_approval: 0,
      awaiting_oauth: 0
    }
  );
  let totalRows = $derived(listResp?.total_rows ?? 0);

  /* ---- selection + bulk actions ----------------------------------- */
  let selected = $state<Set<string>>(new Set<string>());

  /* ---- detail (nested PageState) ---------------------------------- */
  let selectedId = $state<string | null>(null);
  let detailStatus = $state<PageStatus>('empty');
  let detailError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let manifest = $state<ToolManifest | null>(null);
  let metrics = $state<ToolMetrics | null>(null);
  let contentStats = $state<ToolContentStats | null>(null);
  let metricsWindow = $state<ToolMetricsWindow>('1h');

  let selectedTool = $derived<Tool | null>(
    tools.find((t) => t.id === selectedId) ?? null
  );

  /* ---- approval (the §13 fix — real Protocol calls) --------------- */
  let canAdmin = $state(false);
  let approvalPending = $state(false);
  let approvalResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- saved views (Console-DB-backed, D-061) --------------------- */
  let savedFilters = $state<ToolsSavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let savedFilterSpecs = $state<Map<string, ToolFilter>>(new Map());
  let activeSavedId = $state<string | null>(null);

  /* ================================================================ */
  /* Catalog loading                                                   */
  /* ================================================================ */

  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : 'unknown error'
    };
  }

  async function loadCatalog(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      const resp = await client.tools.list<ToolListResponse>(
        filter as Record<string, unknown>,
        page,
        pageSize
      );
      listResp = resp;
      // Empty is a SUCCEEDED-with-zero-rows state, distinct from Error
      // and from Disconnected (CONVENTIONS.md §4).
      status = resp.tools.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      // The Error state suppresses any stale table — drop last-good data
      // so a page in Error never shows data underneath the banner.
      listResp = null;
      pageError = toError(err);
      status = 'error';
    }
  }

  async function loadDetail(id: string): Promise<void> {
    if (client === null) {
      return;
    }
    detailStatus = 'loading';
    detailError = null;
    try {
      const [m, met, cs] = await Promise.all([
        client.tools.describe<ToolManifest>(id),
        client.tools.metrics<ToolMetrics>(id, metricsWindow),
        client.tools.contentStats<ToolContentStats>(id)
      ]);
      manifest = m;
      metrics = met;
      contentStats = cs;
      detailStatus = 'ready';
    } catch (err) {
      manifest = null;
      metrics = null;
      contentStats = null;
      detailError = toError(err);
      detailStatus = 'error';
    }
  }

  /* ================================================================ */
  /* Event handlers                                                    */
  /* ================================================================ */

  function applyFilter(next: ToolFilter): void {
    filter = next;
    page = 1;
    activeSavedId = null;
    void loadCatalog();
  }

  function submitSearch(): void {
    applyFilter({ ...filter, search: searchText.trim() });
  }

  function clearFilters(): void {
    searchText = '';
    applyFilter({});
  }

  function changePage(next: number): void {
    page = next;
    void loadCatalog();
  }

  function changePageSize(size: number): void {
    pageSize = size;
    page = 1;
    void loadCatalog();
  }

  async function selectTool(id: string): Promise<void> {
    selectedId = id;
    approvalResult = null;
    await loadDetail(id);
  }

  async function changeMetricsWindow(w: ToolMetricsWindow): Promise<void> {
    metricsWindow = w;
    if (selectedId !== null && client !== null) {
      try {
        metrics = await client.tools.metrics<ToolMetrics>(selectedId, w);
      } catch (err) {
        detailError = toError(err);
        detailStatus = 'error';
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

  /* ---- the §13 fix: Approve / Reject call the real Protocol -------- */
  async function setApprovalPolicy(
    toolID: string,
    policy: ToolApprovalPolicy
  ): Promise<void> {
    if (client === null || !canAdmin) {
      return;
    }
    approvalPending = true;
    approvalResult = null;
    try {
      await client.tools.setApprovalPolicy(toolID, policy);
      approvalResult = {
        ok: true,
        message: `Approval policy for ${toolID} set to "${policy}".`
      };
      // Re-load the catalog + detail so the rendered policy reflects the
      // Runtime's new state (no optimistic fiction — CLAUDE.md §13).
      await loadCatalog();
      if (selectedId === toolID) {
        await loadDetail(toolID);
      }
    } catch (err) {
      const e = toError(err);
      approvalResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      approvalPending = false;
    }
  }

  /* ---- bulk: revoke OAuth bindings on the selected rows ------------ */
  let bulkPending = $state(false);
  let bulkResult = $state<{ ok: boolean; message: string } | null>(null);

  async function bulkRevokeOAuth(): Promise<void> {
    if (client === null || !canAdmin || selected.size === 0) {
      return;
    }
    bulkPending = true;
    bulkResult = null;
    const ids = [...selected];
    try {
      for (const id of ids) {
        await client.tools.revokeOAuth(id);
      }
      bulkResult = { ok: true, message: `Revoked OAuth bindings for ${ids.length} tool(s).` };
      selected = new Set<string>();
      await loadCatalog();
    } catch (err) {
      const e = toError(err);
      bulkResult = { ok: false, message: `${e.code}: ${e.message}` };
    } finally {
      bulkPending = false;
    }
  }

  function bulkExport(): void {
    const rows = tools.filter((t) => selected.has(t.id));
    triggerDownload('tools-selection.csv', 'text/csv', exportToolsCSV(rows));
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async function refreshSavedViews(): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    try {
      const records = await savedFilters.list();
      savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      savedFilterSpecs = new Map(records.map((r) => [r.id, r.filterSpec]));
    } catch {
      // A Console-local store read failing is non-fatal: the page still
      // works without saved-view chips. The chips simply stay empty.
      savedViews = [];
      savedFilterSpecs = new Map();
    }
  }

  function applySavedView(id: string): void {
    const spec = savedFilterSpecs.get(id);
    if (spec === undefined) {
      return;
    }
    filter = { ...spec };
    searchText = spec.search ?? '';
    page = 1;
    activeSavedId = id;
    void loadCatalog();
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    await savedFilters.delete(id);
    if (activeSavedId === id) {
      activeSavedId = null;
    }
    await refreshSavedViews();
  }

  let saveName = $state('');

  async function saveCurrentFilter(): Promise<void> {
    const name = saveName.trim();
    if (name.length === 0 || savedFilters === null) {
      return;
    }
    const created = await savedFilters.create(name, { ...filter });
    saveName = '';
    await refreshSavedViews();
    activeSavedId = created.id;
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  onMount(() => {
    connection = resolveConnection();
    // Detect the admin scope claim via the shared `hasScope` helper —
    // the same path the Flows page uses. CONVENTIONS.md §6 forbids a
    // `.svelte` file reading `localStorage` directly; `hasScope` is the
    // single sanctioned read of the persisted scope set (D-132 / F5).
    canAdmin = hasScope(connection, 'admin');

    if (connection === null) {
      // Disconnected — NOT an error (CONVENTIONS.md §4 state 1).
      client = null;
      status = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });

    // Wire the Console-DB-backed saved-view store (D-061). The DB open is
    // best-effort: a failure leaves the chips empty but the page works.
    void (async () => {
      try {
        const db = await openListPageDB(connection!);
        const operator = await operatorIdOf(
          connection!.identity.tenant,
          connection!.identity.user
        );
        savedFilters = new ToolsSavedFilters(db, operator);
        await refreshSavedViews();
      } catch {
        savedFilters = null;
      }
    })();

    void loadCatalog();
  });

  /* ---- catalog table column config -------------------------------- */
  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Name' },
    { key: 'version', label: 'Version' },
    { key: 'scope', label: 'Scope' },
    { key: 'transport', label: 'Transport' },
    { key: 'oauth', label: 'OAuth' },
    { key: 'approval', label: 'Approval' },
    { key: 'reliability', label: 'Reliability' },
    { key: 'last_used', label: 'Last used' },
    { key: 'owner', label: 'Owner' }
  ];

  function rowKey(r: unknown): string {
    return (r as Tool).id;
  }

  function lastUsed(iso: string): string {
    if (!iso || iso.startsWith('0001-01-01')) {
      return 'never';
    }
    const then = new Date(iso).getTime();
    if (Number.isNaN(then)) {
      return 'never';
    }
    const deltaMin = Math.round((Date.now() - then) / 60000);
    if (deltaMin < 1) return 'just now';
    if (deltaMin < 60) return `${deltaMin}m ago`;
    const deltaHr = Math.round(deltaMin / 60);
    if (deltaHr < 24) return `${deltaHr}h ago`;
    return `${Math.round(deltaHr / 24)}d ago`;
  }

  function oauthKind(s: ToolOAuthStatus): StatusKind {
    if (s === 'Bound') return 'success';
    if (s === 'Required') return 'accent';
    if (s === 'Expired') return 'danger';
    return 'neutral';
  }

  function approvalKind(p: ToolApprovalPolicy): StatusKind {
    if (p === 'auto') return 'success';
    if (p === 'gated') return 'warning';
    return 'danger';
  }
</script>

<svelte:head>
  <title>Tools · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="tools-page">
  <PageHeader title="Tools" subtitle="Registered tool catalog · runtime lens" />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeSavedId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <input
        class="control save-input"
        type="text"
        placeholder="Save current as…"
        bind:value={saveName}
        data-testid="tools-save-filter-name"
        disabled={savedFilters === null || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && void saveCurrentFilter()}
      />
      <button
        type="button"
        class="control"
        data-testid="tools-save-filter"
        disabled={savedFilters === null || saveName.trim().length === 0 || disconnected}
        title={disconnected
          ? DISCONNECTED_TOOLTIP
          : savedFilters === null
            ? 'Console-local saved-view store unavailable'
            : undefined}
        onclick={() => void saveCurrentFilter()}
      >
        Save view
      </button>
    {/snippet}

    {#snippet facets()}
      <ToolFacetChips {filter} onfilter={applyFilter} />
    {/snippet}

    {#snippet search()}
      <input
        class="control search-input"
        type="search"
        placeholder="Search tools…"
        bind:value={searchText}
        data-testid="tools-search"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && submitSearch()}
      />
      <button
        type="button"
        class="control"
        data-testid="tools-search-apply"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={submitSearch}
      >
        Apply
      </button>
      <button
        type="button"
        class="control"
        data-testid="tools-filter-clear"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={clearFilters}
      >
        Clear
      </button>
    {/snippet}

    {#snippet actions()}
      <button
        type="button"
        class="control"
        data-testid="tools-refresh"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => void loadCatalog()}
      >
        Refresh
      </button>
      <button
        type="button"
        class="control"
        data-testid="tools-export-csv"
        disabled={tools.length === 0 || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => handleExport('csv')}
      >
        Export CSV
      </button>
      <button
        type="button"
        class="control"
        data-testid="tools-export-json"
        disabled={tools.length === 0 || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => handleExport('json')}
      >
        Export JSON
      </button>
    {/snippet}
  </FilterBar>

  <BulkActionBar count={selected.size} onclear={() => (selected = new Set<string>())}>
    {#snippet actions()}
      <button
        type="button"
        class="control"
        data-testid="tools-bulk-revoke-oauth"
        disabled={!canAdmin || bulkPending}
        title={canAdmin
          ? undefined
          : 'Requires the admin scope claim — tools.revoke_oauth is an admin Protocol method (D-079).'}
        onclick={() => void bulkRevokeOAuth()}
      >
        Revoke OAuth bindings
      </button>
      <button
        type="button"
        class="control"
        data-testid="tools-bulk-export"
        onclick={bulkExport}
      >
        Export selection
      </button>
    {/snippet}
  </BulkActionBar>
  {#if bulkResult !== null}
    <p
      class="inline-result"
      class:ok={bulkResult.ok}
      class:err={!bulkResult.ok}
      data-testid="tools-bulk-result"
    >
      {bulkResult.message}
    </p>
  {/if}

  <div class="layout">
    <div class="main-col">
      <PageState status={status} error={pageError} onretry={() => void loadCatalog()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="tools-catalog-empty">
            <p class="headline">No tools match the current view</p>
            <p class="detail">
              No tools registered, or the active filters yield zero rows.
            </p>
            <button type="button" class="control" onclick={clearFilters}>
              Clear filters
            </button>
          </div>
        {/snippet}

        <DataTable
          columns={COLUMNS}
          rows={tools}
          {rowKey}
          selectable
          {selected}
          onselectionchange={(s) => (selected = s)}
          onrowclick={(r) => void selectTool((r as Tool).id)}
        >
          {#snippet row(r)}
            {@const t = r as Tool}
            <td data-testid="tools-catalog-row" class:row-active={t.id === selectedId}>
              {t.name}
            </td>
            <td>{t.version || '—'}</td>
            <td>{t.scope}</td>
            <td><StatusChip kind="neutral" label={t.transport} /></td>
            <td><StatusChip kind={oauthKind(t.oauth_status)} label={t.oauth_status} /></td>
            <td>
              <StatusChip kind={approvalKind(t.approval_policy)} label={t.approval_policy} />
            </td>
            <td>{t.reliability_tier}</td>
            <td>{lastUsed(t.last_used_at)}</td>
            <td>{t.owner || '—'}</td>
          {/snippet}
        </DataTable>
      </PageState>

      {#if status === 'ready' || status === 'empty'}
        <Pagination
          page={page}
          pageSize={pageSize}
          total={totalRows}
          onpage={changePage}
          onpagesize={changePageSize}
        />
      {/if}

      {#if !disconnected}
        <!-- N5 fix (Phase 83r): the disconnected `<PageState>` branch
             already renders the "Not connected" placeholder. The
             secondary `ToolDetailTabs` empty ("Select a tool from the
             catalog…") was stacking BELOW that, producing the
             post-83k walkthrough's two-empties bug. Collapsing the
             tabs while disconnected leaves ONE empty message. -->
        <ToolDetailTabs
          tool={selectedTool}
          {manifest}
          loading={detailStatus === 'loading'}
          {canAdmin}
          {approvalPending}
          {approvalResult}
          onsetpolicy={(id, policy) => void setApprovalPolicy(id, policy)}
        />
      {/if}
      {#if selectedTool !== null}
        <!-- D-132 / W4: a `tools.invoke` Protocol method is NOT shipped
             at V1. Rather than silently omit a "try the tool" surface,
             the page renders a disabled-with-tooltip affordance that
             names the deferral (CONVENTIONS.md §5, CLAUDE.md §13). The
             V1 deferral is recorded in docs/design/console/page-tools.md
             §3 + D-132. -->
        <button
          type="button"
          class="action deferred"
          data-testid="tools-try-tool"
          disabled
          aria-disabled="true"
          title="Try this tool — needs a tools.invoke Protocol method (post-V1, D-132)"
        >
          Try this tool
        </button>
      {/if}
      {#if detailStatus === 'error' && detailError !== null}
        <p class="inline-result err" data-testid="tools-detail-error">
          {detailError.code}: {detailError.message}
        </p>
      {/if}
    </div>

    <DetailRail>
      <RailCard title="Tool overview">
        <ToolOverviewCard {aggregates} {disconnected} />
      </RailCard>
      <RailCard title="Status & error rate">
        <StatusErrorRateCard
          {metrics}
          window={metricsWindow}
          onwindow={(w) => void changeMetricsWindow(w)}
        />
      </RailCard>
      <RailCard title="Content size & display mode">
        <ContentSizeCard stats={contentStats} />
      </RailCard>
      <RailCard title="Run history">
        <RunHistoryStrip tool={selectedTool} {metrics} />
      </RailCard>
    </DetailRail>
  </div>
</div>

<style>
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-6);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .search-input {
    flex: 1;
    min-width: var(--size-search-min);
  }

  .save-input {
    width: var(--size-search-min);
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .empty-block .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-block .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .row-active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }

  .action {
    align-self: flex-start;
    font-size: var(--text-sm);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
  }

  .action.deferred {
    opacity: 0.5;
    cursor: not-allowed;
  }
</style>
