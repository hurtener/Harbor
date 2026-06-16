<script lang="ts">
  // Settings — Governance Posture card (Phase 73m / D-129).
  //
  // Read-only. Consumes 72g's `governance.posture` Protocol method
  // (D-112) — the D-081 IdentityTiers view (per-tier budget ceiling +
  // rate limit + MaxTokens). Editing IdentityTiers is post-V1
  // (page-settings.md §10) — the card never mutates.
  //
  // The mock-mode banner renders here too (per the acceptance criteria)
  // when 72g's `llm.posture` reports MockMode = true.
  import type { GovernancePostureResponse } from '$lib/protocol/settings.js';
  import MockModeBanner from './MockModeBanner.svelte';

  let {
    governance,
    mockMode
  }: {
    governance: GovernancePostureResponse | null;
    mockMode: boolean;
  } = $props();

  // The wire shape is `identity_tiers` (tier name → config map); the latent
  // default is signalled by an empty/absent map (Go: "an empty map signals no
  // enforcement"). Project it to ordered [name, view] entries for the table.
  const tierEntries = $derived(Object.entries(governance?.identity_tiers ?? {}));
</script>

<div class="card-body" data-testid="settings-governance-posture">
  {#if mockMode}
    <MockModeBanner />
  {/if}
  {#if governance === null}
    <p class="muted">Governance posture unavailable.</p>
  {:else if tierEntries.length === 0}
    <p class="muted" data-testid="governance-latent">
      Governance is at its latent default — no identity tiers configured. No cost
      ceilings, rate limits, or MaxTokens caps are enforced.
    </p>
  {:else}
    <p class="muted">
      Default tier: <strong>{governance.default_tier || '—'}</strong>
      &middot; Your tier: <strong>{governance.resolved_tier || '—'}</strong>
    </p>
    <table class="tier-table">
      <thead>
        <tr><th>Tier</th><th>Budget (USD)</th><th>Max tokens</th><th>Rate limit</th></tr>
      </thead>
      <tbody>
        {#each tierEntries as [name, tier] (name)}
          <tr data-testid="governance-tier-row">
            <td>{name}</td>
            <td>{tier.budget_ceiling_usd ?? '—'}</td>
            <td>{tier.max_tokens ?? '—'}</td>
            <td class="muted">
              {tier.rate_limit?.capacity
                ? `${tier.rate_limit.capacity} cap`
                : 'unbounded'}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
    <p class="note">Editing identity tiers is a runtime-config concern (post-V1).</p>
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }
  .tier-table {
    width: 100%;
    border-collapse: collapse;
  }
  th {
    text-align: left;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding-bottom: var(--space-2);
  }
  td {
    padding: var(--space-1) var(--space-2) var(--space-1) var(--space-0);
    color: var(--color-text);
  }
  .muted {
    color: var(--color-text-muted);
  }
  .note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
