<script lang="ts">
  // Harbor Console — Tasks page right-rail Cost Breakdown (Phase 108i /
  // D-181). Renders the per-task cost broken down BY TOKEN TYPE (Input /
  // Output / Reasoning / Total) — the real fields on the run's
  // `llm.cost.recorded` events. The mock's LLM / Tools / Embeddings /
  // Overhead split has no wire source; per the operator's D-181 sign-off
  // this token-type axis keeps the four-row visual while staying wire-real
  // (never a fabricated category). Source is the live stream, NOT
  // `tasks.get.cost` (which comes back all-zero).
  //
  // Each row carries BOTH the token COUNT and the cost: live verification
  // found some providers populate only `Cost.TotalCost` (the per-type
  // `Input/Output/ReasoningTokensCost` come back 0), so the token counts
  // (`Usage.{Prompt,Completion,Reasoning}Tokens`, always populated) are
  // the meaningful per-type signal — the cost column stays honest at the
  // value the wire actually carries. Tokens-specific; tokens only.
  import type { RunCost } from '$lib/tasks/run-events.js';

  let { cost }: { cost: RunCost } = $props();

  const rows = $derived([
    { label: 'Input', usd: cost.inputUSD, tokens: cost.promptTokens },
    { label: 'Output', usd: cost.outputUSD, tokens: cost.outputTokens },
    { label: 'Reasoning', usd: cost.reasoningUSD, tokens: cost.reasoningTokens }
  ]);
</script>

{#if cost.events === 0}
  <p class="muted" data-testid="rail-cost-breakdown-empty">
    No <code>llm.cost.recorded</code> events for this task in the live stream yet.
  </p>
{:else}
  <div class="cost-card" data-testid="rail-cost-breakdown">
    <table class="cost-table">
      <thead>
        <tr><th>Type</th><th class="num">Tokens</th><th class="num">Cost</th></tr>
      </thead>
      <tbody>
        {#each rows as r (r.label)}
          <tr>
            <td>{r.label}</td>
            <td class="num mono">{r.tokens.toLocaleString()}</td>
            <td class="num mono">${r.usd.toFixed(4)}</td>
          </tr>
        {/each}
        <tr class="total-row">
          <td>Total</td>
          <td class="num mono">{cost.totalTokens.toLocaleString()}</td>
          <td class="num mono">${cost.totalUSD.toFixed(4)}</td>
        </tr>
      </tbody>
    </table>
    {#if cost.models.length > 0}
      <p class="models" title="Models seen on this run's cost events">
        {cost.models.join(' · ')}
      </p>
    {/if}
  </div>
{/if}

<style>
  .cost-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .cost-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .cost-table th {
    padding: var(--space-0) var(--space-0) var(--space-1);
    font-size: var(--text-xs);
    font-weight: 500;
    color: var(--color-text-muted);
    text-align: left;
    border-bottom: var(--border-hairline);
  }

  .cost-table td {
    padding: var(--space-1) var(--space-0);
    color: var(--color-text);
  }

  .cost-table .num {
    text-align: right;
  }

  .total-row td {
    border-top: var(--border-hairline);
    font-weight: 600;
    padding-top: var(--space-2);
  }

  .models {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
