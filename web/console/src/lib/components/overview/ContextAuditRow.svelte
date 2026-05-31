<script lang="ts">
  // Harbor Console — Overview context + audit row (Phase 108c).
  //
  // The slim row-1 of the canvas. Per the operator chrome decision, the runtime
  // NAME / version / Protocol live in the app-shell chrome (top bar + bottom
  // AppStatusBar), so this row carries ONLY the parts the chrome does not: a
  // subsystem-health pill (from `runtime.health`) and the audit ribbon — the
  // count of `audit.admin_scope_used` events in the window, deep-linking to the
  // Events page (page-overview.md §5, `[shipped]`). A Refresh control sits at
  // the right (the mock's top-bar action; rebuilt here since the page dropped
  // the PageHeader). Real data only — empty audit count renders "0".
  //
  // Svelte 5 runes (D-092); design tokens only (CLAUDE.md §4.5).
  import { goto } from '$app/navigation';
  import Activity from '@lucide/svelte/icons/activity';
  import ShieldCheck from '@lucide/svelte/icons/shield-check';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import type { RuntimeHealth } from '$lib/protocol/posture.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';

  let {
    health,
    auditCount,
    disconnected,
    onRefresh
  }: {
    health: RuntimeHealth | null;
    /** Count of audit.admin_scope_used events in the window. */
    auditCount: number;
    disconnected: boolean;
    onRefresh: () => void;
  } = $props();

  // Aggregate health: all subsystems ready → healthy; any non-ready → degraded.
  const allReady = $derived(
    (health?.subsystems ?? []).length > 0 &&
      (health?.subsystems ?? []).every((s) => s.status === 'ready')
  );
  const degradedCount = $derived(
    (health?.subsystems ?? []).filter((s) => s.status !== 'ready').length
  );
</script>

<div class="context-row" data-testid="overview-context-row">
  <div class="left">
    <span class="health-pill" data-state={allReady ? 'healthy' : 'degraded'} data-testid="overview-health-pill">
      <Activity size={14} aria-hidden="true" />
      {#if health === null}
        runtime health unavailable
      {:else if allReady}
        all subsystems ready
      {:else}
        {degradedCount} subsystem{degradedCount === 1 ? '' : 's'} degraded
      {/if}
    </span>

    <button
      type="button"
      class="audit-ribbon"
      data-testid="overview-audit-ribbon"
      onclick={() => void goto('/events?type=audit.admin_scope_used')}
    >
      <ShieldCheck size={14} aria-hidden="true" />
      Audit: admin scope used <strong>{auditCount}×</strong> (24h) · View in Events →
    </button>
  </div>

  <button
    type="button"
    class="refresh"
    data-testid="overview-refresh"
    disabled={disconnected}
    title={disconnected ? DISCONNECTED_TOOLTIP : 'Refresh'}
    onclick={onRefresh}
  >
    <RotateCw size={14} aria-hidden="true" /> Refresh
  </button>
</div>

<style>
  .context-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .left {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .health-pill {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-pill);
    border: var(--border-hairline);
    font-size: var(--text-xs);
    color: var(--color-success);
    border-color: var(--color-success);
    background: var(--color-success-soft);
  }

  .health-pill[data-state='degraded'] {
    color: var(--color-warning);
    border-color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .audit-ribbon {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    background: transparent;
    border: none;
    cursor: pointer;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .audit-ribbon:hover {
    color: var(--color-text);
  }

  .audit-ribbon strong {
    color: var(--color-text);
  }

  .refresh {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .refresh:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
