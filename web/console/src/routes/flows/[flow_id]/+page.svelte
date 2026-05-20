<script lang="ts">
  // Harbor Console — Flow detail page (Phase 73i / D-117).
  //
  // The detail-mode view for one selected flow: detail header + the
  // read-only engine graph canvas + the per-flow Budget meter + the
  // run-history table + the selected-run summary panel. View-only
  // (D-063) — the only mutating action is `Run this flow ▶`.
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import DetailHeader from '$lib/components/flows/DetailHeader.svelte';
  import BudgetMeter from '$lib/components/flows/BudgetMeter.svelte';
  import RunHistoryTable from '$lib/components/flows/RunHistoryTable.svelte';
  import RunSummaryPanel from '$lib/components/flows/RunSummaryPanel.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import CompareVersions from '$lib/components/flows/CompareVersions.svelte';
  import EngineGraphCanvas from '$lib/components/graph/EngineGraphCanvas.svelte';
  import { flowsClientFromConnection, hasRunScope } from '$lib/flows/connection';
  import { FlowsClientError } from '$lib/flows/client';
  import type {
    FlowDescription,
    FlowRun,
    FlowRunDescription,
  } from '$lib/flows/types';
  import type { GraphInput } from '$lib/components/graph/types';

  const client = flowsClientFromConnection();
  const canRun = hasRunScope();

  const flowID = $derived(page.params.flow_id ?? '');

  let description = $state<FlowDescription | null>(null);
  let runs = $state<FlowRun[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);

  let selectedRunID = $state<string | null>(null);
  let runSummary = $state<FlowRunDescription | null>(null);
  let selectedNodeID = $state<string | null>(null);

  // Run-flow modal state.
  let runOpen = $state(false);
  let runPending = $state(false);
  let runError = $state<string | null>(null);

  // Compare-versions (Console-local) state.
  let compareOpen = $state(false);
  let snapshot = $state<FlowDescription | null>(null);

  // The shared graph canvas input, projected from the flow description.
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
              timeout_ms: String(n.policy.timeout_ms ?? 0),
            }
          : undefined,
      })),
      edges: description.edges.map((e) => ({ from: e.from, to: e.to })),
    };
  });

  async function loadDetail(): Promise<void> {
    if (!client || !flowID) {
      loading = false;
      return;
    }
    loading = true;
    loadError = null;
    try {
      const [desc, runsResp] = await Promise.all([
        client.describe({ id: flowID }),
        client.runsList({ flow_id: flowID }),
      ]);
      description = desc;
      runs = runsResp.runs;
    } catch (err) {
      loadError =
        err instanceof FlowsClientError
          ? `${err.code}: ${err.message}`
          : 'Failed to load the flow detail.';
    } finally {
      loading = false;
    }
  }

  async function selectRun(runID: string): Promise<void> {
    selectedRunID = runID;
    if (!client) {
      return;
    }
    try {
      runSummary = await client.runsDescribe({ run_id: runID });
    } catch {
      runSummary = null;
    }
  }

  async function submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (!client) {
      return;
    }
    runPending = true;
    runError = null;
    try {
      await client.run({ flow_id: flowID, inputs });
      runOpen = false;
      await loadDetail();
    } catch (err) {
      runError =
        err instanceof FlowsClientError
          ? `${err.code}: ${err.message}`
          : 'Failed to start the flow run.';
    } finally {
      runPending = false;
    }
  }

  function activateNode(nodeID: string): void {
    const node = description?.nodes.find((n) => n.id === nodeID);
    if (node?.type === 'tool' && node.descriptor) {
      void goto(`/tools/${encodeURIComponent(node.descriptor)}`);
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
  <a class="back" href="/flows" data-testid="flow-detail-back">← All flows</a>

  {#if !client}
    <p class="state" data-testid="flow-detail-disconnected">
      Not connected to a Harbor Runtime.
    </p>
  {:else if loading}
    <p class="state" data-testid="flow-detail-loading">Loading flow…</p>
  {:else if loadError}
    <p class="state error" data-testid="flow-detail-error">{loadError}</p>
  {:else if !description}
    <p class="state" data-testid="flow-detail-notfound">Flow not found.</p>
  {:else}
    <DetailHeader
      {description}
      {canRun}
      onrun={() => {
        runOpen = true;
        runError = null;
      }}
      onsavesnapshot={() => {
        snapshot = description;
      }}
      oncompare={() => {
        compareOpen = true;
      }}
    />

    <CompareVersions
      open={compareOpen}
      base={snapshot}
      head={description}
      onclose={() => {
        compareOpen = false;
      }}
    />

    <div class="graph-row">
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
      <BudgetMeter
        budget={description.flow.budget}
        consumption={description.budget_consumption}
      />
    </div>

    <div class="runs-row">
      <RunHistoryTable {runs} {selectedRunID} onselect={(id) => void selectRun(id)} />
      <RunSummaryPanel
        description={runSummary}
        onopensession={(runID) =>
          void goto(`/sessions/${encodeURIComponent(runID)}?run=${encodeURIComponent(runID)}`)}
        onopenartifact={(artifactID) =>
          void goto(`/artifacts/${encodeURIComponent(artifactID)}`)}
      />
    </div>
  {/if}
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
    padding: var(--space-6);
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .back {
    color: var(--color-accent);
    font-size: var(--text-sm);
    text-decoration: none;
  }

  .state {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    padding: var(--space-8);
    text-align: center;
  }

  .state.error {
    color: var(--color-danger);
  }

  .graph-row {
    display: grid;
    grid-template-columns: 1fr var(--size-rail-width);
    gap: var(--space-4);
  }

  .runs-row {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
  }

  .source {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin: var(--space-2) var(--space-0) var(--space-0);
  }

  code {
    font-family: var(--font-mono);
  }
</style>
