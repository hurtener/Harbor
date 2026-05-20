<script lang="ts">
  // StatusErrorRateCard — the right-rail middle card (page-tools.md
  // §12): per-tool error-rate gauges over a 1h / 24h / 7d window
  // toggle plus a status pill, for the row currently selected in the
  // catalog. Svelte 5 runes mode (D-092).
  import type { ToolMetrics, ToolMetricsWindow } from '$lib/protocol/tools.js';

  let {
    metrics = null,
    window: selectedWindow = '1h',
    onwindow
  }: {
    metrics?: ToolMetrics | null;
    window?: ToolMetricsWindow;
    onwindow: (w: ToolMetricsWindow) => void;
  } = $props();

  const WINDOWS: ToolMetricsWindow[] = ['1h', '24h', '7d'];

  function rateFor(m: ToolMetrics, w: ToolMetricsWindow): number {
    if (w === '24h') return m.error_rate_24h;
    if (w === '7d') return m.error_rate_7d;
    return m.error_rate_1h;
  }

  function pct(v: number): string {
    return `${(v * 100).toFixed(1)}%`;
  }
</script>

<section class="card" data-testid="tools-status-card">
  <header>
    <h3>Status &amp; error rate</h3>
    <div class="windows" role="group" aria-label="metrics window">
      {#each WINDOWS as w (w)}
        <button
          type="button"
          class="win"
          class:win-active={w === selectedWindow}
          data-testid="tools-metrics-window"
          data-window={w}
          onclick={() => onwindow(w)}
        >
          {w}
        </button>
      {/each}
    </div>
  </header>

  {#if metrics === null}
    <p class="muted">Select a tool to see its health.</p>
  {:else}
    <div class="pill-row">
      <span
        class="pill"
        class:pill-ok={metrics.status === 'Healthy'}
        class:pill-warn={metrics.status === 'Degraded'}
        class:pill-danger={metrics.status === 'Offline'}
        data-testid="tools-status-pill"
      >
        {metrics.status}
      </span>
    </div>
    <dl>
      <div>
        <dt>Error rate ({selectedWindow})</dt>
        <dd data-testid="tools-error-rate">{pct(rateFor(metrics, selectedWindow))}</dd>
      </div>
      <div>
        <dt>Invocations</dt>
        <dd>{metrics.invocations}</dd>
      </div>
      <div>
        <dt>Failures</dt>
        <dd>{metrics.failures}</dd>
      </div>
    </dl>
  {/if}
</section>

<style>
  .card {
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: var(--space-3);
  }

  h3 {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--border-width-thin);
  }

  .windows {
    display: flex;
    gap: var(--space-1);
  }

  .win {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-width-thin) solid var(--color-border);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .win-active {
    background: var(--color-accent-soft);
    color: var(--color-accent);
    border-color: var(--color-accent);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .pill-row {
    margin-bottom: var(--space-3);
  }

  .pill {
    display: inline-block;
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    background: var(--color-surface-raised);
    color: var(--color-text);
  }

  .pill-ok {
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .pill-warn {
    background: var(--color-warning-soft);
    color: var(--color-warning);
  }

  .pill-danger {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  dl {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  dl div {
    display: flex;
    justify-content: space-between;
  }

  dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  dd {
    margin: var(--space-0);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
</style>
