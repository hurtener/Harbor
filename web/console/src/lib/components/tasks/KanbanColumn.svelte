<script lang="ts">
  // Harbor Console — Tasks page kanban column (Phase 73d / D-123).
  //
  // One status column on the kanban board. It accepts a card drag-drop
  // and emits the matching shipped Phase 54 control verb (the page wires
  // Running → Paused = `pause`, Paused → Running = `resume`,
  // Running → Failed = `cancel`; Pending → Running is a server-initiated
  // transition — the page renders a "transitions automatically" toast).
  // Page-specific; lives in `components/tasks/` per CONVENTIONS.md §3.
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { Snippet } from 'svelte';
  import type { TaskStatus } from '$lib/protocol/tasks.js';

  let {
    status,
    label,
    count,
    cards,
    ondropcard
  }: {
    /** The lifecycle status this column holds. */
    status: TaskStatus;
    /** The column header label. */
    label: string;
    /** The aggregate count for this column (filtered view). */
    count: number;
    /** The rendered cards (a snippet the page supplies). */
    cards: Snippet;
    /** Emitted when a card is dropped onto this column. */
    ondropcard?: (taskID: string, fromStatus: TaskStatus, toStatus: TaskStatus) => void;
  } = $props();

  let dragOver = $state(false);

  function onDragOver(e: DragEvent): void {
    e.preventDefault();
    dragOver = true;
  }

  function onDrop(e: DragEvent): void {
    e.preventDefault();
    dragOver = false;
    const raw = e.dataTransfer?.getData('application/x-harbor-task');
    if (!raw) return;
    let parsed: { id: string; status: TaskStatus };
    try {
      parsed = JSON.parse(raw) as { id: string; status: TaskStatus };
    } catch {
      return;
    }
    if (parsed.status === status) return;
    ondropcard?.(parsed.id, parsed.status, status);
  }
</script>

<section
  class="kanban-column"
  class:drag-over={dragOver}
  data-testid="kanban-column"
  data-status={status}
  role="list"
  aria-label={`${label} tasks — drop a card here to transition its status`}
  ondragover={onDragOver}
  ondragleave={() => (dragOver = false)}
  ondrop={onDrop}
>
  <header class="col-head">
    <h3 class="col-title">{label}</h3>
    <span class="col-count" data-testid="kanban-column-count">{count}</span>
  </header>
  <div class="col-body">
    {@render cards()}
  </div>
</section>

<style>
  .kanban-column {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    min-height: var(--space-12);
  }

  .kanban-column.drag-over {
    border-color: var(--color-accent);
  }

  .col-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .col-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .col-count {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .col-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
</style>
