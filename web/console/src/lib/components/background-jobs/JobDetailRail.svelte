<script lang="ts">
  // Harbor Console — Background Jobs per-job right-rail (Phase 108j /
  // D-182; supersedes the pre-chrome `RightRail.svelte`).
  //
  // The selected-job detail: ONE packed `.panel.card` that fills the right
  // column and scrolls INTERNALLY (never page-level) — the Events-108h
  // `EventDetailRail` pattern, NOT a stack of RailCards that page-scrolls.
  // Composition (mockup order): header (short hash + status + copy + close)
  // / tab strip (Details | Progress | Events | Control History) / the
  // selected tab / Artifacts-for-this-Job / Parent task / Related Sessions.
  //
  // Every datum is real-wired (PAGE-POLISH §3):
  //   - Details / Parent task ← `tasks.get`
  //   - Progress ← `task.progress` + derived ETA + the run's `task.*`
  //     lifecycle timeline (JobProgressTab)
  //   - Events / Control History ← the RUN-scoped `events.subscribe`
  //     projection owned by `TaskRunStream` (reused, NOT forked)
  //   - Artifacts ← `artifacts.list` scoped to the job's session + run id
  //   - Related Sessions ← `tasks.list?group_id` siblings
  //
  // The run-scoped SSE is live-only (no backlog on a fresh connect), so the
  // Events / Control tabs render honest-empty states when the run was quiet
  // while the rail was open — never a fabricated row. Page-specific;
  // Svelte 5 runes (D-092); design tokens only.
  import StatusChip, { type StatusKind } from '$lib/components/ui/StatusChip.svelte';
  import JobProgressTab from './JobProgressTab.svelte';
  import { formatRelative } from '$lib/sessions/format.js';
  import type { TaskRunStream } from '$lib/tasks/run-stream.svelte.js';
  import type { TaskDetail, TaskRow } from '$lib/protocol/tasks.js';
  import type { ArtifactRow } from '$lib/protocol/artifacts.js';

  let {
    detail,
    loading,
    row,
    stream,
    siblings,
    siblingsLoading,
    artifacts,
    artifactsLoading,
    onclose
  }: {
    /** The `tasks.get` enriched detail; null while loading / on error. */
    detail: TaskDetail | null;
    /** Whether the `tasks.get` load is in flight. */
    loading: boolean;
    /** The selected queue row (its progress hint feeds Progress). */
    row: TaskRow | null;
    /** The run-scoped event stream — Events / Control / Progress timeline. */
    stream: TaskRunStream | null;
    /** Sibling tasks under the same TaskGroup (`tasks.list?group_id`). */
    siblings: TaskRow[];
    /** Whether the sibling load is in flight. */
    siblingsLoading: boolean;
    /** Artifacts produced by this job (`artifacts.list` scope.task). */
    artifacts: ArtifactRow[];
    /** Whether the artifacts load is in flight. */
    artifactsLoading: boolean;
    /** Closes the rail (clears the selection) — the header ✕. */
    onclose: () => void;
  } = $props();

  type Tab = 'details' | 'progress' | 'events' | 'control';
  let active = $state<Tab>('details');

  const STATUS_KINDS: Record<string, StatusKind> = {
    pending: 'neutral',
    running: 'accent',
    paused: 'warning',
    complete: 'success',
    failed: 'danger',
    cancelled: 'neutral'
  };

  // Run-scoped projections off the shared stream (newest-first).
  const runEvents = $derived(stream?.runEvents ?? []);
  const controlEvents = $derived(stream?.controlEvents ?? []);

  const TABS = $derived<{ key: Tab; label: string; count?: number }[]>([
    { key: 'details', label: 'Details' },
    { key: 'progress', label: 'Progress' },
    { key: 'events', label: 'Events', count: runEvents.length },
    { key: 'control', label: 'Control History', count: controlEvents.length }
  ]);

  function fmtSize(bytes: number): string {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }

  async function copyID(): Promise<void> {
    if (detail === null) return;
    try {
      await navigator.clipboard.writeText(detail.task.id);
    } catch {
      // clipboard denied (insecure context / no permission) — non-fatal
    }
  }
