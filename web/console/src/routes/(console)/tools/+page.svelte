<script lang="ts">
  // Harbor Console — Tools page (`/tools`) — Phase 108k rebuild (D-183;
  // supersedes the Phase 73f / pre-chrome layout).
  //
  // The registered-tool-catalog browser. Phase 108k rethemes it to the
  // carded, viewport-locked Events-108h / Background-Jobs-108j composition
  // (TABLE-primary on the left + a right-rail detail on the right; the
  // table stays visible, the rail shows the selected tool's full detail or
  // the catalog-overview idle state — NOT a Tasks-style mode-switch),
  // deepens the rail to full mock fidelity, and refactors the ~847-line
  // king file into a `ToolsPageState` controller + pure `derive.ts`
  // projections + the focused `ToolCatalogTable` / `ToolDetailRail`
  // components. It drops the per-page header (the breadcrumb / ⌘K /
  // footer are app-shell chrome, 108b).
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified
  // against the validation runtime's real tool catalog):
  //   - catalog rows + the four view aggregates ← `tools.list`
  //   - per-tool Manifest / Inputs / Outputs ← `tools.describe`
  //   - per-tool Statistics (error-rate + status pill) ← `tools.metrics`
  //   - per-tool Content size & display mode ← `tools.content_stats`
  //   - Approve / Reject ← the REAL admin `tools.set_approval_policy`
  //   - bulk Revoke OAuth ← the REAL admin `tools.revoke_oauth`
  //     (both admin-scope gated, D-079; disabled-with-tooltip without the
  //     claim — never a fabricated success, §13)
  //   - Export CSV / JSON + saved-view chips ← Console-local (D-061)
  //
  // Honest states for the genuine V1 gaps (PAGE-POLISH §1): metrics /
  // content_stats are honest-empty for a never-invoked tool (the stat
  // cards say so, never a fabricated rate); the OAuth / Approval badges are
  // real from the descriptor ("n/a" when neither); recent invocations
  // stream from the Events surface (no durable read-back here); search is a
  // Console-local facet. `tools.invoke` is NOT shipped at V1 (D-132), so
  // the page surfaces no "try this tool" form.
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import { FilterBar, BulkActionBar, Pagination, PageState, SavedViewChips } from '$lib/components/ui/index.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import ToolFacetChips from '$lib/components/tools/ToolFacetChips.svelte';
  import ToolOverviewCard from '$lib/components/tools/ToolOverviewCard.svelte';
  import ToolCatalogTable from '$lib/components/tools/ToolCatalogTable.svelte';
  import ToolDetailRail from '$lib/components/tools/ToolDetailRail.svelte';
  import { ToolsPageState } from '$lib/tools/state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const state = new ToolsPageState();
  const disconnected = $derived(state.disconnected);

  onMount(() => {
    state.load(injectedClient);
    return () => state.close();
  });
</script>

<svelte:head>
  <title>Tools · Harbor Console</title>
</svelte:head>

