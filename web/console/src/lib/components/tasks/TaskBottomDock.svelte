<script lang="ts">
  // Harbor Console — Tasks page per-task bottom dock (Phase 108i / D-181).
  //
  // The detail-mode tab strip — Details | Input | Output | Logs | Events |
  // Errors | Control History | Interventions | Group. Each tab renders REAL
  // data; there is no placeholder prose (the shortfall this supersedes from
  // the deleted `TaskDetailTabs`):
  //   - Details / Input / Output ← `tasks.get` (the `detail` prop)
  //   - Logs                     ← honest empty (needs the Phase 73
  //                                 `state.history` surface, still Pending)
  //   - Events / Errors / Control / Interventions / Group ← the RUN-scoped
  //     `events.subscribe` projection (the `stream` controller). The
  //     Interventions tab backfills `pause.list` and offers a real Resume /
  //     Reject (`approve` / `reject`, control-scope gated — D-066).
  //
  // The stream is owned by the page's `TaskRunStream` controller (so the
  // rail Cost / Summary read the SAME subscription); this component is the
  // pure view. Tasks-specific. Svelte 5 runes (D-092); tokens only.
  import { StatusChip } from '$lib/components/ui/index.js';
  import { categoryOf, categoryKind } from '$lib/events/taxonomy.js';
  import { formatRelative } from '$lib/sessions/format.js';
  import type { TaskRunStream } from '$lib/tasks/run-stream.svelte.js';
  import type { PauseSnapshot } from '$lib/protocol/pause.js';
  import type { TaskDetail } from '$lib/protocol/tasks.js';

  let {
    stream,
    detail,
    detailLoading = false,
    canControl = false
  }: {
    /** The run-scoped event stream controller (owned by the page). */
    stream: TaskRunStream;
    /** The `tasks.get` enriched detail; null while loading / on error. */
    detail: TaskDetail | null;
    /** Whether the `tasks.get` load is in flight. */
    detailLoading?: boolean;
    /** Whether the operator holds the control scope (D-066) — gates Resume. */
    canControl?: boolean;
  } = $props();

  // Derived run-scoped views (read straight off the controller getters).
  const runEvents = $derived(stream.runEvents);
  const errorEvents = $derived(
    runEvents.filter((e) => e.type.includes('failed') || e.type.includes('error'))
  );
  const controlEvents = $derived(stream.controlEvents);
  const interventionEvents = $derived(stream.interventionEvents);
  const groupEvents = $derived(stream.groupEvents);
  const pending = $derived(stream.pending);
  const streamState = $derived(stream.streamState);
  const hasGroup = $derived(groupEvents.length > 0 || (detail?.task.group_id ?? '') !== '');
  // Execution-context model is not on `tasks.get`; derive it from the run's
  // cost events when present (honest "—" otherwise).
  const model = $derived(stream.cost.models[0] ?? '');

  type Tab =
    | 'details'
    | 'input'
    | 'output'
    | 'logs'
    | 'events'
    | 'errors'
    | 'control'
    | 'interventions'
    | 'group';
  let active = $state<Tab>('details');

  const TABS = $derived<{ key: Tab; label: string; count?: number }[]>([
    { key: 'details', label: 'Details' },
    { key: 'input', label: 'Input' },
    { key: 'output', label: 'Output' },
    { key: 'logs', label: 'Logs' },
    { key: 'events', label: 'Events', count: runEvents.length },
    { key: 'errors', label: 'Errors', count: errorEvents.length },
    { key: 'control', label: 'Control History', count: controlEvents.length },
    {
      key: 'interventions',
      label: 'Interventions',
      count: interventionEvents.length + pending.length
    },
    ...(hasGroup ? [{ key: 'group' as Tab, label: 'Group', count: groupEvents.length }] : [])
  ]);

  function resolve(snap: PauseSnapshot, verb: 'approve' | 'reject'): void {
    void stream.resolve(snap, verb);
  }
</script>

