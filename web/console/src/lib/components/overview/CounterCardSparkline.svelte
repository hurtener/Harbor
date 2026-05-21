<script lang="ts">
  // Harbor Console — Overview counter-card sparkline (Phase 73a / D-127).
  //
  // A mini bar-sparkline rendered inside a `<CounterCard>`. It folds the
  // windowed `RateSeries` (from `$lib/overview/aggregations.ts` — pure,
  // subscription-derived; NO new Protocol method per page-overview.md
  // §12) into a row of bars. An empty / quiet window renders flat-zero
  // bars rather than an empty box — "the runtime is up but quiet"
  // (page-overview.md §12 refinement to §7).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import type { RateSeries } from '$lib/overview/aggregations.js';

  let {
    series
  }: {
    /** The windowed rate series this sparkline renders. */
    series: RateSeries;
  } = $props();

  // The bar height is a 0..1 fraction of the y-peak; a flat-zero window
  // keeps every bar at the floor so the chart never collapses.
  function heightPct(count: number): number {
    if (series.peak <= 0) {
      return 0;
    }
    return (count / series.peak) * 100;
  }
</script>

<div
  class="sparkline"
  data-testid="counter-sparkline"
  role="img"
  aria-label={`Trend over the last ${series.window}, peak ${series.peak} per minute`}
>
  {#each series.buckets as bucket (bucket.startMillis)}
    <span
      class="bar"
      style:height={`${Math.max(heightPct(bucket.count), 4)}%`}
      title={`${bucket.count} events`}
    ></span>
  {/each}
</div>

<style>
  .sparkline {
    display: flex;
    align-items: flex-end;
    gap: var(--space-1);
    height: var(--size-sparkline-height);
    width: 100%;
  }

  .bar {
    flex: 1;
    min-width: var(--size-px);
    background: var(--color-accent-soft);
    border-radius: var(--radius-sm);
  }
</style>
