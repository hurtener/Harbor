<script lang="ts">
  // Harbor Console — Memory page strategy-trace card (Phase 108n / D-186).
  //
  // Renders the live `memory.strategy_trace` projection — how the configured
  // memory strategy is compacting THIS session's memory right now: the
  // rolling-summary text (the compaction output), the verbatim-turn count,
  // the token estimate, and the FSM health. It is the strategy's real
  // GetLLMContext + Health output (not a fabricated per-step selection
  // trace), so an empty session renders an honest empty state (§13).
  import type { MemoryStrategyTrace } from '$lib/protocol/memory-types.js';

  let { trace }: { trace: MemoryStrategyTrace | null } = $props();
</script>

<div class="strategy-trace" data-testid="memory-strategy-trace">
  {#if trace === null}
    <p class="muted">Strategy trace unavailable.</p>
  {:else}
    <dl class="trace-grid">
      <div><dt>Strategy</dt><dd>{trace.strategy}</dd></div>
      <div><dt>Verbatim turns</dt><dd>{trace.recent_turn_count}</dd></div>
      <div><dt>Est. tokens</dt><dd>{trace.estimated_tokens}</dd></div>
      <div><dt>Health</dt><dd>{trace.health}</dd></div>
    </dl>
    <h4>Rolling summary</h4>
    {#if trace.summary}
      <pre class="summary" data-testid="memory-strategy-summary">{trace.summary}</pre>
    {:else}
      <p class="muted" data-testid="memory-strategy-summary-empty">
        No rolling summary yet — turns are kept verbatim until the strategy
        compacts them.
      </p>
    {/if}
  {/if}
</div>

<style>
  .strategy-trace {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .trace-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .trace-grid div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }

  dt {
    color: var(--color-text-muted);
  }

  dd {
    margin: var(--space-0);
    font-variant-numeric: tabular-nums;
  }

  h4 {
    margin: var(--space-1) var(--space-0) var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .summary {
    margin: var(--space-0);
    padding: var(--space-3);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    white-space: pre-wrap;
    overflow: auto;
    max-height: var(--size-value-viewer-max);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
