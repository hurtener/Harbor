<script lang="ts">
  // Harbor Console — per-flow run history table (Phase 73i / D-117).
  //
  // Renders `flows.runs.list` rows: run id (short hash) / started /
  // duration / status / trigger / cost. Clicking a row surfaces the
  // run's trace in the RunSummaryPanel. NO "Convert to evaluation"
  // affordance — D-064 (Evaluations) is post-V1.
  import type { FlowRun } from '$lib/flows/types';
  import { formatCost, formatDurationMS, formatRelative, shortRunID } from '$lib/flows/format';

  interface Props {
    runs: FlowRun[];
    selectedRunID?: string | null;
    onselect: (runID: string) => void;
  }

  const { runs, selectedRunID = null, onselect }: Props = $props();
</script>

<section class="run-history" data-testid="run-history">
  <h3>Run history</h3>
  <table>
    <thead>
      <tr>
        <th>Run ID</th>
        <th>Started</th>
        <th>Duration</th>
        <th>Status</th>
        <th>Trigger</th>
        <th>Cost</th>
      </tr>
    </thead>
    <tbody>
      {#if runs.length === 0}
        <tr>
          <td colspan="6" class="empty" data-testid="run-history-empty">
            This flow has not been run yet.
          </td>
        </tr>
      {:else}
        {#each runs as run (run.run_id)}
          <tr
            class:selected={run.run_id === selectedRunID}
            data-testid="run-history-row"
            data-run-id={run.run_id}
            role="button"
            tabindex="0"
            onclick={() => onselect(run.run_id)}
            onkeydown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onselect(run.run_id);
              }
            }}
          >
            <td class="mono">{shortRunID(run.run_id)}</td>
            <td>{formatRelative(run.started_at)}</td>
            <td>{formatDurationMS(run.duration_ms)}</td>
            <td>
              <span class={`status status-${run.status}`}>{run.status}</span>
            </td>
            <td>{run.trigger}</td>
            <td class="mono">{formatCost(run.cost_usd)}</td>
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</section>

<style>
  .run-history {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h3 {
    font-size: var(--text-sm);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  table {
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
    font-size: var(--text-xs);
    text-transform: uppercase;
  }

  tr.selected {
    background: var(--color-surface-raised);
  }

  tbody tr[role='button'] {
    cursor: pointer;
  }

  .empty {
    color: var(--color-text-muted);
    text-align: center;
    padding: var(--space-6);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .status {
    font-size: var(--text-xs);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .status-succeeded {
    background: var(--color-success);
    color: var(--color-bg);
  }

  .status-failed {
    background: var(--color-danger);
    color: var(--color-bg);
  }

  .status-running {
    background: var(--color-accent);
    color: var(--color-bg);
  }

  .status-cancelled {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
  }
</style>
