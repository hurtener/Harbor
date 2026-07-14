<script lang="ts">
  // ToolOverviewCard — the Tools-page right-rail aggregate-counter
  // content (page-tools.md §12). Tools-specific content; the calling
  // page wraps it in the shared `ui/RailCard`, so this component emits
  // only the card BODY — no card chrome (D-121, CONVENTIONS.md §3).
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { ToolAggregates } from '$lib/protocol/tools.js';

  let {
    aggregates,
    disconnected = false,
    partial = false
  }: {
    aggregates: ToolAggregates;
    /** When true, render `—` placeholders instead of raw zeros (Phase
        83r / N4). The disconnected state has no Runtime to source counts
        from; a numeric `0` would be a lie. */
    disconnected?: boolean;
    /** When true (the `tools.list` response's `aggregates_partial`), the
        annotator-backed counters (in-flight / pending-approval /
        awaiting-OAuth) are UNAVAILABLE because no per-tool annotator is
        wired (the concrete lands in Phase 178). Render "unavailable"
        rather than a real-looking 0 (D-313). `total` stays authoritative. */
    partial?: boolean;
  } = $props();

  const placeholder = '—';
  const unavailable = 'unavailable';
</script>

<dl data-testid="tools-overview-card">
  <div class="metric">
    <dt>Total tools</dt>
    <dd data-testid="tools-overview-total">
      {disconnected ? placeholder : aggregates.total}
    </dd>
  </div>
  <div class="metric">
    <!-- N12 (Phase 83x): "Active" was ambiguous — operators read it as
         "tools that have ever fired" (the catalog total minus retired
         ones). The aggregate actually counts currently in-flight tool
         invocations. Relabel to disambiguate. -->
    <dt>In-flight (now)</dt>
    <dd data-testid="tools-overview-active" class:muted={partial}>
      {disconnected ? placeholder : partial ? unavailable : aggregates.active}
    </dd>
  </div>
  <div class="metric">
    <dt>Pending approval</dt>
    <dd class:warn={!disconnected && !partial && aggregates.pending_approval > 0} class:muted={partial}>
      {disconnected ? placeholder : partial ? unavailable : aggregates.pending_approval}
    </dd>
  </div>
  <div class="metric">
    <dt>Awaiting OAuth</dt>
    <dd class:warn={!disconnected && !partial && aggregates.awaiting_oauth > 0} class:muted={partial}>
      {disconnected ? placeholder : partial ? unavailable : aggregates.awaiting_oauth}
    </dd>
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

  dd.muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
