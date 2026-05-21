<script lang="ts" module>
  // Harbor Console — Live Runtime per-task detail pane (Phase 73b /
  // D-126).
  //
  // The bottom-dock right pane when a topology node is selected (Brief
  // 11 §LR-6): a five-tab detail — Details / Input / Output / Logs /
  // Trace. Details/Input/Output consume the shipped `tasks.get` (Phase
  // 73d); the Trace tab consumes the run-scoped event stream the page
  // owns. The pane does NOT consume the canonical chat module — when no
  // node is selected the page renders the Skeleton-primitive composer
  // instead (D-091 encapsulate-first rule).
  //
  // Task-level priority via the shipped `prioritize` method is exposed
  // here on the action menu — NEVER a session-level priority (D-065).
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { TaskDetail } from '$lib/protocol/tasks.js';
  import type { Event } from '$lib/protocol/events.js';

  /** The closed set of per-task detail tabs. */
  export type DetailTab = 'details' | 'input' | 'output' | 'logs' | 'trace';
</script>

<script lang="ts">
  let {
    detail,
    loading,
    traceEvents,
    canControl,
    onprioritize
  }: {
    /** The `tasks.get` detail for the selected node's task, if loaded. */
    detail: TaskDetail | null;
    /** Whether the detail is in flight. */
    loading: boolean;
    /** The run-scoped event slice the Trace tab renders. */
    traceEvents: Event[];
    /** Whether the connection carries the control scope claim (D-079). */
    canControl: boolean;
    /** Emitted with (taskID, priority) — the shipped Phase 54 verb. */
    onprioritize: (taskID: string, priority: number) => void;
  } = $props();

  let activeTab = $state<DetailTab>('details');
  const tabs: { id: DetailTab; label: string }[] = [
    { id: 'details', label: 'Details' },
    { id: 'input', label: 'Input' },
    { id: 'output', label: 'Output' },
    { id: 'logs', label: 'Logs' },
    { id: 'trace', label: 'Trace' }
  ];
</script>

<section class="detail-pane" data-testid="per-task-detail-pane">
  <nav class="detail-tabs" aria-label="Task detail">
    {#each tabs as tab (tab.id)}
      <button
        type="button"
        class="dtab"
        class:on={tab.id === activeTab}
        data-testid={`detail-tab-${tab.id}`}
        onclick={() => (activeTab = tab.id)}
      >
        {tab.label}
      </button>
    {/each}
  </nav>

  {#if loading}
    <p class="pane-note" data-testid="detail-loading">Loading task detail…</p>
  {:else if detail === null}
    <p class="pane-note" data-testid="detail-empty">
      Select a topology node to inspect its task.
    </p>
  {:else}
    {#if activeTab === 'details'}
      <dl class="detail-grid" data-testid="detail-body-details">
        <dt>Task ID</dt>
        <dd>{detail.task.id}</dd>
        <dt>Status</dt>
        <dd>{detail.task.status}</dd>
        <dt>Kind</dt>
        <dd>{detail.task.kind}</dd>
        <dt>Task priority</dt>
        <dd>{detail.task.priority}</dd>
        <dt>Tools spawned</dt>
        <dd>{detail.task.tool_count}</dd>
      </dl>
      <div class="action-menu" data-testid="detail-action-menu">
        <button
          type="button"
          class="control"
          data-testid="detail-prioritize"
          disabled={!canControl}
          title={canControl
            ? undefined
            : 'Requires the control scope claim — task control is an elevated tier (D-079).'}
          onclick={() => onprioritize(detail.task.id, detail.task.priority + 1)}
        >
          Raise task priority
        </button>
      </div>
    {:else if activeTab === 'input'}
      <pre class="payload" data-testid="detail-body-input">{detail.task.query}</pre>
    {:else if activeTab === 'output'}
      <pre class="payload" data-testid="detail-body-output">{detail.result_inline ??
          (detail.result_ref ? `artifact ${detail.result_ref.id}` : 'no result yet')}</pre>
    {:else if activeTab === 'logs'}
      <p class="pane-note" data-testid="detail-body-logs">
        {detail.task.description}
      </p>
    {:else}
      <ul class="trace-list" data-testid="detail-body-trace">
        {#if traceEvents.length === 0}
          <li class="pane-note">No trace events for this run yet.</li>
        {:else}
          {#each traceEvents as ev, i (`${ev.sequence}-${i}`)}
            <li class="trace-row" data-testid="detail-trace-row">
              <span class="t-type">{ev.type}</span>
              <span class="t-time">{ev.occurred_at}</span>
            </li>
          {/each}
        {/if}
      </ul>
    {/if}
  {/if}
</section>

<style>
  .detail-pane {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-height: var(--space-12);
  }

  .detail-tabs {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .dtab {
    background: transparent;
    color: var(--color-text-muted);
    border: none;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .dtab.on {
    color: var(--color-accent);
    font-weight: 600;
  }

  .pane-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    padding: var(--space-3);
  }

  .detail-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .detail-grid dt {
    color: var(--color-text-muted);
  }

  .detail-grid dd {
    margin: var(--space-0);
    color: var(--color-text);
  }

  .action-menu {
    display: flex;
    gap: var(--space-2);
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

  .payload {
    margin: var(--space-0);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text);
    max-height: var(--size-graph-max-height);
    overflow: auto;
    white-space: pre-wrap;
  }

  .trace-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    max-height: var(--size-graph-max-height);
    overflow: auto;
  }

  .trace-row {
    display: grid;
    grid-template-columns: 2fr 1.5fr;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .t-type {
    color: var(--color-accent);
    font-weight: 600;
  }

  .t-time {
    color: var(--color-text-muted);
  }
</style>
