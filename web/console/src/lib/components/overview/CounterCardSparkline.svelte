<script lang="ts">
  // Harbor Console — Overview counter-card sparkline (Phase 73a / 108c).
  //
  // A mini bar-sparkline rendered inside a `<CounterCard>`. It takes a raw
  // numeric series (`values`) — for Events/min this is the windowed event-rate
  // fold (`aggregations.ts`); for the snapshot gauges (tasks / background jobs /
  // MCP) it is a client-side ring buffer of `runtime.counters` samples taken
  // while the page is open (real sampled data — NOT fabricated). A quiet / flat
  // window renders floor bars rather than an empty box ("the runtime is up but
  // quiet" — page-overview.md §12).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  let {
    values,
    label
  }: {
    /** The raw numeric series (oldest → newest). */
    values: number[];
    /** Accessible label describing the trend. */
    label?: string;
  } = $props();

  const peak = $derived(values.reduce((m, v) => (v > m ? v : m), 0));

  function heightPct(v: number): number {
    if (peak <= 0) return 0;
    return (v / peak) * 100;
  }
</script>

<div
  class="sparkline"
  data-testid="counter-sparkline"
  role="img"
  aria-label={label ?? `Trend, peak ${peak}`}
>
  {#each values as v, i (i)}
    <span class="bar" style:height={`${Math.max(heightPct(v), 6)}%`} title={`${v}`}></span>
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
    background: var(--color-accent);
    opacity: 0.55;
    border-radius: var(--radius-sm);
  }
</style>
