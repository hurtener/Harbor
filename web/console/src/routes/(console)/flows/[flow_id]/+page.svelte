<script lang="ts">
  // Harbor Console — Flow detail page (`/flows/<flow_id>`), Phase 73i /
  // D-117, refactored onto the design-system foundation (D-121).
  //
  // The detail-mode view for one selected flow: `PageHeader` + the
  // read-only engine graph canvas + a `DetailRail` carrying the per-flow
  // Budget meter, run history, and the selected-run summary. View-only
  // (D-063) — the only mutating action is `Run this flow ▶`.
  //
  // # Console consistency (CONVENTIONS.md)
  //
  // - Routes under `(console)/flows/[flow_id]` — the `[flow_id]` segment
  //   is grandfathered (§1); no `/console/` URL prefix.
  // - Renders inside the app shell (§2).
  // - Composes the `ui/` inventory: `PageHeader`, `DetailRail`/`RailCard`,
  //   `StatusChip`, `PageState` (§3/§4). The Flows-specific
  //   `EngineGraphCanvas`, `BudgetMeter`, `RunHistoryTable`,
  //   `RunSummaryPanel`, `RunFlowModal`, `CompareVersions` stay in their
  //   page-specific dirs (§3).
  // - Routes async state through the four-state `<PageState>` (§4); the
  //   run-summary uses a nested rail-scoped `<PageState>`.
  // - Talks to the Runtime only through `HarborClient` + `connection.ts`
  //   (§6) — no hand-rolled `fetch`. Design tokens only (§7).
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { FlowsProtocol } from '$lib/protocol/flows.js';
  import { resolveConnection, hasScope } from '$lib/connection.js';
  import {
    PageHeader,
    DetailRail,
    RailCard,
    StatusChip,
    PageState,
    type PageStatus,
    type StatusKind
  } from '$lib/components/ui/index.js';
  import BudgetMeter from '$lib/components/flows/BudgetMeter.svelte';
  import RunHistoryTable from '$lib/components/flows/RunHistoryTable.svelte';
  import RunSummaryPanel from '$lib/components/flows/RunSummaryPanel.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import CompareVersions from '$lib/components/flows/CompareVersions.svelte';
  import EngineGraphCanvas from '$lib/components/graph/EngineGraphCanvas.svelte';
  import type {
    FlowDescription,
    FlowRun,
    FlowRunDescription
  } from '$lib/flows/types.js';
  import type { GraphInput } from '$lib/components/graph/types.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const flowsClient =
    connection !== null
      ? new FlowsProtocol(new HarborClient({ connection }))
      : null;
  const canRun = hasScope(connection, 'admin');

  const flowID = $derived(page.params.flow_id ?? '');

  // ---- Async-state model (CONVENTIONS.md §4) ----
  let status = $state<PageStatus>(connection === null ? 'disconnected' : 'loading');
  let loadError = $state<ProtocolError | null>(null);

  let description = $state<FlowDescription | null>(null);
  let runs = $state<FlowRun[]>([]);

  // ---- Run-summary rail (nested PageState) ----
  let selectedRunID = $state<string | null>(null);
  let runSummary = $state<FlowRunDescription | null>(null);
  let runSummaryStatus = $state<PageStatus>('empty');
  let runSummaryError = $state<ProtocolError | null>(null);
  let selectedNodeID = $state<string | null>(null);

  // ---- Run-flow modal ----
  let runOpen = $state(false);
  let runPending = $state(false);
  let runError = $state<string | null>(null);

  // ---- Compare-versions (Console-local diff) ----
  let compareOpen = $state(false);
  let snapshot = $state<FlowDescription | null>(null);

  /** The shared graph-canvas input, projected from the flow description. */
  const graphInput = $derived.by<GraphInput>(() => {
    if (!description) {
      return { nodes: [], edges: [] };
    }
    return {
      nodes: description.nodes.map((n) => ({
        id: n.id,
        label: n.descriptor || n.id,
        kind: n.type,
        meta: n.policy
          ? {
              retries: String(n.policy.max_retries ?? 0),
              timeout_ms: String(n.policy.timeout_ms ?? 0)
            }
          : undefined
      })),
      edges: description.edges.map((e) => ({ from: e.from, to: e.to }))
    };
  });

  const health = $derived.by<{ label: string; kind: StatusKind }>(() => {
    if (!description) {
      return { label: 'Unknown', kind: 'neutral' };
    }
    const r = description.flow.success_rate;
    if (description.flow.runs_24h === 0) {
      return { label: 'No runs', kind: 'neutral' };
    }
    if (r >= 0.95) {
      return { label: 'Healthy', kind: 'success' };
    }
    if (r >= 0.7) {
      return { label: 'Degraded', kind: 'warning' };
    }
    return { label: 'Errored', kind: 'danger' };
  });

  /** Loads the flow description + run history. */
  async function loadDetail(): Promise<void> {
    if (!flowsClient || !flowID) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    loadError = null;
    try {
      const [desc, runsResp] = await Promise.all([
        flowsClient.describe(flowID),
        flowsClient.runsList({ flow_id: flowID })
      ]);
      description = desc;
      runs = runsResp.runs;
      status = 'ready';
    } catch (err) {
      description = null;
      runs = [];
      loadError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
  }

  /** Loads `flows.runs.describe` for a selected run into the rail. */
  async function selectRun(runID: string): Promise<void> {
    selectedRunID = runID;
    if (!flowsClient) {
      return;
    }
    runSummaryStatus = 'loading';
    runSummaryError = null;
    try {
      runSummary = await flowsClient.runsDescribe(runID);
      runSummaryStatus = 'ready';
    } catch (err) {
      runSummary = null;
      runSummaryError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      runSummaryStatus = 'error';
    }
  }

  /** Submits a `flows.run` invocation from the runner modal. */
  async function submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (!flowsClient) {
      return;
    }
    runPending = true;
    runError = null;
    try {
      await flowsClient.run({ flow_id: flowID, inputs });
      runOpen = false;
      await loadDetail();
    } catch (err) {
      runError =
        err instanceof ProtocolError
          ? `${err.code}: ${err.message}`
          : 'Failed to start the flow run.';
    } finally {
      runPending = false;
    }
  }

  /**
   * Double-click a tool node → the Tools page. The Console does not yet
   * mount a `/tools/[id]` detail route (only the `/tools` list exists);
   * navigating to the list is the real, non-404 destination. When the
   * tool detail route lands, this becomes `/tools/<descriptor>`.
   */
  function activateNode(nodeID: string): void {
    const node = description?.nodes.find((n) => n.id === nodeID);
    if (node?.type === 'tool' && node.descriptor) {
      void goto('/tools');
    }
  }

  $effect(() => {
    // Re-run whenever the route's flow id changes.
    void flowID;
    void loadDetail();
  });
