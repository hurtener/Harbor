<script lang="ts">
  // Harbor Console — Background Jobs page bulk-action toolbar (Phase
  // 73h / D-128).
  //
  // The bulk-action controls shown inside the shared `ui/BulkActionBar`
  // when ≥1 queue row is selected: Cancel / Pause / Resume / Prioritize.
  // Each one invokes the SHIPPED Phase 54 control verb ONCE PER selected
  // row — there is NO bulk endpoint (a single-call bulk endpoint would
  // be a §13 parallel implementation; D-128 reaffirms this). When the
  // operator's connection lacks the control scope claim every action
  // renders disabled-with-tooltip (CONVENTIONS.md §5 — no stubbed action
  // presented as done). Page-specific — lives in
  // `components/background-jobs/`. Svelte 5 runes (D-092); tokens only.

  let {
    canControl,
    pending,
    onverb,
    onprioritize
  }: {
    /** Whether the connection carries the control scope claim (D-079). */
    canControl: boolean;
    /** Whether a bulk dispatch is currently in flight. */
    pending: boolean;
    /** Invokes a per-row Phase 54 control verb across the selection. */
    onverb: (verb: 'cancel' | 'pause' | 'resume') => void;
    /** Invokes the per-row `prioritize` verb across the selection. */
    onprioritize: (priority: number) => void;
  } = $props();

  // The control-scope-missing tooltip — task control is an elevated
  // tier (D-079 / D-066). Disabled-with-tooltip, never a fake success.
  const SCOPE_TIP =
    'Requires the `tasks.control` scope claim — task control is an elevated tier (D-066 / D-079).';

  let priorityInput = $state(0);
</script>

<div class="bulk-toolbar" data-testid="bg-bulk-toolbar">
  <button
    type="button"
    class="control"
    data-testid="bg-bulk-cancel"
    disabled={!canControl || pending}
    title={canControl ? undefined : SCOPE_TIP}
    onclick={() => onverb('cancel')}
  >
    Cancel
  </button>
  <button
    type="button"
    class="control"
    data-testid="bg-bulk-pause"
    disabled={!canControl || pending}
    title={canControl ? undefined : SCOPE_TIP}
    onclick={() => onverb('pause')}
  >
    Pause
  </button>
  <button
    type="button"
    class="control"
    data-testid="bg-bulk-resume"
    disabled={!canControl || pending}
    title={canControl ? undefined : SCOPE_TIP}
    onclick={() => onverb('resume')}
  >
    Resume
  </button>
  <span class="prioritize-group">
    <input
      class="control priority-input"
      type="number"
      min="0"
      max="100"
      bind:value={priorityInput}
      data-testid="bg-bulk-priority-input"
      disabled={!canControl || pending}
      title={canControl ? 'Task-level priority (D-072)' : SCOPE_TIP}
    />
    <button
      type="button"
      class="control"
      data-testid="bg-bulk-prioritize"
      disabled={!canControl || pending}
      title={canControl ? undefined : SCOPE_TIP}
      onclick={() => onprioritize(priorityInput)}
    >
      Prioritize
    </button>
  </span>
</div>

<style>
  .bulk-toolbar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .prioritize-group {
    display: inline-flex;
    gap: var(--space-1);
    align-items: center;
  }

  .priority-input {
    width: var(--size-bar-min);
  }
</style>
