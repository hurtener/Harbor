<script lang="ts">
  // Harbor Console — Tasks page kanban board (Phase 73d / D-123).
  //
  // The Tasks page's PRIMARY view (in place of a flat DataTable) — a
  // 4-column board (Pending / Running / Paused / Failed). Cards drag
  // across columns to invoke the matching shipped Phase 54 control verb
  // (D-072 — NOT priority drag-reordering). Page-specific; composes the
  // `KanbanColumn` + `TaskCard` pieces in `components/tasks/`.
  // Svelte 5 runes mode (D-092); design tokens only.
  import KanbanColumn from './KanbanColumn.svelte';
  import TaskCard from './TaskCard.svelte';
  import {
    KANBAN_COLUMNS,
    type TaskRow,
    type TaskStatus,
    type TaskListAggregates
  } from '$lib/protocol/tasks.js';

  let {
    rows,
    aggregates,
    selected = new Set<string>(),
    activeId = null,
    onselect,
    ontoggle,
    ondropcard
  }: {
    /** The task rows to bucket into columns. */
    rows: TaskRow[];
    /** The per-status aggregate counters for the filtered view. */
    aggregates: TaskListAggregates;
    /** The bulk-action selection set. */
    selected?: Set<string>;
    /** The open detail target id. */
    activeId?: string | null;
    /** Emitted when a card is opened. */
    onselect?: (id: string) => void;
    /** Emitted when a card's selection checkbox toggles. */
    ontoggle?: (id: string, selected: boolean) => void;
    /** Emitted when a card is dragged across columns. */
    ondropcard?: (taskID: string, fromStatus: TaskStatus, toStatus: TaskStatus) => void;
  } = $props();

  function columnRows(status: TaskStatus): TaskRow[] {
    return rows.filter((r) => r.status === status);
  }

  function columnCount(status: TaskStatus): number {
    switch (status) {
      case 'pending':
        return aggregates.pending;
      case 'running':
        return aggregates.running;
      case 'paused':
        return aggregates.paused;
      case 'complete':
        // W7 (Phase 83x): the Complete column reads `aggregates.complete`
        // — the same counter the right-rail summary already shows. The
        // two surfaces now agree.
        return aggregates.complete;
      case 'failed':
        return aggregates.failed;
      default:
        return 0;
    }
  }

  function onCardDragStart(e: DragEvent, task: TaskRow): void {
    e.dataTransfer?.setData(
      'application/x-harbor-task',
      JSON.stringify({ id: task.id, status: task.status })
    );
  }
</script>

<div class="kanban-board" data-testid="kanban-board">
  {#each KANBAN_COLUMNS as col (col.status)}
    <KanbanColumn
      status={col.status}
      label={col.label}
      count={columnCount(col.status)}
      {ondropcard}
    >
      {#snippet cards()}
        {#each columnRows(col.status) as task (task.id)}
          <div
            class="card-drag"
            role="listitem"
            draggable="true"
            ondragstart={(e) => onCardDragStart(e, task)}
          >
            <TaskCard
              {task}
              selected={selected.has(task.id)}
              active={activeId === task.id}
              {onselect}
              {ontoggle}
            />
          </div>
        {/each}
      {/snippet}
    </KanbanColumn>
  {/each}
</div>

<style>
  .kanban-board {
    /* W7 (Phase 83x): the board now carries five columns
       (Pending / Running / Paused / Complete / Failed). */
    display: grid;
    grid-template-columns: repeat(5, 1fr);
    gap: var(--space-3);
    align-items: start;
  }

  .card-drag {
    cursor: grab;
  }
</style>
