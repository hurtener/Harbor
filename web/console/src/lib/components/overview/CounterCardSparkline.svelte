<script lang="ts">
  // Harbor Console — Overview counter-card sparkline (Phase 73a / 108c).
  //
  // A mini LINE/area sparkline (matches the mock — not bars) rendered inside a
  // `<CounterCard>`. Takes a raw numeric series (`values`): for Events/min the
  // windowed event-rate fold; for the snapshot gauges a client-side ring buffer
  // of `runtime.counters` samples taken while the page is open — real sampled
  // data, never fabricated (procedure §1). Strokes with `currentColor` so the
  // card colours it per metric. A flat/quiet window draws a flat line on the
  // floor rather than an empty box (page-overview.md §12).
  //
  // Svelte 5 runes (D-092); design tokens only (CLAUDE.md §4.5).
  let {
    values,
    label
  }: {
    values: number[];
    label?: string;
  } = $props();

  const W = 100;
  const H = 28;

  const line = $derived.by(() => {
    if (values.length === 0) return '';
    const max = values.reduce((m, v) => (v > m ? v : m), 0) || 1;
    const n = values.length;
    return values
      .map((v, i) => {
        const x = n === 1 ? W : (i / (n - 1)) * W;
        const y = H - (v / max) * (H - 2) - 1;
        return `${x.toFixed(1)},${y.toFixed(1)}`;
      })
      .join(' ');
  });
  const area = $derived(line ? `0,${H} ${line} ${W},${H}` : '');
</script>

<svg
  class="spark"
  viewBox="0 0 100 28"
  preserveAspectRatio="none"
  role="img"
  aria-label={label ?? 'trend'}
>
  {#if line}
    <polygon class="area" points={area} />
    <polyline class="line" points={line} />
  {/if}
</svg>

<style>
  .spark {
    display: block;
    width: 100%;
    height: var(--size-sparkline-height);
    color: inherit;
  }

  .line {
    fill: none;
    stroke: currentColor;
    stroke-width: 1.5;
    stroke-linejoin: round;
    vector-effect: non-scaling-stroke;
  }

  .area {
    fill: currentColor;
    opacity: 0.12;
    stroke: none;
  }
</style>
