<script lang="ts">
  // ToolOverviewCard — the Tools-page right-rail aggregate-counter
  // content (page-tools.md §12). Tools-specific content; the calling
  // page wraps it in the shared `ui/RailCard`, so this component emits
  // only the card BODY — no card chrome (D-121, CONVENTIONS.md §3).
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { ToolAggregates } from '$lib/protocol/tools.js';

  let { aggregates }: { aggregates: ToolAggregates } = $props();
</script>

<dl data-testid="tools-overview-card">
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

<style>
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
