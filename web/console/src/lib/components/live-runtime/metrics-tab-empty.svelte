<script lang="ts">
  // Harbor Console — Live Runtime Metrics tab.
  //
  // Phase 108d: the tab is real-wired to the runtime's advertised
  // capability set (`capabilities()` → `metrics_snapshot`) rather than a
  // hardcoded "coming in Phase NN" placeholder. The typed client exposes
  // no metrics method yet, so when the capability is NOT advertised the
  // tab renders an honest "this runtime does not advertise metrics" info
  // state. When it IS advertised but the Console build has no renderer
  // wired, the tab says so honestly (no fabricated tiles). This keeps the
  // page an honest Protocol client (CLAUDE.md §13 — no silent degradation,
  // no fabrication).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import Gauge from '@lucide/svelte/icons/gauge';

  let { available = false }: { available?: boolean } = $props();
</script>

<div class="metrics-empty" data-testid="live-runtime-metrics-empty">
  <Gauge size={32} aria-hidden="true" />
  {#if available}
    <p class="headline">Metrics not yet rendered in this Console</p>
    <p class="detail">
      This runtime advertises a metrics surface, but this Console build does
      not yet render live metric tiles. No data is fabricated.
    </p>
  {:else}
    <p class="headline">This runtime does not advertise metrics</p>
    <p class="detail">
      The connected runtime does not expose a metrics capability. Live metric
      tiles (tokens/sec, cost rate, tool latency) appear here on runtimes that
      advertise one.
    </p>
  {/if}
</div>

<style>
  .metrics-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
    color: var(--color-text-muted);
  }

  .metrics-empty .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .metrics-empty .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    max-width: var(--size-search-min);
  }
</style>
