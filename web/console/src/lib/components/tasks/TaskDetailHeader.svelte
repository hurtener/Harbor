<script lang="ts">
  // Harbor Console — Tasks page detail-mode header (Phase 108i / D-181).
  //
  // The compact header for the selected-task detail mode: a `← Board`
  // return affordance, the task id + copy, status + kind chips, an
  // "Open parent session" deep link, an `actions` slot for the control
  // action bar, and a stat strip (Started / Duration / Tools / Session).
  // Tasks-specific. Svelte 5 runes (D-092); tokens only.
  import type { Snippet } from 'svelte';
  import { StatusChip, type StatusKind } from '$lib/components/ui/index.js';
  import { formatRelative } from '$lib/sessions/format.js';
  import type { TaskRow } from '$lib/protocol/tasks.js';

  let {
    task,
    onback,
    actions
  }: {
    /** The selected task row. */
    task: TaskRow;
    /** Returns to board/list mode. */
    onback: () => void;
    /** The control action bar (Cancel / Pause / … ) — page-supplied. */
    actions?: Snippet;
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
    if (ms <= 0) return '—';
    if (ms < 1000) return `${ms}ms`;
    const s = Math.round(ms / 1000);
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    const rem = s % 60;
    if (m < 60) return rem > 0 ? `${m}m ${rem}s` : `${m}m`;
    return `${Math.floor(m / 60)}h ${m % 60}m`;
  }

  async function copyID(): Promise<void> {
    try {
      await navigator.clipboard.writeText(task.id);
    } catch {
      // clipboard denied (no permission / insecure context) — non-fatal
    }
  }
</script>

<header class="detail-header" data-testid="task-detail-header">
  <div class="head-line">
    <button type="button" class="back" data-testid="task-detail-back" onclick={onback}>← Board</button>
    <span class="id mono" title={task.id}>{task.id}</span>
    <button type="button" class="copy" data-testid="task-copy-id" onclick={() => void copyID()} aria-label="Copy task id">⧉</button>
    <StatusChip kind={statusKinds[task.status] ?? 'neutral'} label={task.status} />
    <span class="kind">{task.kind}</span>
    <a class="open-session" data-testid="task-open-session" href={`/sessions/${task.parent_session_id}`}>
      Open in Live Runtime ↗
    </a>
    {#if actions}
      <div class="actions">{@render actions()}</div>
    {/if}
  </div>
  <dl class="stat-strip">
    <div><dt>Started</dt><dd>{formatRelative(task.started_at)}</dd></div>
    <div><dt>Duration</dt><dd title="Active processing time">{durationLabel(task.duration_ms)}</dd></div>
    <div><dt>Type</dt><dd>{task.kind}</dd></div>
    <div><dt>Tools called</dt><dd>{task.tool_count}</dd></div>
    <div><dt>Session</dt><dd class="mono ellip">{task.parent_session_id}</dd></div>
    {#if task.priority !== 0}
      <div><dt>Priority</dt><dd>{task.priority}</dd></div>
    {/if}
  </dl>
</header>

<style>
  .detail-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    flex-shrink: 0;
  }

  .head-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .back {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .id {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .copy {
    background: none;
    border: none;
    color: var(--color-text-muted);
    cursor: pointer;
    font-size: var(--text-sm);
    padding: var(--space-0);
  }

  .kind {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .open-session {
    font-size: var(--text-xs);
    color: var(--color-accent);
    text-decoration: none;
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    margin-left: auto;
  }

  .stat-strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-4);
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .stat-strip div {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }

  .stat-strip dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .stat-strip dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .ellip {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: var(--size-search-min);
  }
</style>
