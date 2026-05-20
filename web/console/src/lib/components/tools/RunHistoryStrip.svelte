<script lang="ts">
  // RunHistoryStrip — the Tools-page Run-history content (page-tools.md
  // §12): a recent-invocation summary for the selected tool. The
  // per-invocation rows stream from the `tool.*` event topic with the
  // Events-page surface; this strip ships the selected-tool summary.
  // Tools-specific content; the page wraps it in `ui/RailCard`, so this
  // emits only the card BODY — no card chrome (D-121, CONVENTIONS.md §3).
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Tool, ToolMetrics } from '$lib/protocol/tools.js';

  let {
    tool = null,
    metrics = null
  }: {
    tool?: Tool | null;
    metrics?: ToolMetrics | null;
  } = $props();
</script>

<div data-testid="tools-run-history">
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
</div>

<style>
  .summary {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .muted {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  code {
    font-family: var(--font-mono);
  }
</style>
