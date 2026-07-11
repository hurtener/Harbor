<script lang="ts">
  // Harbor Console — Flow detail page (`/flows/<flow_id>`) — Phase 108p rebuild
  // (D-188; supersedes the Phase 73i / D-117 pre-chrome layout).
  //
  // The detail-mode view for one selected flow: a carded action row (replacing
  // the per-page header — 108b chrome) + the full-width read-only engine graph
  // canvas + the run-history table + a `DetailRail` carrying the per-flow Budget
  // meter and the selected-run summary. View-only (D-063) — the only mutating
  // action is `Run this flow ▶`, admin-gated (D-066 / D-079).
  //
  // Unlike the other Phase 108 pages, the flow detail stays its own ROUTE rather
  // than collapsing into a rail: the DAG canvas needs full width and does not
  // fit a `--size-rail` rail (D-188 deviation 1). The shell is still carded +
  // viewport-locked (PAGE-POLISH §6).
  //
  // # Console consistency (CONVENTIONS.md / PAGE-POLISH-PROCEDURE.md)
  //
  // - Route under `(console)/flows/[flow_id]` — the `[flow_id]` segment is
  //   grandfathered (§1); no `/console/` URL prefix. Renders inside the shell.
  // - Composes the `ui/` inventory: `DetailRail`/`RailCard`, `StatusChip`,
  //   `PageState` (§3/§4). The Flows-specific `EngineGraphCanvas`, `BudgetMeter`,
  //   `RunHistoryTable`, `RunSummaryPanel`, `RunFlowModal`, `CompareVersions`
  //   stay in their dirs (§3).
  // - King-file refactor: the `FlowDetailState` controller owns the reactive
  //   state + the Protocol calls (§6) — no hand-rolled `fetch`. Tokens only (§7).
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { DetailRail, RailCard, StatusChip, PageState } from '$lib/components/ui';
  import BudgetMeter from '$lib/components/flows/BudgetMeter.svelte';
  import RunHistoryTable from '$lib/components/flows/RunHistoryTable.svelte';
  import RunSummaryPanel from '$lib/components/flows/RunSummaryPanel.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import CompareVersions from '$lib/components/flows/CompareVersions.svelte';
  import EngineGraphCanvas from '$lib/components/graph/EngineGraphCanvas.svelte';
  import { FlowDetailState } from '$lib/flows/detail.svelte.js';
  import type { FlowsProtocol } from '$lib/protocol/flows.js';

  let { flows: injectedFlows }: { flows?: FlowsProtocol } = $props();

  const detail = new FlowDetailState();
  const flowID = $derived(page.params.flow_id ?? '');

  // Local bindings for the run-history date-range filter inputs (D-295).
  // The controller owns the applied bounds; these hold the in-progress
  // picker values until Apply.
  let sinceDate = $state('');
  let untilDate = $state('');

  // Re-load whenever the route's flow id changes (client-side nav between flows).
  $effect(() => {
    detail.load(flowID, injectedFlows);
  });

  /**
   * Double-click a tool node → the Tools page. The Console does not yet mount a
   * `/tools/[id]` detail route (only the `/tools` list exists); navigating to
   * the list is the real, non-404 destination. When the tool detail route lands,
   * this becomes `/tools/<descriptor>`.
   */
  function activateNode(nodeID: string): void {
    if (detail.toolDescriptorFor(nodeID) !== null) {
      void goto('/tools');
    }
  }
</script>

<svelte:head>
  <title>{flowID} · Flows · Harbor Console</title>
</svelte:head>

