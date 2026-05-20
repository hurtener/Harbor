<script lang="ts">
  // ContentSizeCard — the Tools-page right-rail result-size content
  // (page-tools.md §12): the per-tool result-size histogram vs the
  // heavy-content threshold (RFC §6.5 / D-026) plus the negotiated
  // MCP-Apps DisplayMode snapshot (D-062). Tools-specific content; the
  // page wraps it in `ui/RailCard`, so this emits only the card BODY and
  // uses the shared `ui/StatusChip` for display-mode pills (D-121,
  // CONVENTIONS.md §3). Read-only. Svelte 5 runes mode (D-092).
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
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

<div data-testid="tools-content-card">
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
          <li><code>{mime}</code> → <StatusChip kind="accent" label={mode} /></li>
        {/each}
      </ul>
    {/if}
  {/if}
</div>

<style>
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
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
  }
</style>
