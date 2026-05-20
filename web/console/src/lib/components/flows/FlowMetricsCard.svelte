<script lang="ts">
  // Harbor Console — Flow Metrics card (Phase 73i / D-117).
  //
  // Renders the `flows.metrics` sparkline aggregates for the selected
  // flow: runs-per-bucket, p95 latency, success rate. Read-only — no
  // actions. The sparkline is a simple inline SVG bar chart (no chart
  // library — the data is low-cardinality bucket counts).
  import type { FlowMetrics } from '$lib/flows/types';
  import { formatDurationMS, formatRate } from '$lib/flows/format';

  interface Props {
    metrics: FlowMetrics | null;
  }

  const { metrics }: Props = $props();

  const maxRuns = $derived(
    metrics ? Math.max(1, ...metrics.buckets.map((b) => b.runs)) : 1,
  );
  const totalRuns = $derived(
    metrics ? metrics.buckets.reduce((s, b) => s + b.runs, 0) : 0,
  );
  const avgP95 = $derived(
    metrics && metrics.buckets.length > 0
      ? metrics.buckets.reduce((s, b) => s + b.p95_latency_ms, 0) /
          metrics.buckets.length
      : 0,
  );
</script>

<section class="metrics-card" data-testid="flow-metrics-card">
  <h3>Flow Metrics</h3>
  {#if !metrics}
    <p class="muted">Select a flow to see its metrics.</p>
  {:else}
    <dl class="rollup">
      <div>
        <dt>Runs (window)</dt>
        <dd data-testid="metrics-total-runs">{totalRuns}</dd>
      </div>
      <div>
        <dt>Avg p95 latency</dt>
        <dd>{formatDurationMS(avgP95)}</dd>
      </div>
      <div>
        <dt>Budget used</dt>
        <dd>{metrics.budget_consumption.requests_used} req</dd>
      </div>
    </dl>
    <div class="sparkline" data-testid="metrics-sparkline">
      {#each metrics.buckets as bucket, i (i)}
        <div
          class="bar"
          style:height={`${(bucket.runs / maxRuns) * 100}%`}
          title={`${bucket.runs} runs · ${formatRate(bucket.success_rate)} success`}
        ></div>
      {/each}
    </div>
  {/if}
</section>

<style>
  .metrics-card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h3 {
    font-size: var(--text-sm);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .rollup {
    display: flex;
    gap: var(--space-6);
    margin: var(--space-0) var(--space-0) var(--space-4);
  }

  dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  dd {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-family: var(--font-mono);
  }

  .sparkline {
    display: flex;
    align-items: flex-end;
    gap: var(--space-1);
    height: var(--size-sparkline-height);
  }

  .bar {
    flex: 1;
    min-height: var(--size-bar-min);
    background: var(--color-accent);
    border-radius: var(--radius-sm);
  }
</style>
