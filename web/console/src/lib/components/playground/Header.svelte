<script lang="ts">
  // Harbor Console — Playground page header (Phase 108 / D-167).
  //
  // The page-specific header strip rendered inside the shared
  // `<PageHeader>`'s `actions` slot. Restructured to match the mock:
  // breadcrumb-prefixed session id on the left; agent display name +
  // status pill + planner pill in the middle-left; cost chip + token
  // chip on the right; Cancel run + Restart buttons rightmost.
  //
  // Design tokens only; no raw literals.

  import StatusChip from '$lib/components/ui/StatusChip.svelte';

  /** An impersonation target an admin can run-as. */
  export interface ImpersonationTarget {
    tenant: string;
    user: string;
    session: string;
    label: string;
  }

  let {
    activeAgent,
    model,
    tokenCount,
    costUSD,
    running,
    canImpersonate = false,
    impersonationTargets = [],
    activeImpersonation = null,
    onagentchange: _onagentchange,
    oncancel,
    onrestart,
    onimpersonate
  }: {
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

  const statusKind = $derived<'success' | 'warning' | 'danger' | 'neutral'>(
    running ? 'success' : 'neutral'
  );
  const statusLabel = $derived(running ? 'Active' : 'Ready');
</script>

<div class="playground-header" data-testid="playground-header">
  <div class="header-left">
    <span class="session-id mono" title="Session ID">
      {activeAgent}
    </span>
    <StatusChip kind={statusKind} label={statusLabel} />
    <StatusChip kind="accent" label={model} />
  </div>

  <div class="header-center">
    <span class="chip token-chip tabular" data-testid="playground-token-chip">
      {tokenCount.toLocaleString()} tokens
    </span>
    <span class="chip cost-chip tabular" data-testid="playground-cost-chip">
      ${costUSD.toFixed(4)}
    </span>
  </div>

  <div class="header-right">
    {#if canImpersonate}
      <label class="header-field">
        <span class="field-label">Run as</span>
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
      class="header-button danger"
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
</div>

<style>
  .playground-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
    width: 100%;
  }

  .header-left,
  .header-center,
  .header-right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .session-id {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
  }

  .chip {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .token-chip,
  .cost-chip {
    font-family: var(--font-mono);
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

  .header-button {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .header-button.danger {
    color: var(--color-danger);
    border-color: var(--color-danger);
  }

  .header-button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
