<script lang="ts">
  // Harbor Console — Playground KPI strip (Phase 108 / D-167).
  //
  // Four tiles: Tokens (with mini-sparkline), Cost (with ceiling %),
  // p50 latency, and Status. Reads from the same stores the header
  // chips read from. Numerics use tabular-nums.
  //
  // Design tokens only; no raw literals.

  import StatusChip from '$lib/components/ui/StatusChip.svelte';

  let {
    tokenSamples,
    costUSD,
    ceilingUSD,
    turnLatencies,
    topologyInfo
  }: {
    /** Circular buffer of the last N token-delta observations. */
    tokenSamples: number[];
    /** Current cost in USD. */
    costUSD: number;
    /** The cost ceiling in USD, or null if unknown. */
    ceilingUSD: number | null;
    /** Array of turn latencies in ms. */
    turnLatencies: number[];
    /** Topology info message when the runtime has no topology view. */
    topologyInfo: { headline: string; detail: string } | null;
  } = $props();

  // Sparkline: 60-pt inline SVG.
  const SPARK_W = 60;
  const SPARK_H = 24;

  const sparkPath = $derived.by(() => {
    if (tokenSamples.length === 0) return '';
    const max = Math.max(...tokenSamples, 1);
    const min = Math.min(...tokenSamples, 0);
    const range = max - min || 1;
    const step = SPARK_W / (tokenSamples.length - 1 || 1);
    return tokenSamples
      .map((v, i) => {
        const x = i * step;
        const y = SPARK_H - ((v - min) / range) * SPARK_H;
        return `${i === 0 ? 'M' : 'L'}${x},${y}`;
      })
      .join(' ');
  });

  const p50 = $derived.by(() => {
    if (turnLatencies.length < 3) return null;
    const sorted = [...turnLatencies].sort((a, b) => a - b);
    const mid = Math.floor(sorted.length / 2);
    return sorted.length % 2 === 0
      ? (sorted[mid - 1] + sorted[mid]) / 2
      : sorted[mid];
  });

  const costPercent = $derived.by(() => {
    if (ceilingUSD === null || ceilingUSD <= 0) return null;
    return (costUSD / ceilingUSD) * 100;
  });

  const costWarning = $derived(costPercent !== null && costPercent >= 80);
</script>

<div class="kpi-strip" data-testid="kpi-strip">
  <!-- Tokens tile -->
  <div class="kpi-tile" data-testid="kpi-tokens">
    <div class="kpi-value-row">
      <span class="kpi-value tabular">
        {tokenSamples.length > 0 ? tokenSamples[tokenSamples.length - 1].toLocaleString() : '—'}
      </span>
      {#if tokenSamples.length > 1}
        <svg
          class="sparkline"
          viewBox="0 0 {SPARK_W} {SPARK_H}"
          width={SPARK_W}
          height={SPARK_H}
          aria-hidden="true"
        >
          <path d={sparkPath} fill="none" stroke="var(--color-accent)" stroke-width="1.5" />
        </svg>
      {:else}
        <span class="spark-dot" aria-hidden="true"></span>
      {/if}
    </div>
    <span class="kpi-label">Tokens</span>
  </div>

  <!-- Cost tile -->
  <div class="kpi-tile" data-testid="kpi-cost">
    <span class="kpi-value tabular">${costUSD.toFixed(4)}</span>
    {#if costPercent !== null}
      <StatusChip
        kind={costWarning ? 'warning' : 'neutral'}
        label={`${Math.round(costPercent)}% of $${ceilingUSD!.toFixed(2)} ceiling`}
      />
    {:else}
      <span class="kpi-sublabel">—</span>
    {/if}
    <span class="kpi-label">Cost</span>
  </div>

  <!-- p50 latency tile -->
  <div class="kpi-tile" data-testid="kpi-latency">
    <span class="kpi-value tabular kpi-3xl">
      {p50 !== null ? `${Math.round(p50)}` : '—'}
    </span>
    <span class="kpi-sublabel">p50 over last {turnLatencies.length} turns</span>
    <span class="kpi-label">Latency (ms)</span>
  </div>

  <!-- Status tile -->
  <div class="kpi-tile" data-testid="kpi-status">
    {#if topologyInfo}
      <StatusChip kind="info" label="ℹ" />
      <span class="kpi-sublabel">{topologyInfo.headline}</span>
    {:else}
      <span class="kpi-value">✓</span>
      <span class="kpi-sublabel">Idle</span>
    {/if}
    <span class="kpi-label">Status</span>
  </div>
</div>

<style>
  .kpi-strip {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-3);
    padding: var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    background: var(--color-surface);
  }

  .kpi-tile {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    background: var(--color-bg);
  }

  .kpi-value-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .kpi-value {
    font-size: var(--text-2xl);
    font-weight: 600;
    color: var(--color-text);
    line-height: 1;
  }

  .kpi-3xl {
    font-size: var(--text-3xl);
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
  }

  .kpi-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
    letter-spacing: var(--tracking-wider);
  }

  .kpi-sublabel {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .sparkline {
    flex-shrink: 0;
  }

  .spark-dot {
    width: var(--size-sparkline-dot);
    height: var(--size-sparkline-dot);
    border-radius: 50%;
    background: var(--color-accent);
    display: inline-block;
  }
</style>