<section class="tools-page" data-testid="tools-page">
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={state.savedViews}
          activeId={state.activeSavedId}
          onselect={(id) => state.applySavedView(id)}
          ondelete={(id) => void state.deleteSavedView(id)}
        />
        <input
          class="bar-input save-input"
          type="text"
          placeholder="Save current as…"
          bind:value={state.saveName}
          data-testid="tools-save-filter-name"
          disabled={state.savedFilters === null || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => e.key === 'Enter' && void state.saveCurrentFilter()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="tools-save-filter"
          disabled={state.savedFilters === null || state.saveName.trim().length === 0 || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : state.savedFilters === null
              ? 'Console-local saved-view store unavailable'
              : undefined}
          onclick={() => void state.saveCurrentFilter()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <ToolFacetChips filter={state.filter} onfilter={(f) => state.applyFilter(f)} />
      {/snippet}

      {#snippet search()}
        <input
          class="bar-input search-input"
          type="search"
          placeholder="Search tools…"
          bind:value={state.searchText}
          data-testid="tools-search"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => e.key === 'Enter' && state.submitSearch()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="tools-search-apply"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.submitSearch()}
        >
          Apply
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="tools-filter-clear"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.clearFilters()}
        >
          Clear
        </button>
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="tools-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.refresh()}
        >
          Refresh
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="tools-export-csv"
          disabled={state.tools.length === 0 || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.handleExport('csv')}
        >
          Export CSV
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="tools-export-json"
          disabled={state.tools.length === 0 || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.handleExport('json')}
        >
          Export JSON
        </button>
      {/snippet}
    </FilterBar>

    <BulkActionBar count={state.selected.size} onclear={() => state.setSelection(new Set())}>
      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="tools-bulk-revoke-oauth"
          disabled={!state.canAdmin || state.bulkPending}
          title={state.canAdmin
            ? undefined
            : 'Requires the admin scope claim — tools.revoke_oauth is an admin Protocol method (D-079).'}
          onclick={() => void state.bulkRevokeOAuth()}
        >
          Revoke OAuth bindings
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="tools-bulk-export"
          onclick={() => state.bulkExport()}
        >
          Export selection
        </button>
      {/snippet}
    </BulkActionBar>
    {#if state.bulkResult !== null}
      <p
        class="inline-result"
        class:ok={state.bulkResult.ok}
        class:err={!state.bulkResult.ok}
        data-testid="tools-bulk-result"
      >
        {state.bulkResult.message}
      </p>
    {/if}
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState status={state.displayStatus} error={state.pageError} onretry={() => state.refresh()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="tools-catalog-empty">
            <p class="empty-headline">No tools match the current view</p>
            <p class="empty-detail">
              No tools registered, or the active filters yield zero rows.
              Configure agents to attach tools to this runtime.
            </p>
            <button type="button" class="bar-action" onclick={() => state.clearFilters()}>
              Clear filters
            </button>
          </div>
        {/snippet}

        <div class="table-scroll">
          <ToolCatalogTable
            rows={state.tools}
            selected={state.selected}
            activeId={state.selectedId}
            onselect={(id) => void state.selectTool(id)}
            onselectionchange={(s) => state.setSelection(s)}
          />
        </div>
      </PageState>

      {#if state.displayStatus === 'ready' || state.displayStatus === 'empty'}
        <Pagination
          page={state.page}
          pageSize={state.pageSize}
          total={state.totalRows}
          onpage={(p) => state.changePage(p)}
          onpagesize={(s) => state.changePageSize(s)}
        />
      {/if}
    </section>

    <!-- Right column — the per-tool detail when a row is selected (and the
         Console is connected), else the catalog-overview idle state. Both
         fill the column and scroll INTERNALLY (PAGE-POLISH §6). The
         `!disconnected` guard keeps a disconnected page on the single idle
         overview (with `—` placeholders, not fabricated zeros) rather than
         a second empty surface (the Phase 83r N5 invariant, carried onto
         the carded layout). -->
    {#if !disconnected && state.selectedId !== null}
      <ToolDetailRail
        tool={state.selectedTool}
        manifest={state.manifest}
        metrics={state.metrics}
        contentStats={state.contentStats}
        loading={state.detailStatus === 'loading'}
        detailError={state.detailError}
        metricsWindow={state.metricsWindow}
        onwindow={(w) => void state.changeMetricsWindow(w)}
        canAdmin={state.canAdmin}
        approvalPending={state.approvalPending}
        approvalResult={state.approvalResult}
        onsetpolicy={(id, policy) => void state.setApprovalPolicy(id, policy)}
        onclose={() => state.clearSelection()}
      />
    {:else}
      <section class="panel card idle-card" data-testid="tools-rail-idle">
        <h2 class="panel-title">Tool overview</h2>
        <ToolOverviewCard aggregates={state.aggregates} {disconnected} />
        <p class="idle-hint">
          Click a tool to inspect its descriptor — schema, policy,
          statistics, content profile, provenance and run history.
        </p>
      </section>
    {/if}
  </div>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the catalog table + the right rail scroll
     internally (PAGE-POLISH §6 — the Events / Background-Jobs pattern). */
  .tools-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-title {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  /* ---- filter card (fixed height) ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  /* Kill the shared FilterBar's own vertical padding so the strip packs
     tight (the card already supplies the padding — Events-108h pattern). */
  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  /* The dense Tools facet set (Transport ×5 + Approval ×3) makes the
     default 12rem search-slot min-width overflow the card; let the search
     slot shrink so its Apply/Clear never clip (re-measured live). */
  .filter-card :global(.filter-bar .search) {
    min-width: var(--size-input-compact);
  }

  .bar-input {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .search-input {
    flex: 1;
    min-width: var(--size-input-compact);
  }

  .save-input {
    width: var(--size-input-compact);
  }

  .bar-action {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    text-decoration: none;
  }

  .bar-action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }

  /* ---- layout (fills remaining height; both columns scroll internally) ---- */
  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  /* PageState wraps the table; let its content fill + scroll. */
  .table-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .idle-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    overflow-y: auto;
  }

  .idle-hint {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--space-8);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-8) var(--space-4);
    text-align: center;
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
    max-width: var(--size-modal-width);
  }
</style>
