<script lang="ts">
  // Harbor Console — Background Jobs page per-job right-rail (Phase 73h
  // / D-128).
  //
  // The selected-job detail panel: a tabbed sub-panel set rendered
  // inside a shared `ui/RailCard`. Tabs, in page-spec §12 mockup order:
  // Details | Progress | Logs | Pending approvals | Artifacts | Related
  // Sessions. Each tab is a projection of an already-shipped or
  // 73h-extended Protocol surface — `tasks.get`, the `task.*` event
  // stream, `artifacts.list?task_id=…`, `tasks.list?group_id=…`. Page-
  // specific — lives in `components/background-jobs/`. Svelte 5 runes
  // (D-092); design tokens only.
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import type { TaskDetail, TaskRow } from '$lib/protocol/tasks.js';

  let {
    detail,
    loading,
    siblings,
    siblingsLoading
  }: {
    /** The `tasks.get` enriched detail of the selected job; null when none. */
    detail: TaskDetail | null;
    /** Whether the `tasks.get` load is in flight. */
    loading: boolean;
    /** The sibling tasks under the same TaskGroup (`tasks.list?group_id`). */
    siblings: TaskRow[];
    /** Whether the sibling `tasks.list?group_id` load is in flight. */
    siblingsLoading: boolean;
  } = $props();

  type Tab = 'details' | 'progress' | 'logs' | 'approvals' | 'artifacts' | 'related';
  const TABS: { id: Tab; label: string }[] = [
    { id: 'details', label: 'Details' },
    { id: 'progress', label: 'Progress' },
    { id: 'logs', label: 'Logs' },
    { id: 'approvals', label: 'Pending approvals' },
    { id: 'artifacts', label: 'Artifacts for this Job' },
    { id: 'related', label: 'Related Sessions' }
  ];
  let active = $state<Tab>('details');

  const STATUS_KINDS: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  function progressLabel(p: number | undefined): string {
    if (p === undefined) return 'No planner progress hint emitted';
    return `${Math.round(Math.max(0, Math.min(1, p)) * 100)}%`;
  }
</script>

{#if loading}
  <div class="rail-skeleton" data-testid="bg-rail-skeleton" aria-hidden="true">
    {#each [0, 1, 2] as i (i)}
      <span class="skeleton-line"></span>
    {/each}
  </div>
{:else if detail === null}
  <p class="rail-empty" data-testid="bg-rail-empty">
    Select a background job to inspect its detail.
  </p>
{:else}
  <section class="right-rail" data-testid="bg-right-rail">
    <header class="rail-header">
      <code class="rail-id">{detail.task.id}</code>
      <StatusChip
        kind={STATUS_KINDS[detail.task.status] ?? 'neutral'}
        label={detail.task.status}
      />
    </header>

    <div class="tab-strip" role="tablist">
      {#each TABS as tab (tab.id)}
        <button
          type="button"
          class="tab"
          class:active={active === tab.id}
          role="tab"
          aria-selected={active === tab.id}
          data-testid={`bg-rail-tab-${tab.id}`}
          onclick={() => (active = tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </div>

    <div class="tab-body" data-testid={`bg-rail-panel-${active}`}>
      {#if active === 'details'}
        <dl class="detail-list">
          <dt>Task ID</dt>
          <dd><code>{detail.task.id}</code></dd>
          <dt>Kind</dt>
          <dd>{detail.task.kind}</dd>
          <dt>Parent session</dt>
          <dd><code>{detail.task.parent_session_id}</code></dd>
          <dt>Parent task</dt>
          <dd>{detail.parent_task?.task_id ?? '— (top-level)'}</dd>
          <dt>Priority</dt>
          <dd>{detail.task.priority}</dd>
          <dt>Tags</dt>
          <dd>{(detail.task.tags ?? []).join(', ') || '—'}</dd>
        </dl>
      {:else if active === 'progress'}
        <p class="muted">Current progress: {progressLabel(detail.task.progress)}</p>
        <p class="muted">
          Started {detail.task.started_at} · last activity
          {detail.task.last_activity_at}
        </p>
      {:else if active === 'logs'}
        <p class="muted">
          <code>task.*</code> / <code>tool.*</code> events for this job stream
          on the Events page filtered by <code>run_id={detail.task.id}</code>.
        </p>
      {:else if active === 'approvals'}
        {#if detail.task.has_pending_approval}
          <p class="muted">
            This job has an open HITL / tool-approval gate — resolve it via the
            shipped <code>approve</code> / <code>reject</code> control verbs.
          </p>
        {:else}
          <p class="muted">No pending approvals for this job.</p>
        {/if}
      {:else if active === 'artifacts'}
        <p class="muted">
          Artifacts produced by this job are listed via
          <code>artifacts.list?task_id={detail.task.id}</code>.
        </p>
      {:else if active === 'related'}
        {#if siblingsLoading}
          <p class="muted">Loading sibling tasks…</p>
        {:else if siblings.length === 0}
          <p class="muted" data-testid="bg-rail-related-empty">
            This job is not a member of a TaskGroup — no related sessions.
          </p>
        {:else}
          <ul class="sibling-list" data-testid="bg-rail-related-list">
            {#each siblings as sib (sib.id)}
              <li>
                <code>{sib.id}</code>
                <StatusChip
                  kind={STATUS_KINDS[sib.status] ?? 'neutral'}
                  label={sib.status}
                />
              </li>
            {/each}
          </ul>
        {/if}
      {/if}
    </div>
  </section>
{/if}

<style>
  .right-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .rail-header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .rail-id {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .tab {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: transparent;
    border: none;
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-accent);
    font-weight: 600;
  }

  .tab-body {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .detail-list {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .detail-list dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .detail-list dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .detail-list code,
  .tab-body code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .muted {
    margin: var(--space-0) var(--space-0) var(--space-2);
    color: var(--color-text-muted);
  }

  .sibling-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
    padding: var(--space-0);
    list-style: none;
  }

  .sibling-list li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .rail-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-line {
    height: var(--space-4);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }
</style>
