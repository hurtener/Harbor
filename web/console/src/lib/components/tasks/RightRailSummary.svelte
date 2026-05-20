<script lang="ts">
  // Harbor Console — Tasks page right-rail Summary card body (Phase 73d
  // / D-123). Renders the filtered-view per-status aggregate counters.
  // Page-specific; lives in `components/tasks/`. Design tokens only.
  import type { TaskListAggregates } from '$lib/protocol/tasks.js';

  let { aggregates }: { aggregates: TaskListAggregates } = $props();

  const rows = $derived([
    { label: 'Pending', value: aggregates.pending },
    { label: 'Running', value: aggregates.running },
    { label: 'Paused', value: aggregates.paused },
    { label: 'Failed', value: aggregates.failed },
    { label: 'Complete', value: aggregates.complete },
    { label: 'Cancelled', value: aggregates.cancelled }
  ]);
</script>

<dl class="summary" data-testid="rail-summary">
  {#each rows as r (r.label)}
    <div><dt>{r.label}</dt><dd>{r.value}</dd></div>
  {/each}
</dl>

<style>
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
  }

  .summary div {
    display: flex;
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
  }
</style>
