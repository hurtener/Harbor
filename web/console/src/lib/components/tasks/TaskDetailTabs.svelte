<script lang="ts">
  // Harbor Console — Tasks page per-task detail tabs (Phase 73d /
  // D-123).
  //
  // The bottom-dock tab strip: Metadata | Inputs | Logs | Events |
  // Errors | Output. Renders the `tasks.get` enriched detail. Page-
  // specific; lives in `components/tasks/`. Svelte 5 runes (D-092);
  // design tokens only.
  import type { TaskDetail } from '$lib/protocol/tasks.js';

  let {
    detail,
    loading = false
  }: {
    /** The enriched task detail; null when no task is open. */
    detail: TaskDetail | null;
    /** Whether the detail load is in flight. */
    loading?: boolean;
  } = $props();

  type Tab = 'metadata' | 'inputs' | 'logs' | 'events' | 'errors' | 'output';
  const TABS: { id: Tab; label: string }[] = [
    { id: 'metadata', label: 'Metadata' },
    { id: 'inputs', label: 'Inputs' },
    { id: 'logs', label: 'Logs' },
    { id: 'events', label: 'Events' },
    { id: 'errors', label: 'Errors' },
    { id: 'output', label: 'Output' }
  ];
  let active = $state<Tab>('metadata');
</script>

{#if detail !== null || loading}
  <section class="detail-tabs" data-testid="task-detail-tabs">
    <div class="tab-strip" role="tablist">
      {#each TABS as tab (tab.id)}
        <button
          type="button"
          class="tab"
          class:active={active === tab.id}
          role="tab"
          aria-selected={active === tab.id}
          data-testid={`task-tab-${tab.id}`}
          onclick={() => (active = tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </div>

    <div class="tab-body" role="tabpanel">
      {#if loading}
        <p class="muted">Loading task detail…</p>
      {:else if detail !== null}
        {#if active === 'metadata'}
          <dl class="meta">
            <div><dt>ID</dt><dd>{detail.task.id}</dd></div>
            <div><dt>Kind</dt><dd>{detail.task.kind}</dd></div>
            <div><dt>Status</dt><dd>{detail.task.status}</dd></div>
            <div><dt>Priority</dt><dd>{detail.task.priority}</dd></div>
            <div><dt>Started</dt><dd>{detail.task.started_at}</dd></div>
            <div><dt>Updated</dt><dd>{detail.task.updated_at}</dd></div>
            <div><dt>Parent session</dt><dd>{detail.task.parent_session_id}</dd></div>
            {#if detail.task.parent_task_id}
              <div><dt>Parent task</dt><dd>{detail.task.parent_task_id}</dd></div>
            {/if}
            {#if detail.planner_snapshot}
              <div>
                <dt>Planner checkpoint</dt>
                <dd>{detail.planner_snapshot.checkpoint_id}</dd>
              </div>
            {/if}
          </dl>
        {:else if active === 'inputs'}
          <pre class="code" data-testid="task-tab-content">{detail.task.query || '(no query)'}</pre>
        {:else if active === 'logs'}
          <p class="muted" data-testid="task-tab-content">
            Per-task logs stream from the event bus — see the Events tab.
          </p>
        {:else if active === 'events'}
          <p class="muted" data-testid="task-tab-content">
            {detail.task.tool_count} tool task(s) spawned. Live `task.*`
            event deltas surface on the kanban board.
          </p>
        {:else if active === 'errors'}
          {#if detail.task.error_class}
            <p class="err" data-testid="task-tab-content">{detail.task.error_class}</p>
          {:else}
            <p class="muted" data-testid="task-tab-content">No errors recorded.</p>
          {/if}
        {:else if active === 'output'}
          {#if detail.result_ref}
            <p class="muted" data-testid="task-tab-content">
              Result is a {detail.result_ref.size_bytes}-byte artifact
              ({detail.result_ref.id}) — fetched by reference (D-026).
            </p>
          {:else if detail.result_inline}
            <pre class="code" data-testid="task-tab-content">{detail.result_inline}</pre>
          {:else}
            <p class="muted" data-testid="task-tab-content">No output recorded.</p>
          {/if}
        {/if}
      {/if}
    </div>
  </section>
{/if}

<style>
  .detail-tabs {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
  }

  .tab-strip {
    display: flex;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .tab {
    background: transparent;
    border: none;
    border-bottom: var(--border-hairline);
    border-color: transparent;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .tab-body {
    padding: var(--space-3);
  }

  .meta {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2);
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

  .code {
    margin: var(--space-0);
    font-size: var(--text-xs);
    white-space: pre-wrap;
    word-break: break-word;
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .err {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-danger);
  }
</style>
