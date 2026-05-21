<script lang="ts">
  // Chat module — tool-call trace card (Phase 73n / D-130).
  //
  // Renders the `ChatToolCall[]` attached to an agent message as a
  // compact trace card. The summaries are redacted by the runtime
  // (CLAUDE.md §7 — never raw tool arguments); the card displays them
  // verbatim.
  //
  // This is the in-bubble structured view. The matching renderer in the
  // canonical registry (`renderers/ToolCallTrace.svelte`, registered
  // under `application/vnd.harbor.tool-call-trace`) is the preview-pane
  // dispatch target — both share the trace shape.
  import type { ChatToolCall } from './types.js';

  let { toolCalls }: { toolCalls: ChatToolCall[] } = $props();
</script>

<div class="tool-call-card" data-testid="tool-call-trace-card">
  <span class="card-label">Tool calls</span>
  <ul class="trace-list">
    {#each toolCalls as call, i (i)}
      <li class="trace-entry">
        <span class="tool-name">{call.tool}</span>
        <span class="trace-status" data-status={call.status}>{call.status}</span>
        <span class="trace-summary">{call.summary}</span>
      </li>
    {/each}
  </ul>
</div>

<style>
  .tool-call-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .card-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .trace-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .trace-entry {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2);
    background: var(--color-surface);
    border: var(--border-thin) solid var(--color-border);
    border-radius: var(--radius-sm);
  }

  .tool-name {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
    font-weight: 600;
  }

  .trace-status {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .trace-status[data-status='failed'] {
    color: var(--color-danger);
  }

  .trace-status[data-status='succeeded'] {
    color: var(--color-success);
  }

  .trace-summary {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
