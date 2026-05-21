<script lang="ts">
  // Harbor Console — Background Jobs page orphan badge (Phase 73h /
  // D-128).
  //
  // Renders the `AwaitTask` orphan badge on a queue row whose parent
  // task is no longer alive, and — on click — surfaces a diagnostic
  // dialog explaining the orphan. The badge is the UI surface for the
  // §13 binding that `SpawnTask` + `AwaitTask` MUST emit in the same
  // phase (Phase 47 / D-056 closed this for ReAct). Page-specific —
  // lives in `components/background-jobs/`. Svelte 5 runes (D-092);
  // design tokens only.
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    row,
    orphan
  }: {
    /** The queue row this badge decorates. */
    row: TaskRow;
    /** Whether the row was flagged by the Console-side orphan detector. */
    orphan: boolean;
  } = $props();

  let dialogOpen = $state(false);
</script>

{#if orphan}
  <button
    type="button"
    class="orphan-badge"
    data-testid="orphan-badge"
    title="This background job's parent task is no longer alive — a SpawnTask that was never joined via AwaitTask. Click for diagnostics."
    onclick={(e) => {
      e.stopPropagation();
      dialogOpen = true;
    }}
  >
    Orphan
  </button>

  {#if dialogOpen}
    <div class="dialog-backdrop">
      <div
        class="dialog"
        role="dialog"
        aria-modal="true"
        aria-label="Orphan diagnostics"
        data-testid="orphan-dialog"
        tabindex="-1"
        onclick={(e) => e.stopPropagation()}
        onkeydown={(e) => {
          if (e.key === 'Escape') dialogOpen = false;
        }}
      >
        <h3 class="dialog-title">Orphaned background job</h3>
        <p class="dialog-body">
          Job <code>{row.id}</code> declares parent task
          <code>{row.parent_task_id}</code>, which is no longer present in
          the runtime's active-task set. The planner spawned this job via
          <code>SpawnTask</code> but the parent finished, failed, or was
          cancelled before joining it via <code>AwaitTask</code>.
        </p>
        <p class="dialog-note">
          The runtime cannot recover orphan work automatically — cancel the
          job from the bulk-action toolbar, or inspect its progress in the
          right rail before deciding.
        </p>
        <button
          type="button"
          class="dialog-close"
          data-testid="orphan-dialog-close"
          onclick={() => (dialogOpen = false)}
        >
          Close
        </button>
      </div>
    </div>
  {/if}
{/if}

<style>
  .orphan-badge {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-danger);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-color: var(--color-danger);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }

  .dialog-backdrop {
    position: fixed;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--color-overlay);
  }

  .dialog {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    width: var(--size-modal-width);
    max-width: 90vw;
    padding: var(--space-5);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .dialog-title {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .dialog-body,
  .dialog-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .dialog code {
    font-family: var(--font-mono);
    color: var(--color-text);
  }

  .dialog-close {
    align-self: flex-end;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    cursor: pointer;
  }
</style>