<section class="dock" data-testid="task-bottom-dock">
  <div class="tab-strip" role="tablist" aria-label="Task detail tabs">
    {#each TABS as tab (tab.key)}
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={active === tab.key}
        aria-selected={active === tab.key}
        data-testid={`task-dock-tab-${tab.key}`}
        onclick={() => (active = tab.key)}
      >
        {tab.label}{#if tab.count !== undefined && tab.count > 0}<span class="badge">{tab.count}</span>{/if}
      </button>
    {/each}
    <span class="stream-state" data-testid="task-dock-stream" data-state={streamState}>
      {streamState === 'open' ? '● live' : streamState === 'connecting' ? '○ connecting' : streamState}
    </span>
  </div>

  <div class="tab-panel" role="tabpanel" data-testid="task-dock-panel">
    {#if detailLoading && detail === null}
      <p class="empty">Loading task detail…</p>
    {:else if active === 'details'}
      {#if detail !== null}
        <div class="meta-grid">
          <section>
            <h4 class="meta-head">Task metadata</h4>
            <dl class="kv">
              <div><dt>Task ID</dt><dd class="mono">{detail.task.id}</dd></div>
              <div><dt>Run ID</dt><dd class="mono">{detail.task.id}</dd></div>
              <div><dt>Parent task</dt><dd class="mono">{detail.task.parent_task_id || '—'}</dd></div>
              <div><dt>Spawned by planner</dt><dd>{detail.task.parent_task_id ? 'Yes' : 'No'}</dd></div>
              <div><dt>Background</dt><dd>{detail.task.is_background ? 'Yes' : 'No'}</dd></div>
              <div><dt>Bg acknowledged</dt><dd>{detail.task.background_acknowledged ? 'Yes' : 'No'}</dd></div>
              <div><dt>Priority</dt><dd>{detail.task.priority}</dd></div>
              <div><dt>Group</dt><dd class="mono">{detail.task.group_id || '—'}</dd></div>
            </dl>
          </section>
          <section>
            <h4 class="meta-head">Execution context</h4>
            <dl class="kv">
              <div><dt>Model</dt><dd>{model || '—'}</dd></div>
              <div><dt>Planner snapshot</dt><dd class="mono">{detail.planner_snapshot?.checkpoint_id || '—'}</dd></div>
              <div><dt>Session</dt><dd class="mono">{detail.task.identity.session}</dd></div>
              <div><dt>User</dt><dd>{detail.task.identity.user}</dd></div>
              <div><dt>Tenant</dt><dd>{detail.task.identity.tenant}</dd></div>
              <div><dt>Started</dt><dd>{formatRelative(detail.task.started_at)}</dd></div>
              <div><dt>Last activity</dt><dd>{formatRelative(detail.task.last_activity_at)}</dd></div>
            </dl>
          </section>
        </div>
      {:else}
        <p class="empty">No task detail loaded.</p>
      {/if}
    {:else if active === 'input'}
      <pre class="code" data-testid="task-dock-input">{detail?.task.query || '(no query recorded)'}</pre>
    {:else if active === 'output'}
      {#if detail?.result_ref}
        <p class="empty" data-testid="task-dock-output">
          Result is a {detail.result_ref.size_bytes}-byte artifact
          (<code>{detail.result_ref.id}</code>) — fetched by reference (D-026).
        </p>
      {:else if detail?.result_inline}
        <pre class="code" data-testid="task-dock-output">{detail.result_inline}</pre>
      {:else}
        <p class="empty" data-testid="task-dock-output">No output recorded yet.</p>
      {/if}
    {:else if active === 'logs'}
      <p class="empty" data-testid="task-dock-logs">
        The step-by-step log / trajectory surface lands with the Phase 73
        <code>state.history</code> Protocol method (still Pending). The live
        planner / tool / task lifecycle for this run is on the
        <strong>Events</strong> tab.
      </p>
    {:else if active === 'events'}
      {#if runEvents.length === 0}
        <p class="empty" data-testid="task-dock-empty-events">
          No events for this task in the live stream. The SSE streams events
          going forward; a run that finished before this dock opened may show
          no rows — generate activity while open, or use a durable event driver.
        </p>
      {:else}
        <ul class="event-log" data-testid="task-events-list">
          {#each runEvents as ev (ev.sequence)}
            <li class="event-row">
              <StatusChip kind={categoryKind(categoryOf(ev.type))} label={categoryOf(ev.type)} />
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'errors'}
      {#if detail?.task.error_class}
        <p class="err" data-testid="task-dock-error-class">Error class: {detail.task.error_class}</p>
      {/if}
      {#if errorEvents.length === 0}
        <p class="empty" data-testid="task-dock-empty-errors">No error events for this task.</p>
      {:else}
        <ul class="event-log" data-testid="task-errors-list">
          {#each errorEvents as ev (ev.sequence)}
            <li class="event-row">
              <StatusChip kind="danger" label={categoryOf(ev.type)} />
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'control'}
      {#if controlEvents.length === 0}
        <p class="empty" data-testid="task-dock-empty-control">
          No control instructions recorded for this task in the live stream.
        </p>
      {:else}
        <ul class="event-log" data-testid="task-control-list">
          {#each controlEvents as ev (ev.sequence)}
            <li class="event-row">
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'interventions'}
      {#if pending.length > 0}
        <ul class="pending-list" data-testid="task-interventions-pending">
          {#each pending as snap (snap.token)}
            {@const result = stream.actionResult.get(snap.token)}
            <li class="pending-row">
              <div class="pending-meta">
                <span class="pending-reason">{snap.reason}</span>
                <span class="event-age">{formatRelative(snap.paused_at)}</span>
              </div>
              <div class="pending-actions">
                {#if result}
                  <span class="action-result" data-testid="task-intervention-result">{result}</span>
                {:else}
                  <button
                    type="button"
                    class="mini"
                    data-testid="task-intervention-approve"
                    disabled={!canControl || stream.actionBusy === snap.token || (snap.identity.run ?? '') === ''}
                    title={canControl ? 'Resume this run (approve)' : 'Requires the control-plane scope claim (D-066)'}
                    onclick={() => resolve(snap, 'approve')}
                  >
                    Resume
                  </button>
                  <button
                    type="button"
                    class="mini"
                    data-testid="task-intervention-reject"
                    disabled={!canControl || stream.actionBusy === snap.token || (snap.identity.run ?? '') === ''}
                    title={canControl ? 'Reject this run' : 'Requires the control-plane scope claim (D-066)'}
                    onclick={() => resolve(snap, 'reject')}
                  >
                    Reject
                  </button>
                {/if}
              </div>
            </li>
          {/each}
        </ul>
      {/if}
      {#if interventionEvents.length === 0 && pending.length === 0}
        <p class="empty" data-testid="task-dock-empty-interventions">
          No interventions (pauses / approvals) recorded for this task.
        </p>
      {:else if interventionEvents.length > 0}
        <ul class="event-log" data-testid="task-interventions-list">
          {#each interventionEvents as ev (ev.sequence)}
            <li class="event-row">
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'group'}
      {#if groupEvents.length === 0}
        <p class="empty" data-testid="task-dock-empty-group">
          No TaskGroup lifecycle events for this task in the live stream.
        </p>
      {:else}
        <ul class="event-log" data-testid="task-group-list">
          {#each groupEvents as ev (ev.sequence)}
            <li class="event-row">
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {/if}
  </div>
</section>

<style>
  .dock {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    border-top: var(--border-hairline);
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    padding: var(--space-2) var(--space-0) var(--space-0);
    flex-shrink: 0;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    background: none;
    color: var(--color-text-muted);
    border: none;
    border-bottom: var(--border-hairline);
    border-bottom-color: transparent;
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-text);
    border-bottom-color: var(--color-accent);
    font-weight: 600;
  }

  .badge {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-pill);
    padding: 0 var(--space-1);
  }

  .stream-state {
    margin-left: auto;
    padding-right: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .tab-panel {
    flex: 1;
    min-height: 0;
    padding: var(--space-3) var(--space-0) var(--space-0);
    overflow-y: auto;
  }

  .meta-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
  }

  .meta-head {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .kv {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
  }

  .kv div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .kv dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .kv dd {
    margin: var(--space-0);
    font-size: var(--text-xs);
    text-align: right;
    color: var(--color-text);
  }

  .code {
    margin: var(--space-0);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    white-space: pre-wrap;
    word-break: break-word;
    color: var(--color-text);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .err {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-danger);
  }

  .event-log,
  .pending-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .event-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-0);
  }

  .event-type {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .event-age {
    margin-left: auto;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .pending-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .pending-meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .pending-reason {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .pending-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .action-result {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .mini {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .mini:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .mono {
    font-family: var(--font-mono);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
