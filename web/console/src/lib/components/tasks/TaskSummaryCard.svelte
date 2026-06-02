<script lang="ts">
  // Harbor Console — Tasks page right-rail per-task Summary (Phase 108i /
  // D-181). The detail-mode Summary card: status / duration / progress /
  // tools / events / cost / tokens for the selected task. The event count
  // + cost / tokens come from the run-scoped stream (the SAME subscription
  // the dock + Cost Breakdown read); status / duration / progress / tools
  // from the `tasks.list` row. `tasks.get.cost` is all-zero on the wire, so
  // cost is the live `llm.cost.recorded` rollup — never the zero field.
  // Tasks-specific. Svelte 5 runes (D-092); tokens only.
  import { StatusChip, type StatusKind } from '$lib/components/ui/index.js';
  import type { TaskRow } from '$lib/protocol/tasks.js';
  import type { RunCost } from '$lib/tasks/run-events.js';

  let {
    task,
    cost,
    eventCount
  }: {
    /** The selected task row. */
    task: TaskRow;
    /** The run-scoped token-type cost rollup. */
    cost: RunCost;
    /** The count of run events loaded so far. */
    eventCount: number;
  } = $props();

  const statusKinds: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  function durationLabel(ms: number): string {
    if (ms <= 0) return '—';
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    const rem = s % 60;
    if (m < 60) return rem > 0 ? `${m}m ${rem}s` : `${m}m`;
    return `${Math.floor(m / 60)}h ${m % 60}m`;
  }

  // The planner-emitted progress hint is in [0,1]; absent when the planner
  // emitted none — render an honest "—" (no fabricated 0% bar).
  const progressPct = $derived(
    task.progress === undefined ? null : Math.round(task.progress * 100)
  );
</script>

<dl class="summary" data-testid="task-rail-summary">
  <div><dt>Status</dt><dd><StatusChip kind={statusKinds[task.status] ?? 'neutral'} label={task.status} /></dd></div>
  <div><dt>Duration</dt><dd>{durationLabel(task.duration_ms)}</dd></div>
  <div class="progress-row">
    <dt>Progress</dt>
    <dd>
      {#if progressPct === null}
        <span class="muted">—</span>
      {:else}
        <span class="bar" aria-hidden="true"><span class="fill" style:width={`${progressPct}%`}></span></span>
        <span class="pct">{progressPct}%</span>
      {/if}
    </dd>
  </div>
  <div><dt>Tools called</dt><dd>{task.tool_count}</dd></div>
  <div><dt>Events</dt><dd>{eventCount}</dd></div>
  <div><dt>Cost</dt><dd class="mono">${cost.totalUSD.toFixed(4)}</dd></div>
  <div><dt>Tokens</dt><dd class="mono">{cost.totalTokens.toLocaleString()}</dd></div>
</dl>

<style>
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .summary > div {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .summary dt {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .summary dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
    text-align: right;
  }

  .progress-row dd {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1;
    justify-content: flex-end;
  }

  .bar {
    flex: 1;
    max-width: var(--space-12);
    height: var(--space-1);
    background: var(--color-surface-raised);
    border-radius: var(--radius-pill);
    overflow: hidden;
  }

  .fill {
    display: block;
    height: 100%;
    background: var(--color-accent);
  }

  .pct {
    font-size: var(--text-xs);
  }

  .muted {
    color: var(--color-text-muted);
    font-weight: 400;
  }

  .mono {
    font-family: var(--font-mono);
  }
</style>