</script>

<section class="panel card job-rail" data-testid="bg-right-rail">
  {#if loading && detail === null}
    <div class="rail-skeleton" data-testid="bg-rail-skeleton" aria-hidden="true">
      {#each [0, 1, 2, 3] as i (i)}
        <span class="skeleton-line"></span>
      {/each}
    </div>
  {:else if detail === null}
    <p class="rail-empty" data-testid="bg-rail-empty">
      Select a background job to inspect its detail — events, control history,
      progress, artifacts and related sessions.
    </p>
  {:else}
    <header class="rail-header">
      <code class="rail-id" title={detail.task.id}>{detail.task.id}</code>
      <span class="kind">{detail.task.kind}</span>
      <StatusChip kind={STATUS_KINDS[detail.task.status] ?? 'neutral'} label={detail.task.status} />
      <button type="button" class="icon-btn" data-testid="bg-rail-copy-id" aria-label="Copy job id" onclick={() => void copyID()}>⧉</button>
      <button type="button" class="icon-btn close" data-testid="bg-rail-close" aria-label="Close detail" onclick={onclose}>✕</button>
    </header>

    <div class="tab-strip" role="tablist">
      {#each TABS as tab (tab.key)}
        <button
          type="button"
          class="tab"
          class:active={active === tab.key}
          role="tab"
          aria-selected={active === tab.key}
          data-testid={`bg-rail-tab-${tab.key}`}
          onclick={() => (active = tab.key)}
        >
          {tab.label}{#if tab.count !== undefined && tab.count > 0}<span class="badge">{tab.count}</span>{/if}
        </button>
      {/each}
    </div>

    <div class="tab-body" data-testid={`bg-rail-panel-${active}`}>
      {#if active === 'details'}
        <dl class="detail-list">
          <dt>Task ID</dt>
          <dd><code>{detail.task.id}</code></dd>
          <dt>Run ID</dt>
          <dd><code>{detail.task.id}</code></dd>
          <dt>Kind</dt>
          <dd>{detail.task.kind}</dd>
          <dt>Bg acknowledged</dt>
          <dd>{detail.task.background_acknowledged ? 'Yes' : 'No'}</dd>
          <dt>Priority</dt>
          <dd>{detail.task.priority}</dd>
          <dt>Tags</dt>
          <dd>{(detail.task.tags ?? []).join(', ') || '—'}</dd>
          <dt>Tenant / User</dt>
          <dd>{detail.task.identity.tenant} / {detail.task.identity.user}</dd>
          <dt>Started</dt>
          <dd>{formatRelative(detail.task.started_at)}</dd>
          <dt>Last activity</dt>
          <dd>{formatRelative(detail.task.last_activity_at)}</dd>
        </dl>
      {:else if active === 'progress'}
        <JobProgressTab {row} {stream} />
      {:else if active === 'events'}
        {#if runEvents.length === 0}
          <p class="muted" data-testid="bg-rail-events-empty">
            No events for this job on the live stream. The run-scoped SSE
            streams forward with no backlog — generate activity while the rail
            is open, or use a durable event driver for read-back.
          </p>
        {:else}
          <ul class="event-log" data-testid="bg-rail-events-list">
            {#each runEvents as ev (ev.sequence)}
              <li class="event-row">
                <span class="event-type">{ev.type}</span>
                <span class="event-at">{formatRelative(ev.occurred_at)}</span>
              </li>
            {/each}
          </ul>
        {/if}
      {:else if active === 'control'}
        {#if controlEvents.length === 0}
          <p class="muted" data-testid="bg-rail-control-empty">
            No control instructions recorded for this job on the live stream.
          </p>
        {:else}
          <ul class="event-log" data-testid="bg-rail-control-list">
            {#each controlEvents as ev (ev.sequence)}
              <li class="event-row">
                <span class="event-type">{ev.type}</span>
                <span class="event-at">{formatRelative(ev.occurred_at)}</span>
              </li>
            {/each}
          </ul>
        {/if}
      {/if}
    </div>

    <!-- Artifacts-so-far ────────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Artifacts for this Job{#if artifacts.length > 0}<span class="count">{artifacts.length}</span>{/if}</h4>
      {#if artifactsLoading}
        <p class="muted">Loading artifacts…</p>
      {:else if artifacts.length === 0}
        <p class="muted" data-testid="bg-rail-artifacts-empty">
          No artifacts produced by this job yet (<code>artifacts.list</code>
          scoped to its run id).
        </p>
      {:else}
        <ul class="artifact-list" data-testid="bg-rail-artifacts-list">
          {#each artifacts as a (a.ref.id)}
            <li class="artifact-row">
              <span class="artifact-name" title={a.ref.id}>{a.ref.filename || a.ref.id}</span>
              <StatusChip kind="neutral" label={a.ref.mime_type || '—'} />
              <span class="artifact-size">{fmtSize(a.ref.size_bytes)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </section>

    <!-- Parent task ─────────────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Parent task</h4>
      {#if detail.parent_task}
        <a class="rail-link" data-testid="bg-rail-parent-task" href={`/tasks/${detail.parent_task.task_id}`}>
          {detail.parent_task.task_id} ({detail.parent_task.status}) ↗
        </a>
      {:else}
        <p class="muted" data-testid="bg-rail-parent-task-empty">
          Top-level job — no parent task (spawned-by: {detail.task.parent_task_id || '—'}).
        </p>
      {/if}
    </section>

    <!-- Related Sessions ────────────────────────────────────────── -->
    <section class="rail-section">
      <h4 class="section-head">Related Sessions</h4>
      {#if siblingsLoading}
        <p class="muted">Loading sibling tasks…</p>
      {:else if siblings.length === 0}
        <p class="muted" data-testid="bg-rail-related-empty">
          This job is not a member of a TaskGroup — no related sessions.
        </p>
      {:else}
        <ul class="sibling-list" data-testid="bg-rail-related-list">
          {#each siblings as sib (sib.id)}
            <li class="sibling-row">
              <a class="rail-link" href={`/sessions/${sib.parent_session_id}`}>
                <code>{sib.id}</code>
              </a>
              <StatusChip kind={STATUS_KINDS[sib.status] ?? 'neutral'} label={sib.status} />
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  {/if}
</section>

<style>
  /* ONE packed card that fills the right column and scrolls INTERNALLY
     (never page-level) — the Events-108h EventDetailRail pattern. */
  .job-rail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    overflow-y: auto;
    /* The `.card` padding in +page.svelte is Svelte-scoped to that
       component, so it never reaches this child — give the rail its own
       inset (matching the other cards). `scrollbar-gutter: stable`
       reserves the classic-scrollbar gutter so it never overlays the
       right-aligned detail-list / artifact values (the same fix learned in
       the Tools-108k ToolDetailRail). */
    padding: var(--space-3);
    scrollbar-gutter: stable;
  }

  .rail-header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .rail-id {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: var(--size-search-min);
  }

  .kind {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .icon-btn {
    background: none;
    border: none;
    color: var(--color-text-muted);
    cursor: pointer;
    font-size: var(--text-sm);
    padding: var(--space-0);
  }

  .icon-btn.close {
    margin-left: auto;
  }

  .tab-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    flex-shrink: 0;
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: transparent;
    border: none;
    border-bottom: var(--border-hairline);
    border-bottom-color: transparent;
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-accent);
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
    text-align: right;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .rail-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border-top: var(--border-hairline);
    padding-top: var(--space-3);
  }

  .section-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .count {
    color: var(--color-accent);
    text-transform: none;
  }

  .event-log,
  .artifact-list,
  .sibling-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
    padding: var(--space-0);
    list-style: none;
  }

  .event-row,
  .sibling-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .artifact-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .event-type,
  .artifact-name {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .event-at,
  .artifact-size {
    margin-left: auto;
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    flex-shrink: 0;
  }

  .rail-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
  }

  .rail-link code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .detail-list code,
  .tab-body code,
  .muted code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
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
