<script lang="ts">
  // Harbor Console — Overview cost-rollup card (Phase 73a / 108c).
  //
  // Canvas row-3 right column (page-overview.md §4/§12). Renders the windowed
  // total + a stacked share bar + a per-key breakdown (key · $ · %) folded
  // client-side from the SHIPPED `llm.cost.recorded` topic (`$lib/overview/
  // cost.ts` — NO new Protocol method). Two real axes (Phase 108c):
  //   - Model  — the inference endpoint (rate-limit / routing lens), multi-row;
  //   - Agent  — all in-scope cost under the connected runtime's agent name
  //              (one row in single-runtime V1; fans out in multi-runtime).
  // There is NO by-agent-WITHIN-runtime split (no agent_id on the cost event)
  // and NO "vs previous window" delta (no previous-window total source) — both
  // omitted rather than faked (procedure §1).
  //
  // Svelte 5 runes (D-092); design tokens only (CLAUDE.md §4.5).
  import type { CostRollup, CostBreakdown } from '$lib/overview/cost.js';

  let {
    rollup,
    breakdown,
    onbreakdown,
    disconnected = false
  }: {
    rollup: CostRollup;
    /** The active grouping axis. */
    breakdown: CostBreakdown;
    /** Switch the grouping axis. */
    onbreakdown: (b: CostBreakdown) => void;
    /** True when no Runtime is attached — render the honest placeholder. */
    disconnected?: boolean;
  } = $props();

  function fmtUSD(n: number): string {
    return `$${n.toFixed(2)}`;
  }
  function pct(n: number): number {
    return rollup.totalUSD > 0 ? (n / rollup.totalUSD) * 100 : 0;
  }
  // Per-segment opacity (descending) keeps the stacked bar token-pure
  // (single accent hue) without a categorical colour palette.
  function seg(i: number, len: number): number {
    if (len <= 1) return 1;
    return 1 - (i / len) * 0.6;
  }
</script>

<div class="cost-card" data-testid="cost-rollup-card">
  {#if disconnected}
    <p class="empty" data-testid="cost-rollup-disconnected">Not connected to a Runtime.</p>
    <a class="govern-link" href="/settings">Attach a Runtime in Settings →</a>
  {:else}
    <div class="cost-head">
      <span class="total" data-testid="cost-rollup-total">{fmtUSD(rollup.totalUSD)}</span>
      <div class="axis-select" role="group" aria-label="Cost breakdown">
        <button
          type="button"
          class="axis-chip"
          data-active={breakdown === 'model'}
          data-testid="cost-axis-model"
          onclick={() => onbreakdown('model')}
        >
          Model
        </button>
        <button
          type="button"
          class="axis-chip"
          data-active={breakdown === 'runtime'}
          data-testid="cost-axis-runtime"
          onclick={() => onbreakdown('runtime')}
        >
          Agent
        </button>
      </div>
    </div>

    {#if rollup.rows.length === 0}
      <p class="empty" data-testid="cost-rollup-empty">No cost recorded in the window.</p>
    {:else}
      <div class="bar" data-testid="cost-rollup-bar" aria-hidden="true">
        {#each rollup.rows as row, i (row.key)}
          <span
            class="seg"
            style:width={`${pct(row.costUSD)}%`}
            style:opacity={seg(i, rollup.rows.length)}
          ></span>
        {/each}
      </div>
      <ul class="rows">
        {#each rollup.rows as row, i (row.key)}
          <li class="row" data-testid="cost-rollup-row">
            <span class="dot" style:opacity={seg(i, rollup.rows.length)}></span>
            <span class="key">{row.key}</span>
            <span class="cost">{fmtUSD(row.costUSD)}</span>
            <span class="share">{pct(row.costUSD).toFixed(0)}%</span>
          </li>
        {/each}
      </ul>
    {/if}
    <a class="govern-link" href="/settings">Cost ceilings →</a>
  {/if}
</div>

<style>
  /* Content-only — the enclosing panel section (Phase 108c) provides the card
     surface so all three row-3/row-4 panels share one carded look. */
  .cost-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .cost-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .total {
    font-size: var(--text-2xl);
    font-weight: 600;
    line-height: 1;
  }

  .axis-select {
    display: inline-flex;
    gap: var(--space-1);
  }

  .axis-chip {
    background: var(--color-surface);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .axis-chip[data-active='true'] {
    color: var(--color-accent);
    border-color: var(--color-accent);
    background: var(--color-accent-soft);
  }

  .bar {
    display: flex;
    width: 100%;
    height: var(--space-2);
    border-radius: var(--radius-pill);
    overflow: hidden;
    gap: var(--size-px);
  }

  .seg {
    background: var(--color-accent);
    min-width: var(--size-px);
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
    display: grid;
    grid-template-columns: auto 1fr auto auto;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-accent);
  }

  .key {
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .cost {
    font-family: var(--font-mono);
    color: var(--color-text);
  }

  .share {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    min-width: var(--size-chip-min-width);
    text-align: right;
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
