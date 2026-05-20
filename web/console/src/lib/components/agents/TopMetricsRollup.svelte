<script lang="ts">
  // Harbor Console — Agents-page top metrics rollup (Phase 73e / D-124).
  //
  // The mockup's hero numbers: Active Agents / Running Tasks / Total Cost
  // / Total Tokens, sourced from the `agents.metrics` registry-wide
  // rollup. A page-specific component (CONVENTIONS.md §3) — it composes
  // tokens only, no raw literals.
  import type { AgentMetrics } from '$lib/protocol/agents.js';

  let { metrics }: { metrics: AgentMetrics | null } = $props();

  /** Formats a USD amount with two decimal places. */
  function usd(n: number): string {
    return `$${n.toFixed(2)}`;
  }

  /** Formats a token count with thousands separators. */
  function tokens(n: number): string {
    return n.toLocaleString('en-US');
  }
</script>

<div class="rollup" data-testid="agents-metrics-rollup">
  <div class="metric">
    <span class="value" data-testid="metric-active-agents">
      {metrics?.active_agents ?? '—'}
    </span>
    <span class="label">Active agents</span>
  </div>
  <div class="metric">
    <span class="value" data-testid="metric-running-tasks">
      {metrics?.running_tasks ?? '—'}
    </span>
    <span class="label">Running tasks</span>
  </div>
  <div class="metric">
    <span class="value" data-testid="metric-total-cost">
      {metrics ? usd(metrics.total_cost_usd) : '—'}
    </span>
    <span class="label">Total cost</span>
  </div>
  <div class="metric">
    <span class="value" data-testid="metric-total-tokens">
      {metrics ? tokens(metrics.total_tokens) : '—'}
    </span>
    <span class="label">Total tokens</span>
  </div>
</div>

<style>
  .rollup {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-3);
  }

  .metric {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-3) var(--space-4);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .value {
    font-size: var(--text-xl);
    font-weight: 600;
    color: var(--color-text);
    font-variant-numeric: tabular-nums;
  }

  .label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
</style>