</script>

<svelte:head>
  <title>{flowID} · Flows · Harbor Console</title>
</svelte:head>

<section class="flow-detail" data-testid="flow-detail-page">
  <PageHeader
    title={description?.flow.name ?? flowID}
    subtitle={description?.flow.version
      ? `Version ${description.flow.version}`
      : 'Flow detail — view-only (D-063).'}
  >
    {#snippet actions()}
      <a class="back" href="/flows" data-testid="flow-detail-back">← All flows</a>
      {#if status === 'ready'}
        <StatusChip kind={health.kind} label={health.label} />
        <button
          type="button"
          class="primary"
          data-testid="detail-run"
          disabled={!canRun}
          title={canRun
            ? 'Run this flow'
            : 'Running a flow requires the flows.run scope claim'}
          onclick={() => {
            runOpen = true;
            runError = null;
          }}
        >
          Run this flow ▶
        </button>
        <button
          type="button"
          class="ghost"
          data-testid="detail-save-snapshot"
          onclick={() => {
            snapshot = description;
          }}
        >
          Save snapshot
        </button>
        <button
          type="button"
          class="ghost"
          data-testid="detail-compare"
          onclick={() => {
            compareOpen = true;
          }}
        >
          Compare versions
        </button>
      {/if}
    {/snippet}
  </PageHeader>

  <PageState {status} error={loadError} onretry={() => void loadDetail()}>
    {#snippet empty()}
      <p class="headline">Flow not found</p>
      <p class="detail">No flow is registered under the id <code>{flowID}</code>.</p>
    {/snippet}

    {#if description}
      <CompareVersions
        open={compareOpen}
        base={snapshot}
        head={description}
        onclose={() => {
          compareOpen = false;
        }}
      />

      <div class="detail-grid">
        <div class="primary-col">
          <div class="graph-wrap">
            <EngineGraphCanvas
              graph={graphInput}
              {selectedNodeID}
              onselect={(id) => {
                selectedNodeID = id;
              }}
              onactivate={activateNode}
            />
            {#if description.source}
              <p class="source" data-testid="flow-source">
                Source: <code>{description.source}</code>
              </p>
            {/if}
          </div>

          <RunHistoryTable
            {runs}
            {selectedRunID}
            onselect={(id) => void selectRun(id)}
          />
        </div>

        <DetailRail>
          <RailCard title="Per-flow budget">
            <BudgetMeter
              budget={description.flow.budget}
              consumption={description.budget_consumption}
            />
          </RailCard>
          <RailCard title="Run summary">
            <PageState
              status={runSummaryStatus}
              error={runSummaryError}
              onretry={() => selectedRunID && void selectRun(selectedRunID)}
            >
              {#snippet empty()}
                <p class="detail" data-testid="run-summary-empty">
                  Select a run from the history to see its per-node trace.
                </p>
              {/snippet}
              <!-- `onopensession` / `onopenartifact` are intentionally
                   NOT passed: the Console does not yet mount a
                   `/sessions/[id]` or `/artifacts/[id]` detail route.
                   `RunSummaryPanel` renders those affordances
                   disabled-with-tooltip rather than linking to a 404
                   (CONVENTIONS.md §5). They wire up when the routes land. -->
              <RunSummaryPanel description={runSummary} />
            </PageState>
          </RailCard>
        </DetailRail>
      </div>
    {/if}
  </PageState>
</section>

<RunFlowModal
  {flowID}
  open={runOpen}
  pending={runPending}
  errorMessage={runError}
  onsubmit={(inputs) => void submitRun(inputs)}
  oncancel={() => {
    runOpen = false;
    runError = null;
  }}
/>

<style>
  .flow-detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .back {
    color: var(--color-accent);
    font-size: var(--text-sm);
    text-decoration: none;
    align-self: center;
  }

  .detail-grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .primary-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .source {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin: var(--space-2) var(--space-0) var(--space-0);
  }

  code {
    font-family: var(--font-mono);
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

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }
</style>
