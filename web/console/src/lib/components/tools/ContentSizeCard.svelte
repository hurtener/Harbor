<script lang="ts">
  // ContentSizeCard — the right-rail bottom card (page-tools.md §12):
  // the per-tool result-size histogram vs the heavy-content threshold
  // (RFC §6.5 / D-026) plus the negotiated MCP-Apps DisplayMode
  // snapshot (D-062). Read-only. Svelte 5 runes mode (D-092).
  import type { ToolContentStats } from '$lib/protocol/tools.js';

  let { stats = null }: { stats?: ToolContentStats | null } = $props();

  function fmtBytes(n: number): string {
    if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(0)}MiB`;
    if (n >= 1024) return `${(n / 1024).toFixed(0)}KiB`;
    return `${n}B`;
  }

  let maxCount = $derived(
    stats === null
      ? 0
      : stats.histogram.reduce((m, b) => Math.max(m, b.count), 0)
  );

  function barWidth(count: number): string {
    if (maxCount === 0) return '0%';
    return `${Math.round((count / maxCount) * 100)}%`;
  }
</script>

<section class="card" data-testid="tools-content-card">
  <h3>Content size &amp; display mode</h3>
  {#if stats === null}
    <p class="muted">Select a tool to see its result-size profile.</p>
  {:else}
    <p class="threshold">
      Heavy threshold: <strong>{fmtBytes(stats.heavy_threshold_bytes)}</strong>
      · heavy results: <strong>{stats.heavy_count}</strong>
    </p>
    {#if stats.histogram.length === 0}
      <p class="muted">No recent invocations recorded.</p>
    {:else}
      <ul class="histogram">
        {#each stats.histogram as bucket (bucket.max_bytes)}
          <li>
            <span class="bucket-label">≤{fmtBytes(bucket.max_bytes)}</span>
            <span class="bar-track">
              <span class="bar" style:width={barWidth(bucket.count)}></span>
            </span>
            <span class="bucket-count">{bucket.count}</span>
          </li>
        {/each}
      </ul>
    {/if}
    {#if Object.keys(stats.negotiated_display).length > 0}
      <h4>Negotiated display modes</h4>
      <ul class="display-modes">
        {#each Object.entries(stats.negotiated_display) as [mime, mode] (mime)}
          <li><code>{mime}</code> → <span class="chip">{mode}</span></li>
        {/each}
      </ul>
    {/if}
  {/if}
</section>

<style>
  .card {
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-width-thin) solid var(--color-border);
    border-radius: var(--radius-md);
  }

  h3 {
    margin: var(--space-0) var(--space-0) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--border-width-thin);
  }

  h4 {
    margin: var(--space-3) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .threshold {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  .threshold strong {
    color: var(--color-text);
  }

  .histogram,
  .display-modes {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .histogram li {
    display: grid;
    grid-template-columns:
      var(--layout-hist-label-width) 1fr var(--layout-hist-count-width);
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
  }

  .bucket-label {
    color: var(--color-text-muted);
  }

  .bucket-count {
    text-align: right;
    color: var(--color-text);
  }

  .bar-track {
    height: var(--space-2);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .bar {
    display: block;
    height: 100%;
    background: var(--color-accent);
  }

  .display-modes li {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .chip {
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-accent);
    font-size: var(--text-xs);
  }
</style>
