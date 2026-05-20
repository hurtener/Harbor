<script lang="ts">
  // ToolOverviewCard — the right-rail top card (page-tools.md §12):
  // aggregate counters for the currently-filtered catalog view.
  // Svelte 5 runes mode (D-092).
  import type { ToolAggregates } from '$lib/protocol/tools.js';

  let { aggregates }: { aggregates: ToolAggregates } = $props();
</script>

<section class="card" data-testid="tools-overview-card">
  <h3>Tool overview</h3>
  <dl>
    <div class="metric">
      <dt>Total tools</dt>
      <dd data-testid="tools-overview-total">{aggregates.total}</dd>
    </div>
    <div class="metric">
      <dt>Active</dt>
      <dd data-testid="tools-overview-active">{aggregates.active}</dd>
    </div>
    <div class="metric">
      <dt>Pending approval</dt>
      <dd class:warn={aggregates.pending_approval > 0}>{aggregates.pending_approval}</dd>
    </div>
    <div class="metric">
      <dt>Awaiting OAuth</dt>
      <dd class:warn={aggregates.awaiting_oauth > 0}>{aggregates.awaiting_oauth}</dd>
    </div>
  </dl>
</section>

<style>
  .card {
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  h3 {
    margin: var(--space-0) var(--space-0) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--border-width-thin);
  }

  dl {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-3);
    margin: var(--space-0);
  }

  .metric {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  dd {
    margin: var(--space-0);
    font-size: var(--text-xl);
    color: var(--color-text);
  }

  dd.warn {
    color: var(--color-warning);
  }
</style>
