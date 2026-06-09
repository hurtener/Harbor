<script lang="ts">
  // Harbor Console — Flows catalog page (`/flows`) — Phase 108p rebuild
  // (D-188; supersedes the Phase 73i / D-117 pre-chrome layout).
  //
  // The list-mode view: a searchable catalog of registered flows (`flows.list`)
  // + the Flow Metrics card for the selected flow in a `DetailRail`. Phase 108p
  // rethemes it to the carded, viewport-locked composition (a filter card that
  // does not scroll + a TABLE-left + a right rail, both scrolling internally),
  // refactors the king file into a `FlowsListState` controller + pure
  // `derive.ts` + the focused `FlowsTable` component, and drops the per-page
  // page header (the breadcrumb / ⌘K / footer are app-shell chrome, 108b).
  //
  // A flow is a composable engine-graph DAG registered as a TOOL (D-023) and
  // invocable directly via `flows.run` — it is NOT planner-bound (the corrected
  // empty-state copy, D-188). A row-click opens the detail route (mock-faithful
  // — page-flows.md §6); the page is view-only (D-063) with `Run flow` the only
  // mutating action, admin-gated (D-066 / D-079).
  //
  // # Console consistency (CONVENTIONS.md / PAGE-POLISH-PROCEDURE.md)
  //
  // - Route under `(console)/` with no `/console/` URL prefix (§1); renders
  //   inside the app shell — sidebar, breadcrumb, footer (§2).
  // - Composes the `ui/` inventory: `FilterBar`, `SavedViewChips`, `DetailRail`/
  //   `RailCard`, `Pagination`, `PageState` (§3). The Flows-specific
  //   `FlowsTable` / `FlowMetricsCard` / `RunFlowModal` stay in their dirs (§3).
  // - Routes all async state through the four-state `<PageState>` (§4).
  // - Talks to the Runtime only through `HarborClient` + `connection.ts` via the
  //   controller (§6) — no hand-rolled `fetch`. Design tokens only (§7).
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import {
    FilterBar,
    SavedViewChips,
    DetailRail,
    RailCard,
    Pagination,
    PageState
  } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import FlowsTable from '$lib/components/flows/FlowsTable.svelte';
  import FlowMetricsCard from '$lib/components/flows/FlowMetricsCard.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import { FlowsListState } from '$lib/flows/state.svelte.js';
  import type { FlowsProtocol } from '$lib/protocol/flows.js';
  import type { FlowsSavedFilters } from '$lib/db/saved_filters_flows.js';

  let { flows: injectedFlows, savedViewStore: injectedStore }: {
    flows?: FlowsProtocol;
    savedViewStore?: FlowsSavedFilters;
  } = $props();

  const flows = new FlowsListState();
  const disconnected = $derived(flows.disconnected);

  const SKELETON_ROWS = [0, 1, 2, 3, 4];

  onMount(() => {
    flows.load(injectedFlows, injectedStore);
  });

  function openFlow(id: string): void {
    void goto(`/flows/${encodeURIComponent(id)}`);
  }
</script>

<svelte:head>
  <title>Flows · Harbor Console</title>
</svelte:head>

<section class="flows-page" data-testid="flows-page">
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={flows.savedViews}
          activeId={flows.activeViewId}
          onselect={(id) => void flows.applySavedView(id)}
          ondelete={(id) => void flows.deleteSavedView(id)}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="flows-save-view"
          disabled={flows.savedViewStore === null || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : flows.savedViewStore === null
              ? 'Connect to a Runtime to save views'
              : 'Save the current filter as a view'}
          onclick={() => void flows.saveCurrentView()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet search()}
        <input
          type="search"
          placeholder="Search flows…"
          data-testid="flows-search"
          bind:value={flows.query}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => {
            if (e.key === 'Enter') flows.applySearch();
          }}
        />
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="flows-search-apply"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => flows.applySearch()}
        >
          Apply
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="flows-clear"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => flows.clearFilters()}
        >
          Clear
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="flows-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => flows.refresh()}
        >
          Refresh
        </button>
      {/snippet}
    </FilterBar>
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState status={flows.displayStatus} error={flows.pageError} onretry={() => flows.refresh()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each SKELETON_ROWS as i (i)}<span class="skeleton-row"></span>{/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="flows-empty">
            <p class="empty-headline">No flows registered</p>
            <p class="empty-detail">
              Flows are composable engine-graph DAGs — registered as
              <strong>tools</strong> (usable by any planner) or invoked directly
              via <code>flows.run</code>. The flow engine is a general runtime
              primitive, not tied to a specific planner. Register a flow as a
              tool, then refresh.
            </p>
            <button
              type="button"
              class="bar-action"
              data-testid="flows-empty-refresh"
              disabled={disconnected}
              onclick={() => flows.refresh()}
            >
              Refresh
            </button>
          </div>
        {/snippet}

        <div class="table-scroll">
          <FlowsTable
            flows={flows.flows}
            canRun={flows.canRun}
            selectedId={flows.selectedId}
            onopen={openFlow}
            onmetrics={(id) => void flows.selectFlow(id)}
            onrun={(id) => flows.openRun(id)}
          />
        </div>
      </PageState>

      {#if flows.displayStatus === 'ready' || flows.displayStatus === 'empty'}
        <Pagination
          page={flows.page}
          pageSize={flows.pageSize}
          total={flows.totalRows}
          onpage={(p) => flows.changePage(p)}
          onpagesize={(s) => flows.changePageSize(s)}
        />
      {/if}
    </section>

    <DetailRail>
      <RailCard title="Flow metrics">
        <PageState
          status={flows.railStatus}
          error={flows.railError}
          onretry={() => flows.selectedId && void flows.selectFlow(flows.selectedId)}
        >
          {#snippet empty()}
            <p class="rail-empty" data-testid="rail-metrics-empty">
              Select a flow's <strong>Metrics</strong> to preview its sparkline
              aggregates here.
            </p>
          {/snippet}
          <FlowMetricsCard metrics={flows.metrics} />
        </PageState>
      </RailCard>
    </DetailRail>
  </div>
</section>

<RunFlowModal
  flowID={flows.runFlowId ?? ''}
  open={flows.runFlowId !== null}
  pending={flows.runPending}
  errorMessage={flows.runError}
  onsubmit={(inputs) => void flows.submitRun(inputs)}
  oncancel={() => flows.cancelRun()}
/>

<style>
  /* Viewport-locked: only the catalog table + the right rail scroll internally
     (PAGE-POLISH §6 — the Tools / Memory / Artifacts pattern). */
  .flows-page {
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

  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
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

  input[type='search'] {
    width: 100%;
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
  }

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

  .layout :global(.detail-rail) {
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
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

  .empty-detail code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .rail-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
