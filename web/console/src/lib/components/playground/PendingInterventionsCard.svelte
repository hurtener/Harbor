<script lang="ts">
  // Harbor Console — Playground Pending Interventions card (Phase 73n /
  // D-130).
  //
  // The right-rail card listing HITL gates awaiting an operator
  // decision. Approve / Reject invoke the SHIPPED Phase 54 `approve` /
  // `reject` control verbs (no parallel approval protocol — CLAUDE.md
  // §13). When the operator lacks the steering scope claim the buttons
  // render disabled-with-tooltip (CONVENTIONS.md §5).
  //
  // Design tokens only.

  /** A pending HITL intervention awaiting a decision. */
  export interface PendingIntervention {
    runID: string;
    reason: string;
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
  {#if interventions.length === 0}
    <p class="empty-note" data-testid="interventions-empty">
      No pending interventions.
    </p>
  {:else}
    <ul class="intervention-list">
      {#each interventions as item (item.runID)}
        <li class="intervention" data-run-id={item.runID}>
          <div class="intervention-meta">
            <span class="intervention-run">{item.runID}</span>
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

  .intervention-run {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
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
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .action.reject {
    background: var(--color-danger-soft);
    color: var(--color-danger);
  }

  .action:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }
</style>
