<script lang="ts">
  // StatusErrorRateCard — the Tools-page right-rail per-tool health
  // content (page-tools.md §12): error-rate gauges over a 1h / 24h / 7d
  // window toggle plus a status pill. Tools-specific content; the page
  // wraps it in `ui/RailCard`, so this emits only the card BODY and uses
  // the shared `ui/StatusChip` for the pill — no forked pill or chrome
  // (D-121, CONVENTIONS.md §3). Svelte 5 runes mode (D-092); tokens only.
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import type { ToolMetrics, ToolMetricsWindow, ToolStatus } from '$lib/protocol/tools.js';

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

  /** Maps a runtime `ToolStatus` onto the shared StatusChip kind scale. */
  function statusKind(s: ToolStatus): StatusKind {
    if (s === 'Healthy') return 'success';
    if (s === 'Degraded') return 'warning';
    return 'danger';
  }
</script>

<div data-testid="tools-status-card">
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

  {#if metrics === null}
    <p class="muted">Select a tool to see its health.</p>
  {:else}
    <div class="pill-row">
      <span data-testid="tools-status-pill">
        <StatusChip kind={statusKind(metrics.status)} label={metrics.status} />
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
</div>

<style>
  .windows {
    display: flex;
    gap: var(--space-1);
    margin-bottom: var(--space-3);
  }

  .win {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
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