<section class="flow-detail" data-testid="flow-detail-page">
  <section class="panel card action-card">
    <div class="title-block">
      <a class="back" href="/flows" data-testid="flow-detail-back">← All flows</a>
      <div class="title-text">
        <h1>{detail.description?.flow.name ?? flowID}</h1>
        <p class="subtitle">
          {detail.description?.flow.version
            ? `Version ${detail.description.flow.version}`
            : 'Flow detail — view-only (D-063).'}
        </p>
      </div>
    </div>
    {#if detail.status === 'ready'}
      <div class="action-row">
        <StatusChip kind={detail.health.kind} label={detail.health.label} />
        <button
          type="button"
          class="primary"
          data-testid="detail-run"
          disabled={!detail.canRun}
          title={detail.canRun
            ? 'Run this flow'
            : 'Running a flow requires the admin scope claim (flows.run — D-079)'}
          onclick={() => detail.openRun()}
        >
          Run this flow ▶
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="detail-save-snapshot"
          onclick={() => detail.saveSnapshot()}
        >
          Save snapshot
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="detail-compare"
          onclick={() => detail.openCompare()}
        >
          Compare versions
        </button>
      </div>
    {/if}
  </section>

  <PageState status={detail.status} error={detail.loadError} onretry={() => detail.refresh()}>
    {#snippet empty()}
      <div class="empty-block">
        <p class="empty-headline">Flow not found</p>
        <p class="empty-detail">No flow is registered under the id <code>{flowID}</code>.</p>
        <a class="bar-action" href="/flows">← All flows</a>
      </div>
    {/snippet}

    {#if detail.description}
      <CompareVersions
        open={detail.compareOpen}
        base={detail.snapshot}
        head={detail.description}
        onclose={() => detail.closeCompare()}
      />

      <div class="layout">
        <section class="panel card primary-col">
          <div class="graph-wrap">
            <EngineGraphCanvas
              graph={detail.graphInput}
              selectedNodeID={detail.selectedNodeId}
              onselect={(id) => detail.selectNode(id)}
              onactivate={activateNode}
            />
            {#if detail.description.source}
              <p class="source" data-testid="flow-source">
                Source: <code>{detail.description.source}</code>
              </p>
            {/if}
          </div>

          <div class="run-filter" data-testid="run-filter">
            <span class="run-filter-label">Run history</span>
            <label class="run-filter-field">
              <span>From</span>
              <input
                type="date"
                data-testid="run-filter-since"
                bind:value={sinceDate}
                max={untilDate || undefined}
              />
            </label>
            <label class="run-filter-field">
              <span>To</span>
              <input
                type="date"
                data-testid="run-filter-until"
                bind:value={untilDate}
                min={sinceDate || undefined}
              />
            </label>
            <button
              type="button"
              class="bar-action"
              data-testid="run-filter-apply"
              onclick={() => detail.applyRunFilter(sinceDate, untilDate)}
            >
              Apply
            </button>
            {#if detail.runsFilterActive}
              <button
                type="button"
                class="bar-action"
                data-testid="run-filter-clear"
                onclick={() => {
                  sinceDate = '';
                  untilDate = '';
                  detail.clearRunFilter();
                }}
              >
                Clear
              </button>
            {/if}
          </div>

          <RunHistoryTable
            runs={detail.runs}
            selectedRunID={detail.selectedRunId}
            onselect={(id) => void detail.selectRun(id)}
          />
        </section>

        <DetailRail>
          <RailCard title="Per-flow budget">
            <BudgetMeter
              budget={detail.description.flow.budget}
              consumption={detail.description.budget_consumption}
            />
          </RailCard>
          <RailCard title="Run summary">
            <PageState
              status={detail.runSummaryStatus}
              error={detail.runSummaryError}
              onretry={() => detail.selectedRunId && void detail.selectRun(detail.selectedRunId)}
            >
              {#snippet empty()}
                <p class="rail-empty" data-testid="run-summary-empty">
                  Select a run from the history to see its per-node trace.
                </p>
              {/snippet}
              <!-- `onopensession` / `onopenartifact` are intentionally NOT
                   passed: the Console does not yet mount a `/sessions/[id]` or
                   `/artifacts/[id]` detail route. `RunSummaryPanel` renders
                   those affordances disabled-with-tooltip rather than linking to
                   a 404 (CONVENTIONS.md §5). They wire up when the routes land. -->
              <RunSummaryPanel description={detail.runSummary} />
            </PageState>
          </RailCard>
        </DetailRail>
      </div>
    {/if}
  </PageState>
</section>

<RunFlowModal
  {flowID}
  open={detail.runOpen}
  pending={detail.runPending}
  errorMessage={detail.runError}
  onsubmit={(inputs) => void detail.submitRun(inputs)}
  oncancel={() => detail.cancelRun()}
/>

<style>
  /* Viewport-locked: only the left column + the right rail scroll internally
     (PAGE-POLISH §6). The graph canvas keeps full width (D-188). */
  .flow-detail {
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

  .action-card {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-shrink: 0;
    flex-wrap: wrap;
  }

  .title-block {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-width: 0;
  }

  .back {
    color: var(--color-accent);
    font-size: var(--text-sm);
    text-decoration: none;
    white-space: nowrap;
  }

  .title-text {
    min-width: 0;
  }

  .title-text h1 {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .subtitle {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .action-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .run-filter {
    display: flex;
    align-items: flex-end;
    gap: var(--space-2);
    flex-wrap: wrap;
    margin-bottom: var(--space-2);
  }

  .run-filter-label {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
    margin-right: auto;
  }

  .run-filter-field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .run-filter-field input {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .primary-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    min-width: 0;
    overflow: auto;
  }

  .graph-wrap {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .source {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin: var(--space-0);
  }

  code {
    font-family: var(--font-mono);
  }

  .layout :global(.detail-rail) {
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
  }

  .primary {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .primary:disabled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
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
  }

  .rail-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
