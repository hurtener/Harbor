<script lang="ts">
  // Harbor Console — Playground page header (Phase 108 / D-167).
  //
  // The agent sub-bar rendered inside the shared `<PageHeader>`'s
  // `actions` slot. Matches the binding mockup: a copyable session id +
  // the agent display name + a run-status pill + model / planner pills on
  // the left; impersonation + Cancel run + Restart on the right. The
  // token / cost numerics live in the KPI strip below, not here (one
  // component per concern — CONVENTIONS.md §3).
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
    sessionID,
    model,
    planner = '',
    running,
    paused = false,
    phase,
    canImpersonate = false,
    impersonationTargets = [],
    activeImpersonation = null,
    onagentchange: _onagentchange,
    oncancel,
    onpause,
    onrestart,
    onimpersonate
  }: {
    activeAgent: string;
    /** The session id (rendered mono, copy-on-click). */
    sessionID: string;
    /** The active model string (e.g. `anthropic/claude-haiku-4.5`). */
    model: string;
    /** The planner name, or '' when the runtime does not expose it. */
    planner?: string;
    /** True while a run is active — gates Cancel / Pause. */
    running: boolean;
    /** True when the active run is paused — flips the Pause button to Resume. */
    paused?: boolean;
    /** The live run phase, driving the status pill. */
    phase: 'streaming' | 'active' | 'idle';
    /** True when the operator has the `auth.ScopeAdmin` claim (D-079). */
    canImpersonate?: boolean;
    impersonationTargets?: ImpersonationTarget[];
    activeImpersonation?: ImpersonationTarget | null;
    onagentchange: (agent: string) => void;
    oncancel: () => void;
    /** Pause/resume toggle for the active run. */
    onpause?: () => void;
    onrestart: () => void;
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

  const statusMeta = $derived.by<{ kind: 'success' | 'info' | 'neutral'; label: string }>(() => {
    if (phase === 'streaming') return { kind: 'info', label: 'Streaming' };
    if (phase === 'active') return { kind: 'success', label: 'Active' };
    return { kind: 'neutral', label: 'Ready' };
  });

  let copied = $state(false);
  async function copySession(): Promise<void> {
    try {
      await navigator.clipboard.writeText(sessionID);
      copied = true;
      setTimeout(() => (copied = false), 1200);
    } catch {
      /* clipboard unavailable — no-op */
    }
  }
</script>

<div class="playground-header" data-testid="playground-header">
  <div class="header-left">
    <button
      type="button"
      class="session-id mono"
      title={copied ? 'Copied!' : 'Copy session id'}
      data-testid="playground-session-id"
      onclick={() => void copySession()}
    >
      {sessionID || '—'}
    </button>
    <span class="agent-name" data-testid="playground-agent-name">{activeAgent}</span>
    <StatusChip kind={statusMeta.kind} label={statusMeta.label} />
    {#if model && model !== '—'}
      <span class="meta-pill" title="Model">{model}</span>
    {/if}
    {#if planner}
      <span class="meta-pill" title="Planner">{planner}</span>
    {/if}
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
      class="header-button"
      data-testid="playground-pause-run"
      onclick={() => onpause?.()}
      disabled={!running}
      title={running ? (paused ? 'Resume the run' : 'Pause the run') : 'No active run'}
    >
      {paused ? 'Resume' : 'Pause'}
    </button>
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
  .header-right {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .session-id {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
    max-width: var(--size-session-max-width);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .session-id:hover {
    color: var(--color-text);
    border-color: var(--color-accent);
  }

  .agent-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .meta-pill {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text-muted);
  }

  .header-field {
    display: flex;
    align-items: center;
    gap: var(--space-2);
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

  .header-button.danger:not(:disabled):hover {
    background: var(--color-danger-soft);
  }

  .header-button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
