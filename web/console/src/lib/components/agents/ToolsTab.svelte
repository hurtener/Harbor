<script lang="ts">
  // Harbor Console — Agents detail Tools tab (Phase 73e / D-124). The
  // agent's configured tool bindings + per-binding OAuth status, from
  // `agents.tools`. Page-specific component; composes OAuthBindingRow.
  import OAuthBindingRow from './OAuthBindingRow.svelte';
  import type { AgentToolBinding } from '$lib/protocol/agents.js';

  let { bindings }: { bindings: AgentToolBinding[] } = $props();
</script>

<div class="tools-tab" data-testid="agent-tools-tab">
  {#if bindings.length === 0}
    <p class="empty">No tool bindings configured for this agent.</p>
  {:else}
    {#each bindings as binding (binding.tool_id)}
      <OAuthBindingRow {binding} />
    {/each}
  {/if}
</div>

<style>
  .tools-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
