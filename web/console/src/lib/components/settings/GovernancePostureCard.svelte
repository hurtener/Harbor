<script lang="ts">
  // Settings — Governance Posture card.
  //
  // READ: consumes `governance.posture` — the IdentityTiers view (per-tier
  // budget ceiling + rate limit + MaxTokens).
  //
  // WRITE (D-332): an ADMIN-gated affordance drives `governance.set_posture`
  // — the write sibling of the read. The edit form is visible/enabled ONLY
  // for a session carrying the `admin` scope claim; it goes through the typed
  // `HarborClient` (no hand-rolled fetch) and RE-READS `governance.posture`
  // after a successful write rather than mirroring its own submission (the
  // runtime stays the sole owner of the policy record; D-061). See
  // `docs/design/console/CONVENTIONS.md` (D-121) — the four-state async
  // contract + design-token surface below is built against it.
  //
  // The mock-mode banner renders here too when `llm.posture` reports
  // MockMode = true.
  import type { GovernancePostureResponse } from '$lib/protocol/settings.js';
  import type { GovernancePostureWriteState } from '$lib/settings/state.svelte.js';
  import MockModeBanner from './MockModeBanner.svelte';

  let {
    governance,
    mockMode,
    write = null
  }: {
    governance: GovernancePostureResponse | null;
    mockMode: boolean;
    write?: GovernancePostureWriteState | null;
  } = $props();

  // The write affordance renders the runtime's re-read posture once a write
  // has landed; otherwise it shows the read the page loaded.
  const effectiveGovernance = $derived(write?.posture ?? governance);

  // The wire shape is `identity_tiers` (tier name → config map); the latent
  // default is signalled by an empty/absent map (Go: "an empty map signals no
  // enforcement"). Project it to ordered [name, view] entries for the table.
  const tierEntries = $derived(Object.entries(effectiveGovernance?.identity_tiers ?? {}));
</script>

<div class="card-body" data-testid="settings-governance-posture">
  {#if mockMode}
    <MockModeBanner />
  {/if}
  {#if effectiveGovernance === null}
    <p class="muted">Governance posture unavailable.</p>
  {:else if tierEntries.length === 0}
    <p class="muted" data-testid="governance-latent">
      Governance is at its latent default — no identity tiers configured. No cost
      ceilings, rate limits, or MaxTokens caps are enforced.
    </p>
  {:else}
    <p class="muted">
      Default tier: <strong>{effectiveGovernance.default_tier || '—'}</strong>
      &middot; Your tier: <strong>{effectiveGovernance.resolved_tier || '—'}</strong>
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
  {/if}

  {#if write}
    <div class="write" data-testid="governance-set-posture">
      {#if !write.hasAdminScope}
        <p class="note" data-testid="governance-write-gated">
          Writing the identity-tier policy requires the <strong>admin</strong> scope
          claim. Re-attach with an admin token to edit.
        </p>
      {:else}
        <p class="note">
          Write the whole identity-tier policy table (a full replace). A tier that
          omits or zeroes a currently-enforced ceiling is rejected.
        </p>
        <label class="field">
          <span>Default tier</span>
          <input
            type="text"
            bind:value={write.defaultTier}
            oninput={() => write?.markEdited()}
            data-testid="governance-write-default-tier"
          />
        </label>
        <label class="field">
          <span>Identity tiers (JSON)</span>
          <textarea
            rows="8"
            bind:value={write.tiersJSON}
            oninput={() => write?.markEdited()}
            data-testid="governance-write-tiers"
          ></textarea>
        </label>
        <div class="actions">
          <button
            type="button"
            onclick={() => write?.save()}
            disabled={write.phase === 'saving'}
            data-testid="governance-write-save"
          >
            {write.phase === 'saving' ? 'Saving…' : 'Replace policy'}
          </button>
          {#if write.saved}
            <span class="ok" data-testid="governance-write-saved">Saved.</span>
          {/if}
        </div>
        {#if write.pendingRestart}
          <p class="note" data-testid="governance-write-pending-restart">
            Saved, but this runtime booted with no governance enforcement active,
            so the new policy will not be enforced until the runtime restarts.
          </p>
        {/if}
        {#if write.error}
          <p class="err" data-testid="governance-write-error">{write.error.message}</p>
        {/if}
      {/if}
    </div>
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
  .write {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin-top: var(--space-3);
    padding-top: var(--space-3);
    border-top: var(--border-hairline);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .field input,
  .field textarea {
    font-size: var(--text-sm);
    color: var(--color-text);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }
  .field textarea {
    font-family: inherit;
    resize: vertical;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .actions button {
    align-self: flex-start;
    font-size: var(--text-sm);
    color: var(--color-bg);
    background: var(--color-accent);
    border: var(--border-hairline);
    border-color: var(--color-accent);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    cursor: pointer;
  }
  .actions button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .ok {
    font-size: var(--text-xs);
    color: var(--color-success);
  }
  .err {
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
</style>
