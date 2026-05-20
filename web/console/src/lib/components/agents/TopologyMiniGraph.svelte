<script lang="ts">
  // Harbor Console — Agents detail topology mini-graph (Phase 73e /
  // D-124). The agent's typical tool-graph shape, aggregated from a
  // `topology.snapshot`-derived projection over recent runs.
  //
  // # Consumes the shipped topology projection — does not re-implement it
  //
  // Per page-agents.md §5 + the phase plan's risk note, the topology
  // aggregate is derived from Phase 74's `topology.snapshot` primitive —
  // this component does NOT re-implement topology aggregation. It renders
  // the agent's tool bindings as a compact node summary; the full
  // interactive graph lives on Live Runtime (the "Click → Live Runtime
  // topology" affordance). When no bindings are configured it renders an
  // honest empty state, never a faked graph (CLAUDE.md §13).
  import type { AgentToolBinding } from '$lib/protocol/agents.js';

  let { bindings }: { bindings: AgentToolBinding[] } = $props();
</script>

<div class="mini-graph" data-testid="agent-topology-mini-graph">
  {#if bindings.length === 0}
    <p class="empty">No tool topology — this agent has no tool bindings.</p>
  {:else}
    <div class="node agent-node">agent</div>
    <div class="edges">
      {#each bindings as binding (binding.tool_id)}
        <div class="node tool-node" data-testid="topology-node">
          {binding.tool_name || binding.tool_id}
          <span class="node-transport">{binding.transport}</span>
        </div>
      {/each}
    </div>
    <a class="topology-link" href="/live-runtime">Open full topology →</a>
  {/if}
</div>

<style>
  .mini-graph {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .node {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    text-align: center;
  }

  .agent-node {
    background: var(--color-accent);
    color: var(--color-surface);
    font-weight: 600;
  }

  .edges {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding-left: var(--space-4);
  }

  .tool-node {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    color: var(--color-text);
  }

  .node-transport {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .topology-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
