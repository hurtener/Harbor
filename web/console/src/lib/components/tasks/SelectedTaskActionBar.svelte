<script lang="ts">
  // Harbor Console — Tasks page selected-task action bar (Phase 73d /
  // D-123).
  //
  // The per-task control bar: the six shipped Phase 54 verbs (`cancel` /
  // `pause` / `resume` / `prioritize` / `approve` / `reject`). Each
  // button either invokes the REAL control method via `client.control.*`
  // or renders disabled-with-tooltip when the connection lacks the
  // control scope claim (CONVENTIONS.md §5 — no stubbed action presented
  // as done). NO new control verb is minted (CLAUDE.md §13). Page-
  // specific; lives in `components/tasks/`. Svelte 5 runes (D-092);
  // design tokens only.
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    task,
    canControl,
    pending = false,
    result = null,
    onverb,
    onprioritize
  }: {
    /** The currently-open task; null hides the bar. */
    task: TaskRow | null;
    /** Whether the connection carries the control scope claim (D-079). */
    canControl: boolean;
    /** Whether a control call is in flight. */
    pending?: boolean;
    /** The last action's inline result. */
    result?: { ok: boolean; message: string } | null;
    /** Emitted for a plain control verb. */
    onverb?: (verb: 'cancel' | 'pause' | 'resume' | 'approve' | 'reject', id: string) => void;
    /** Emitted for a prioritize action with the composed priority. */
    onprioritize?: (id: string, priority: number) => void;
  } = $props();

  let priorityInput = $state(0);

  const scopeTip =
    'Requires the control scope claim — task control is an elevated tier (D-079).';

  function submitPriority(): void {
    if (task === null) return;
    onprioritize?.(task.id, Math.trunc(priorityInput));
  }
</script>

{#if task !== null}
  <div class="action-bar" data-testid="task-action-bar" role="toolbar" aria-label="Task controls">
    <span class="bar-label">Task {task.id}</span>
    {#each ['pause', 'resume', 'cancel', 'approve', 'reject'] as const as verb (verb)}
      <button
        type="button"
        class="control"
        data-testid={`task-verb-${verb}`}
        disabled={!canControl || pending}
        title={canControl ? undefined : scopeTip}
        onclick={() => onverb?.(verb, task.id)}
      >
        {verb}
      </button>
    {/each}
    <span class="prioritize-group">
      <input
        type="number"
        class="control priority-input"
        data-testid="task-priority-input"
        bind:value={priorityInput}
        disabled={!canControl || pending}
        aria-label="Task priority"
      />
      <button
        type="button"
        class="control"
        data-testid="task-verb-prioritize"
        disabled={!canControl || pending}
        title={canControl ? undefined : scopeTip}
        onclick={submitPriority}
      >
        prioritize
      </button>
    </span>
  </div>
  {#if result !== null}
    <p
      class="inline-result"
      class:ok={result.ok}
      class:err={!result.ok}
      data-testid="task-action-result"
    >
      {result.message}
    </p>
  {/if}
{/if}

<style>
  .action-bar {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .bar-label {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
    margin-right: var(--space-2);
  }

  .control {
    background: var(--color-surface);
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

  .priority-input {
    width: var(--space-12);
  }

  .prioritize-group {
    display: flex;
    align-items: center;
    gap: var(--space-1);
  }

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
  }
</style>
