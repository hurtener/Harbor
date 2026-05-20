<script lang="ts">
  // Harbor Console — Tasks page right-rail Parent Session card body
  // (Phase 73d / D-123). Renders the `tasks.get` parent-session + parent
  // -task references with deep links into Sessions / Tasks (unprefixed
  // routes per CONVENTIONS.md §1). Page-specific; design tokens only.
  import type { TaskDetail } from '$lib/protocol/tasks.js';

  let { detail }: { detail: TaskDetail | null } = $props();
</script>

{#if detail !== null}
  <div class="parent-card" data-testid="rail-parent-session">
    <dl class="kv">
      <div><dt>Session</dt><dd>{detail.parent_session.session_id}</dd></div>
      {#if detail.parent_session.agent_name}
        <div><dt>Agent</dt><dd>{detail.parent_session.agent_name}</dd></div>
      {/if}
      {#if detail.parent_session.status}
        <div><dt>Status</dt><dd>{detail.parent_session.status}</dd></div>
      {/if}
    </dl>
    <a
      class="rail-link"
      data-testid="rail-open-session"
      href={`/sessions/${detail.parent_session.session_id}`}
    >
      Open parent session in Live Runtime
    </a>
    {#if detail.parent_task}
      <a
        class="rail-link"
        data-testid="rail-open-parent-task"
        href={`/tasks/${detail.parent_task.task_id}`}
      >
        Open parent task ({detail.parent_task.status})
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
    justify-content: space-between;
    gap: var(--space-2);
  }

  .kv dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .kv dd {
    margin: var(--space-0);
    font-size: var(--text-xs);
  }

  .rail-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
    text-decoration: none;
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
