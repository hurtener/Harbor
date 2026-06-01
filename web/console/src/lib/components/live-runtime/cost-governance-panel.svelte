<script lang="ts">
  // Harbor Console — Live Runtime cockpit Cost panel (Phase 108e / D-177).
  //
  // The runtime's cost rollup, folded CLIENT-SIDE from the SHIPPED
  // `llm.cost.recorded` event topic (reusing the Overview `projectCost`
  // projection — RFC §6.13, no new Protocol method). A spine panel that
  // self-probes its source: an empty event stream renders the honest "no cost
  // recorded yet" state, never a fabricated figure (CLAUDE.md §13).
  //
  // Governance ceilings / rate-limit posture (72g `governance.posture`) is a
  // future gated surface; until it ships this panel shows the cost rollup
  // only and labels the absent posture honestly.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import DollarSign from '@lucide/svelte/icons/dollar-sign';
  import type { CostRollup } from '$lib/overview/cost.js';

  let { rollup }: { rollup: CostRollup } = $props();
</script>

{#if rollup.rows.length > 0}
  <div class="cost" data-testid="cost-panel">
    <div class="total-row">
      <span class="total" data-testid="cost-total">${rollup.totalUSD.toFixed(4)}</span>
      <span class="total-label">total observed this session</span>
    </div>
    <ul class="cost-list">
      {#each rollup.rows as row (row.key)}
        <li class="cost-row" data-testid="cost-row">
          <span class="cost-key">{row.key}</span>
          <span class="cost-amount">${row.costUSD.toFixed(4)}</span>
        </li>
      {/each}
    </ul>
  </div>
{:else}
  <div class="cost-empty" data-testid="cost-panel-empty">
    <span class="empty-icon"><DollarSign size={20} aria-hidden="true" /></span>
    <p class="empty-headline">No cost recorded yet</p>
    <p class="empty-detail">
      LLM spend appears here as the runtime emits <code>llm.cost.recorded</code>
      events. Governance ceilings &amp; rate-limit posture surface here once the
      runtime advertises them.
    </p>
  </div>
{/if}

<style>
  .cost {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .total-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .total {
    font-size: var(--text-xl);
    font-weight: 600;
    color: var(--color-text);
  }

  .total-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .cost-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .cost-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-sm);
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .cost-key {
    color: var(--color-text);
    overflow-wrap: anywhere;
  }

  .cost-amount {
    color: var(--color-text-muted);
    font-family: var(--font-mono);
  }

  .cost-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-5) var(--space-2);
    text-align: center;
  }

  .empty-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: var(--radius-md);
    margin-bottom: var(--space-1);
    background: var(--color-accent-soft);
    color: var(--color-accent);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
