<script lang="ts">
  // Harbor Console — Playground page header (Phase 73n / D-130).
  //
  // The page-specific header strip rendered inside the shared
  // `<PageHeader>`'s `actions` slot. It carries: an agent picker, a
  // model badge, a token-count chip, a cost chip, Cancel-run + Restart
  // buttons, and — for admin operators only — a "Run as identity"
  // selector consuming Phase 72b's `IdentityScope` impersonation
  // triplet (Brief 11 §PG-5, D-107).
  //
  // The "Run as identity" selector renders ONLY when the operator
  // carries the `auth.ScopeAdmin` claim — non-admin operators do not
  // see it (rendered absent, not disabled — minimises clutter). When an
  // admin picks a triple, `onimpersonate` fires; the page folds the
  // triple onto `IdentityScope.Impersonating` for the next
  // `user_message` / `start` call.
  //
  // Design tokens only; no raw literals.

  /** An impersonation target an admin can run-as. */
  export interface ImpersonationTarget {
    tenant: string;
    user: string;
    session: string;
    label: string;
  }

  let {
    agents,
    activeAgent,
    model,
    tokenCount,
    costUSD,
    running,
    canImpersonate = false,
    impersonationTargets = [],
    activeImpersonation = null,
    onagentchange,
    oncancel,
    onrestart,
    onimpersonate
  }: {
    agents: string[];
    activeAgent: string;
    model: string;
    tokenCount: number;
    costUSD: number;
    /** True while a run is active — gates Cancel. */
    running: boolean;
    /** True when the operator has the `auth.ScopeAdmin` claim (D-079). */
    canImpersonate?: boolean;
    /** The tenants/users/sessions an admin may run-as. */
    impersonationTargets?: ImpersonationTarget[];
    /** The active impersonation target, or null for "self". */
    activeImpersonation?: ImpersonationTarget | null;
    onagentchange: (agent: string) => void;
    oncancel: () => void;
    onrestart: () => void;
    /** Fires with the picked target, or null to clear impersonation. */
    onimpersonate?: (target: ImpersonationTarget | null) => void;
  } = $props();

  function onAgentSelect(e: Event): void {
    onagentchange((e.currentTarget as HTMLSelectElement).value);
  }

  function onImpersonationSelect(e: Event): void {
    const value = (e.currentTarget as HTMLSelectElement).value;
    if (value === '') {
      onimpersonate?.(null);
      return;
    }
    const target = impersonationTargets.find(
      (t) => `${t.tenant}/${t.user}/${t.session}` === value
    );
    onimpersonate?.(target ?? null);
  }
</script>

<div class="playground-header" data-testid="playground-header">
  <label class="header-field">
    <span class="field-label">Agent</span>
    <select
      class="header-select"
      data-testid="playground-agent-picker"
      value={activeAgent}
      onchange={onAgentSelect}
    >
      {#each agents as agent (agent)}
        <option value={agent}>{agent}</option>
      {/each}
    </select>
  </label>

  <span class="model-badge" data-testid="playground-model-badge">{model}</span>

  <span class="chip token-chip" data-testid="playground-token-chip">
    {tokenCount} tokens
  </span>
  <span class="chip cost-chip" data-testid="playground-cost-chip">
    ${costUSD.toFixed(4)}
  </span>

  {#if canImpersonate}
    <label class="header-field">
      <span class="field-label">Run as identity</span>
      <select
        class="header-select"
        data-testid="playground-impersonation-select"
        value={activeImpersonation
          ? `${activeImpersonation.tenant}/${activeImpersonation.user}/${activeImpersonation.session}`
          : ''}
        onchange={onImpersonationSelect}
      >
        <option value="">Self</option>
        {#each impersonationTargets as target (`${target.tenant}/${target.user}/${target.session}`)}
          <option value={`${target.tenant}/${target.user}/${target.session}`}>
            {target.label}
          </option>
        {/each}
      </select>
    </label>
  {/if}

  <button
    type="button"
    class="header-button"
    data-testid="playground-cancel-run"
    onclick={oncancel}
    disabled={!running}
    title={running ? 'Cancel the active run' : 'No active run to cancel'}
  >
    Cancel run
  </button>
  <button
    type="button"
    class="header-button"
    data-testid="playground-restart-run"
    onclick={onrestart}
  >
    Restart
  </button>
</div>

<style>
  .playground-header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .header-field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .field-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .header-select {
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .model-badge {
    padding: var(--space-1) var(--space-2);
    background: var(--color-accent-soft);
    color: var(--color-accent);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
  }

  .chip {
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .header-button {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .header-button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
