<script lang="ts">
  // Harbor Console — Overview cost-rollup card (Phase 73a / D-127).
  //
  // The right-column card in canvas row 3 (page-overview.md §4). It
  // renders a per-agent cost breakdown by default; a per-tenant
  // breakdown is the admin elevation (page-overview.md §12 — the
  // operator's existing scope claim selects which appears). Data is the
  // SHIPPED `llm.cost.recorded` event topic, aggregated client-side via
  // `$lib/overview/cost.ts` — NO new Protocol method (page-overview.md
  // §5 tags it `[shipped]`).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import type { CostRollup } from '$lib/overview/cost.js';

  let {
    rollup,
    canElevate,
    disconnected = false
  }: {
    /** The client-side cost rollup. */
    rollup: CostRollup;
    /** Whether the operator carries the admin scope (drives the per-tenant note). */
    canElevate: boolean;
    /** True when the Console has no Runtime attached (Phase 83r / W1). When
        true, the card renders the consolidated "Not connected" placeholder
        rather than the synthetic `$0.00 · No cost recorded` shape. */
    disconnected?: boolean;
  } = $props();

  function fmtUSD(n: number): string {
    return `$${n.toFixed(2)}`;
  }
</script>

<div class="cost-card" data-testid="cost-rollup-card">
  {#if disconnected}
    <!-- W1 fix (Phase 83r): the disconnected state stops the synthetic
         `$0.00 · No cost recorded` rendering. The other three Overview
         cards already route through `<PageState>`'s disconnected branch;
         this card now matches that posture. -->
    <p class="empty" data-testid="cost-rollup-disconnected">
      Not connected to a Runtime.
    </p>
    <a class="govern-link" href="/settings">Attach a Runtime in Settings →</a>
  {:else}
    <div class="cost-head">
      <span class="total" data-testid="cost-rollup-total">{fmtUSD(rollup.totalUSD)}</span>
      <span class="axis">
        by {rollup.breakdown}
        {#if !canElevate}
          <span class="axis-note">(per-tenant needs admin)</span>
        {/if}
      </span>
    </div>
    {#if rollup.rows.length === 0}
      <p class="empty" data-testid="cost-rollup-empty">No cost recorded in the window.</p>
    {:else}
      <ul class="rows">
        {#each rollup.rows as row (row.key)}
          <li class="row" data-testid="cost-rollup-row">
            <span class="key">{row.key}</span>
            <span class="cost">{fmtUSD(row.costUSD)}</span>
          </li>
        {/each}
      </ul>
    {/if}
    <a class="govern-link" href="/settings">Cost ceilings →</a>
  {/if}
</div>

<style>
  .cost-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .cost-head {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .total {
    font-size: var(--text-xl);
    font-weight: 600;
    line-height: 1;
  }

  .axis {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .axis-note {
    text-transform: none;
    letter-spacing: normal;
  }

  .rows {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .row {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }

  .key {
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cost {
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .govern-link {
    font-size: var(--text-xs);
    color: var(--color-accent);
    text-decoration: none;
  }
</style>
