<script lang="ts">
  // Harbor Console — Live Runtime Health tab.
  //
  // Phase 108d: the tab is real-wired to the runtime's advertised
  // capability set (`capabilities()` → `runtime_health`) rather than a
  // hardcoded "coming in Phase NN" placeholder. The typed client exposes
  // no health method yet, so when the capability is NOT advertised the tab
  // renders an honest "this runtime does not advertise health" info state.
  // When it IS advertised but the Console build has no renderer wired, the
  // tab says so honestly (no fabricated board). This keeps the page an
  // honest Protocol client (CLAUDE.md §13 — no silent degradation, no
  // fabrication).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import HeartPulse from '@lucide/svelte/icons/heart-pulse';

  let { available = false }: { available?: boolean } = $props();
</script>

<div class="health-empty" data-testid="live-runtime-health-empty">
  <HeartPulse size={32} aria-hidden="true" />
  {#if available}
    <p class="headline">Health not yet rendered in this Console</p>
    <p class="detail">
      This runtime advertises a health surface, but this Console build does not
      yet render the health board. No data is fabricated.
    </p>
  {:else}
    <p class="headline">This runtime does not advertise health</p>
    <p class="detail">
      The connected runtime does not expose a health capability. Runtime health
      (uptime, queue depth, driver status) appears here on runtimes that
      advertise one.
    </p>
  {/if}
</div>

<style>
  .health-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
    color: var(--color-text-muted);
  }

  .health-empty .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .health-empty .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    max-width: var(--size-search-min);
  }
</style>
