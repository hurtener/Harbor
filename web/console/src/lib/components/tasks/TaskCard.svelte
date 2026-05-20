<script lang="ts">
  // Harbor Console — Tasks page kanban card (Phase 73d / D-123).
  //
  // One task on the kanban board. Page-specific (it composes the shared
  // `StatusChip` ui/ primitive underneath); lives in `components/tasks/`
  // per CONVENTIONS.md §3. Svelte 5 runes mode (D-092); design tokens
  // only — no raw color / spacing literals (CONVENTIONS.md §7).
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    task,
    selected = false,
    active = false,
    onselect,
    ontoggle
  }: {
    /** The task this card renders. */
    task: TaskRow;
    /** Whether the card's checkbox is selected (bulk-action model). */
    selected?: boolean;
    /** Whether this card is the open detail target. */
    active?: boolean;
    /** Emitted when the card body is clicked — opens the detail. */
    onselect?: (id: string) => void;
    /** Emitted when the card's selection checkbox toggles. */
    ontoggle?: (id: string, selected: boolean) => void;
  } = $props();

  const statusKinds: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  function durationLabel(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.round(s / 60);
    if (m < 60) return `${m}m`;
    return `${Math.round(m / 60)}h`;
  }

  function shortIdentity(): string {
    return `${task.identity.tenant}/${task.identity.user}`;
  }
</script>

<div
  class="task-card"
  class:active
  data-testid="task-card"
  data-task-id={task.id}
  data-status={task.status}
>
  <div class="card-head">
    <input
      type="checkbox"
      class="card-check"
      data-testid="task-card-check"
      checked={selected}
      onclick={(e) => e.stopPropagation()}
      onchange={(e) => ontoggle?.(task.id, (e.currentTarget as HTMLInputElement).checked)}
      aria-label={`Select task ${task.id}`}
    />
    <StatusChip kind={statusKinds[task.status] ?? 'neutral'} label={task.status} />
    <span class="kind" data-testid="task-card-kind">{task.kind}</span>
  </div>

  <button
    type="button"
    class="card-body"
    data-testid="task-card-open"
    onclick={() => onselect?.(task.id)}
  >
    <p class="desc">{task.description || task.query || task.id}</p>
    <dl class="meta">
      <div><dt>Priority</dt><dd>{task.priority}</dd></div>
      <div><dt>Duration</dt><dd>{durationLabel(task.duration_ms)}</dd></div>
      <div><dt>Tools</dt><dd>{task.tool_count}</dd></div>
      <div><dt>Identity</dt><dd class="ellip">{shortIdentity()}</dd></div>
    </dl>
    {#if task.error_class}
      <p class="err" data-testid="task-card-error">{task.error_class}</p>
    {/if}
    {#if task.parent_task_id}
      <p class="parent" data-testid="task-card-parent">child of {task.parent_task_id}</p>
    {/if}
    {#if task.background_acknowledged}
      <span class="ack-badge" data-testid="task-card-ack">background acknowledged</span>
    {/if}
  </button>
</div>

<style>
  .task-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .task-card.active {
    border-color: var(--color-accent);
  }

  .card-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .kind {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    margin-left: auto;
  }

  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: transparent;
    border: none;
    padding: var(--space-0);
    text-align: left;
    cursor: pointer;
    color: var(--color-text);
  }

  .desc {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
  }

  .meta {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-1);
    margin: var(--space-0);
  }

  .meta div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .meta dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .meta dd {
    margin: var(--space-0);
    font-size: var(--text-xs);
  }

  .ellip {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: var(--size-search-min);
  }

  .err {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }

  .parent {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .ack-badge {
    align-self: flex-start;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }
</style>
