<script lang="ts">
  // Harbor Console — Agents detail Cost ceilings tab (Phase 73e /
  // D-124). Per-identity-tier cost ceilings + spend (Phase 36a) and
  // rate-limit posture (Phase 36b), from `agents.governance`.
  // Page-specific component.
  import type { AgentGovernance } from '$lib/protocol/agents.js';

  let { governance }: { governance: AgentGovernance } = $props();

  const hasData = $derived(
    governance.ceilings.length > 0 || governance.rate_limits.length > 0
  );
</script>

<div class="cost-tab" data-testid="agent-cost-tab">
  {#if !hasData}
    <p class="empty">No governance posture configured for this agent.</p>
  {:else}
    {#if governance.ceilings.length > 0}
      <section>
        <h4>Cost ceilings</h4>
        <table>
          <thead>
            <tr><th>Tier</th><th>Spend</th><th>Limit</th></tr>
          </thead>
          <tbody>
            {#each governance.ceilings as c (c.tier)}
              <tr data-testid="cost-ceiling-row">
                <td>{c.tier}</td>
                <td class="num">${c.spend_usd.toFixed(2)}</td>
                <td class="num">${c.limit_usd.toFixed(2)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </section>
    {/if}
    {#if governance.rate_limits.length > 0}
      <section>
        <h4>Rate limits</h4>
        <table>
          <thead>
            <tr><th>Tier</th><th>Req/min</th><th>Max tokens</th></tr>
          </thead>
          <tbody>
            {#each governance.rate_limits as r (r.tier)}
              <tr data-testid="rate-limit-row">
                <td>{r.tier}</td>
                <td class="num">{r.requests_per_minute}</td>
                <td class="num">{r.max_tokens}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </section>
    {/if}
    <a class="ceilings-link" href="/settings">View governance settings →</a>
  {/if}
</div>

<style>
  .cost-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  h4 {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    text-align: left;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding: var(--space-1) var(--space-2);
  }

  td {
    font-size: var(--text-sm);
    color: var(--color-text);
    padding: var(--space-1) var(--space-2);
    border-top: var(--border-hairline);
  }

  td.num {
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .ceilings-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
