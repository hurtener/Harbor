<script lang="ts">
  // Harbor Console — Tasks page right-rail Parent Session card (Phase 108i
  // / D-181). Renders the `tasks.get` parent-session + parent-task
  // references with deep links into Sessions / Tasks (unprefixed routes —
  // CONVENTIONS.md §1). The registry returns a SPARSE parent_session
  // (empty agent / status / started on this runtime — live-wire finding),
  // so the empty fields render `—`, never a fabricated value (§13).
  // Tasks-specific; tokens only.
  import { formatRelative } from '$lib/sessions/format.js';
  import type { TaskDetail } from '$lib/protocol/tasks.js';

  let { detail }: { detail: TaskDetail | null } = $props();

  /** Renders a wire string, or "—" when the registry left it empty. */
  function orDash(v: string | undefined): string {
    return v !== undefined && v !== '' ? v : '—';
  }

  /** Renders an RFC-3339 instant, or "—" when zero / empty. */
  function timeOrDash(v: string | undefined): string {
    if (v === undefined || v === '' || v.startsWith('0001-01-01')) return '—';
    return formatRelative(v);
  }
</script>

{#if detail !== null}
  <div class="parent-card" data-testid="rail-parent-session">
    <dl class="kv">
      <div><dt>Session ID</dt><dd class="mono ellip" title={detail.parent_session.session_id}>{detail.parent_session.session_id}</dd></div>
      <div><dt>Agent</dt><dd>{orDash(detail.parent_session.agent_name)}</dd></div>
      <div><dt>Status</dt><dd>{orDash(detail.parent_session.status)}</dd></div>
      <div><dt>Started</dt><dd>{timeOrDash(detail.parent_session.started_at)}</dd></div>
    </dl>
    <a class="rail-link" data-testid="rail-open-session" href={`/sessions/${detail.parent_session.session_id}`}>
      Open in Live Runtime ↗
    </a>
    {#if detail.parent_task}
      <a class="rail-link" data-testid="rail-open-parent-task" href={`/tasks/${detail.parent_task.task_id}`}>
        Open parent task ({detail.parent_task.status}) ↗
      </a>
    {/if}
  </div>
{:else}
  <p class="muted">Select a task to see its parent session.</p>
{/if}

<style>
  .parent-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .kv {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
  }

  .kv div {
    display: flex;
    align-items: center;
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
    min-width: 0;
  }

  .rail-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
  }

  .mono {
    font-family: var(--font-mono);
  }

  .ellip {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
