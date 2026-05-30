<script lang="ts">
  // Harbor Console — Playground Pending Interventions card (Phase 73n /
  // D-130, Phase 108 / D-167).
  //
  // The right-rail card listing HITL gates awaiting an operator
  // decision. Phase 108 adds: red count badge in header, avatar per row,
  // intent-coloured title, Approve/Reject button intents.
  //
  // Design tokens only.

  import StatusChip from '$lib/components/ui/StatusChip.svelte';

  /** A pending HITL intervention awaiting a decision. */
  export interface PendingIntervention {
    runID: string;
    reason: string;
    /** The source event that created this intervention. */
    source: 'tool.approval_requested' | 'tool.auth_required' | 'pause.requested';
  }

  function sourceToIntent(source: PendingIntervention['source']): 'warning' | 'accent' | 'danger' {
    switch (source) {
      case 'tool.approval_requested':
        return 'warning';
      case 'tool.auth_required':
        return 'accent';
      case 'pause.requested':
        return 'danger';
    }
  }

  function sourceToLabel(source: PendingIntervention['source']): string {
    switch (source) {
      case 'tool.approval_requested':
        return 'Approval';
      case 'tool.auth_required':
        return 'OAuth';
      case 'pause.requested':
        return 'HITL';
    }
  }

  let {
    interventions,
    canDecide = true,
    onapprove,
    onreject
  }: {
    interventions: PendingIntervention[];
    /** True when the operator carries the steering scope claim. */
    canDecide?: boolean;
    onapprove: (runID: string) => void;
    onreject: (runID: string) => void;
  } = $props();
</script>

<div class="interventions-card" data-testid="playground-interventions-card">
  <div class="card-header">
    <span class="card-title">Pending Interventions</span>
    {#if interventions.length > 0}
      <span class="count-badge" data-testid="interventions-count">
        {interventions.length}
      </span>
    {/if}
  </div>

  {#if interventions.length === 0}
    <p class="empty-note" data-testid="interventions-empty">
      No pending interventions.
    </p>
  {:else}
    <ul class="intervention-list">
      {#each interventions as item (item.runID)}
        <li class="intervention" data-run-id={item.runID}>
          <div class="intervention-meta">
            <div class="intervention-avatar-row">
              <span
                class="intervention-avatar"
                style:background="var(--chip-{sourceToIntent(item.source)}-bg)"
                style:color="var(--chip-{sourceToIntent(item.source)}-fg)"
              >
                {sourceToLabel(item.source)[0]}
              </span>
              <StatusChip
                kind={sourceToIntent(item.source)}
                label={sourceToLabel(item.source)}
              />
            </div>
            <span class="intervention-reason">{item.reason}</span>
          </div>
          <div class="intervention-actions">
            <button
              type="button"
              class="action approve"
              data-testid="intervention-approve"
              onclick={() => onapprove(item.runID)}
              disabled={!canDecide}
              title={canDecide
                ? 'Approve this intervention'
                : 'Requires a steering scope claim'}
            >
              Approve
            </button>
            <button
              type="button"
              class="action reject"
              data-testid="intervention-reject"
              onclick={() => onreject(item.runID)}
              disabled={!canDecide}
              title={canDecide
                ? 'Reject this intervention'
                : 'Requires a steering scope claim'}
            >
              Reject
            </button>
          </div>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .interventions-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .card-header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .card-title {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .count-badge {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-badge-sm);
    height: var(--size-badge-sm);
    border-radius: 50%;
    background: var(--color-danger);
    color: var(--color-bg);
    font-size: var(--text-xs);
    font-weight: 600;
  }

  .empty-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .intervention-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .intervention {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .intervention-meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .intervention-avatar-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .intervention-avatar {
    width: var(--size-avatar-sm);
    height: var(--size-avatar-sm);
    border-radius: 50%;
    display: flex;
    align-items: center;
    justify-content: center;
    font-size: var(--text-xs);
    font-weight: 600;
    flex-shrink: 0;
  }

  .intervention-reason {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .intervention-actions {
    display: flex;
    gap: var(--space-2);
  }

  .action {
    flex: 1;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .action.approve {
    background: var(--chip-success-bg);
    color: var(--chip-success-fg);
    border-color: var(--chip-success-border);
  }

  .action.reject {
    background: var(--chip-danger-bg);
    color: var(--chip-danger-fg);
    border-color: var(--chip-danger-border);
  }

  .action:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
