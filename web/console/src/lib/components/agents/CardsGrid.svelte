<script lang="ts">
  // Harbor Console — Agents-page cards grid (Phase 73e / D-124).
  //
  // The responsive grid of `AgentCard`s — the list-mode primary view
  // (CONVENTIONS.md §5: a page's primary DataTable OR canvas; the Agents
  // page's mockup uses a cards canvas). A page-specific component.
  import AgentCard from './AgentCard.svelte';
  import type { Agent } from '$lib/protocol/agents.js';

  let { agents, onopen }: { agents: Agent[]; onopen: (id: string) => void } =
    $props();
</script>

<div class="grid" data-testid="agents-cards-grid">
  {#each agents as agent (agent.id)}
    <AgentCard {agent} {onopen} />
  {/each}
</div>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(var(--size-card-min), 1fr));
    gap: var(--space-3);
  }
</style>
