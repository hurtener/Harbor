<script lang="ts">
  // RunHistoryStrip — the bottom-right Run-history panel
  // (page-tools.md §12): a recent invocation timeline for the selected
  // tool. The invocation rows stream from the `tool.*` event topic in
  // a later phase; this strip ships the static surface + the selected-
  // tool summary. Svelte 5 runes mode (D-092).
  import type { Tool, ToolMetrics } from '$lib/protocol/tools.js';

  let {
    tool = null,
    metrics = null
  }: {
    tool?: Tool | null;
    metrics?: ToolMetrics | null;
  } = $props();
</script>

<section class="card" data-testid="tools-run-history">
  <h3>Run history</h3>
  {#if tool === null}
    <p class="muted">Select a tool to see its recent invocations.</p>
  {:else}
    <p class="summary">
      <strong>{tool.name}</strong>
      {#if metrics !== null}
        — {metrics.invocations} invocations, {metrics.failures} failed
        ({metrics.window})
      {/if}
    </p>
    <p class="muted">
      Per-invocation timeline streams from the <code>tool.*</code> event
      topic. Rows deep-link into the originating session's bottom dock.
    </p>
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

  .summary {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
