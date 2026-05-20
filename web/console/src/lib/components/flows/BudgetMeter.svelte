<script lang="ts">
  // Harbor Console — per-flow Budget meter (Phase 73i / D-117).
  //
  // Renders the per-flow `Budget` (D-023) as progress bars: token cap,
  // cost cap, request cap. Read-only at V1 — editing the Budget is
  // `flows.set_budget`, deferred post-V1 (page-flows.md §10).
  import type { FlowBudget, FlowBudgetConsumption } from '$lib/flows/types';
  import { budgetFraction, formatCost } from '$lib/flows/format';

  interface Props {
    budget: FlowBudget;
    consumption: FlowBudgetConsumption;
  }

  const { budget, consumption }: Props = $props();

  const costFrac = $derived(
    budgetFraction(consumption.cost_usd_used, budget.cost_cap_usd ?? 0),
  );
  const reqFrac = $derived(
    budgetFraction(consumption.requests_used, budget.request_cap ?? 0),
  );
  const tokenFrac = $derived(
    budgetFraction(consumption.tokens_used, budget.token_cap ?? 0),
  );
</script>

<section class="budget-meter" data-testid="budget-meter">
  <h3>Per-flow Budget</h3>
  <div class="meter">
    <span class="label">Cost</span>
    <div class="track">
      <div class="fill" style:width={`${costFrac * 100}%`}></div>
    </div>
    <span class="value">
      {formatCost(consumption.cost_usd_used)} / {budget.cost_cap_usd
        ? formatCost(budget.cost_cap_usd)
        : 'no cap'}
    </span>
  </div>
  <div class="meter">
    <span class="label">Requests</span>
    <div class="track">
      <div class="fill" style:width={`${reqFrac * 100}%`}></div>
    </div>
    <span class="value">
      {consumption.requests_used} / {budget.request_cap ?? 'no cap'}
    </span>
  </div>
  <div class="meter">
    <span class="label">Tokens</span>
    <div class="track">
      <div class="fill" style:width={`${tokenFrac * 100}%`}></div>
    </div>
    <span class="value">
      {consumption.tokens_used} / {budget.token_cap ?? 'no cap'}
    </span>
  </div>
</section>

<style>
  .budget-meter {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-4);
  }

  h3 {
    font-size: var(--text-sm);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  .meter {
    margin-bottom: var(--space-3);
  }

  .label {
    display: block;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin-bottom: var(--space-1);
  }

  .track {
    height: var(--size-progress-track);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .fill {
    height: 100%;
    background: var(--color-accent);
  }

  .value {
    display: block;
    margin-top: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
