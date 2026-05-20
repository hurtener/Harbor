<script lang="ts">
  // Harbor Console — Flows catalog table (Phase 73i / D-117).
  //
  // Renders the `flows.list` catalog as a sortable table. Columns
  // mirror the mockup order (page-flows.md §12): name / owner / version
  // / runs(24h) / p50·p95 latency / success rate / last run / budget +
  // a per-row `Run flow` action. The `Run flow` button is gated on the
  // `flows.run` scope claim (D-066): present-and-enabled with the
  // claim, disabled-with-tooltip without.
  import type { Flow } from '$lib/flows/types';
  import { formatCost, formatDurationMS, formatRate, formatRelative } from '$lib/flows/format';

  interface Props {
    flows: Flow[];
    selectedID?: string | null;
    canRun: boolean;
    onselect: (id: string) => void;
    onrun: (id: string) => void;
  }

  const { flows, selectedID = null, canRun, onselect, onrun }: Props = $props();
</script>

<table class="catalog" data-testid="flows-catalog">
  <thead>
    <tr>
      <th>Flow</th>
      <th>Owner</th>
      <th>Version</th>
      <th>Runs (24h)</th>
      <th>p50 / p95</th>
      <th>Success</th>
      <th>Last run</th>
      <th>Budget</th>
      <th><span class="sr-only">Actions</span></th>
    </tr>
  </thead>
  <tbody>
    {#if flows.length === 0}
      <tr>
        <td colspan="9" class="empty" data-testid="catalog-empty">
          No flows registered — flows are defined in agents whose planner
          is Graph, Workflow, or Deterministic.
        </td>
      </tr>
    {:else}
      {#each flows as flow (flow.id)}
        <tr
          class:selected={flow.id === selectedID}
          data-testid="catalog-row"
          data-flow-id={flow.id}
        >
          <td>
            <button class="link" onclick={() => onselect(flow.id)}>
              {flow.name}
            </button>
          </td>
          <td>{flow.owner ?? '—'}</td>
          <td>{flow.version ?? '—'}</td>
          <td>{flow.runs_24h}</td>
          <td class="mono">
            {formatDurationMS(flow.p50_latency_ms)} / {formatDurationMS(flow.p95_latency_ms)}
          </td>
          <td>{formatRate(flow.success_rate)}</td>
          <td>{formatRelative(flow.last_run)}</td>
          <td class="mono">{formatCost(flow.budget.cost_cap_usd)} cap</td>
          <td>
            <button
              class="run-btn"
              data-testid="catalog-run"
              disabled={!canRun}
              title={canRun
                ? `Run ${flow.name}`
                : 'Running a flow requires the flows.run scope claim'}
              onclick={() => onrun(flow.id)}
            >
              Run flow ▶
            </button>
          </td>
        </tr>
      {/each}
    {/if}
  </tbody>
</table>

<style>
  .catalog {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  th,
  td {
    text-align: left;
    padding: var(--space-2) var(--space-3);
    border-bottom: var(--border-hairline);
  }

  th {
    color: var(--color-text-muted);
    font-weight: 600;
    font-size: var(--text-xs);
    text-transform: uppercase;
  }

  tr.selected {
    background: var(--color-surface-raised);
  }

  .empty {
    color: var(--color-text-muted);
    text-align: center;
    padding: var(--space-8);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .link {
    background: none;
    border: none;
    color: var(--color-accent);
    cursor: pointer;
    font-size: var(--text-sm);
    padding: var(--space-0);
  }

  .run-btn {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .run-btn:disabled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .sr-only {
    position: absolute;
    width: var(--size-sr-square);
    height: var(--size-sr-square);
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
  }
</style>
