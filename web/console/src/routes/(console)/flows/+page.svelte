<script lang="ts">
  // Harbor Console — Flows catalog page (Phase 73i / D-117).
  //
  // The list-mode view: a searchable, sortable catalog of registered
  // graph-family flows (`flows.list`) + the Flow Metrics card for the
  // hovered/selected flow. Selecting a flow row navigates to the detail
  // route `/flows/<flow_id>`. The page is VIEW-ONLY (D-063) — the only
  // mutating action is `Run flow`, gated on the `flows.run` scope claim.
  import { goto } from '$app/navigation';
  import CatalogTable from '$lib/components/flows/CatalogTable.svelte';
  import FlowMetricsCard from '$lib/components/flows/FlowMetricsCard.svelte';
  import RunFlowModal from '$lib/components/flows/RunFlowModal.svelte';
  import { flowsClientFromConnection, hasRunScope } from '$lib/flows/connection';
  import { FlowsClientError } from '$lib/flows/client';
  import type { Flow, FlowMetrics } from '$lib/flows/types';

  const client = flowsClientFromConnection();
  const canRun = hasRunScope();

  let flows = $state<Flow[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let query = $state('');
  let selectedID = $state<string | null>(null);
  let metrics = $state<FlowMetrics | null>(null);

  // Run-flow modal state.
  let runFlowID = $state<string | null>(null);
  let runPending = $state(false);
  let runError = $state<string | null>(null);

  const filtered = $derived.by(() => {
    const q = query.trim().toLowerCase();
    if (!q) {
      return flows;
    }
    return flows.filter(
      (f) =>
        f.name.toLowerCase().includes(q) ||
        (f.owner ?? '').toLowerCase().includes(q),
    );
  });

  async function loadCatalog(): Promise<void> {
    if (!client) {
      loading = false;
      loadError = null;
      return;
    }
    loading = true;
    loadError = null;
    try {
      const resp = await client.list({ filter: {} });
      flows = resp.flows;
    } catch (err) {
      loadError =
        err instanceof FlowsClientError
          ? `${err.code}: ${err.message}`
          : 'Failed to load the flow catalog.';
    } finally {
      loading = false;
    }
  }

  async function selectFlow(id: string): Promise<void> {
    selectedID = id;
    if (!client) {
      return;
    }
    try {
      metrics = await client.metrics({ flow_id: id });
    } catch {
      metrics = null;
    }
  }

  async function submitRun(inputs: Record<string, unknown>): Promise<void> {
    if (!client || !runFlowID) {
      return;
    }
    runPending = true;
    runError = null;
    try {
      await client.run({ flow_id: runFlowID, inputs });
      runFlowID = null;
    } catch (err) {
      runError =
        err instanceof FlowsClientError
          ? `${err.code}: ${err.message}`
          : 'Failed to start the flow run.';
    } finally {
      runPending = false;
    }
  }

  $effect(() => {
    void loadCatalog();
  });
</script>

<svelte:head>
  <title>Flows · Harbor Console</title>
</svelte:head>

<section class="flows-page" data-testid="flows-page">
  <header class="page-head">
    <h1>Flows</h1>
    <input
      type="search"
      placeholder="Search flows…"
      data-testid="flows-search"
      bind:value={query}
    />
    <button class="ghost" data-testid="flows-refresh" onclick={() => void loadCatalog()}>
      Refresh
    </button>
  </header>

  {#if !client}
    <p class="state" data-testid="flows-disconnected">
      Not connected to a Harbor Runtime. Attach a Runtime to view its flows.
    </p>
  {:else if loading}
    <p class="state" data-testid="flows-loading">Loading flow catalog…</p>
  {:else if loadError}
    <p class="state error" data-testid="flows-error">{loadError}</p>
  {:else}
    <div class="grid">
      <CatalogTable
        flows={filtered}
        {selectedID}
        {canRun}
        onselect={(id) => void goto(`/flows/${encodeURIComponent(id)}`)}
        onrun={(id) => {
          runFlowID = id;
          runError = null;
        }}
      />
      <aside>
        <FlowMetricsCard {metrics} />
        {#if filtered.length > 0 && !selectedID}
          <button
            class="ghost preview-btn"
            data-testid="flows-preview-metrics"
            onclick={() => void selectFlow(filtered[0].id)}
          >
            Preview metrics for {filtered[0].name}
          </button>
        {/if}
      </aside>
    </div>
  {/if}
</section>

<RunFlowModal
  flowID={runFlowID ?? ''}
  open={runFlowID !== null}
  pending={runPending}
  errorMessage={runError}
  onsubmit={(inputs) => void submitRun(inputs)}
  oncancel={() => {
    runFlowID = null;
    runError = null;
  }}
/>

<style>
  .flows-page {
    padding: var(--space-6);
  }

  .page-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin-bottom: var(--space-4);
  }

  h1 {
    font-size: var(--text-xl);
    margin: var(--space-0);
    flex: 1;
  }

  input[type='search'] {
    background: var(--color-surface);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
  }

  .grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail-width);
    gap: var(--space-4);
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

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .preview-btn {
    margin-top: var(--space-3);
    width: 100%;
  }
</style>
