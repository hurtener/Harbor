<script lang="ts">
  // Harbor Console — Agents-page agent card (Phase 73e / D-124).
  //
  // One card in the cards grid: name + planner / model / tool-count
  // badges + a health pill. Clicking the card navigates to the agent's
  // detail route. A page-specific component (CONVENTIONS.md §3); composes
  // the shared `StatusChip` and design tokens only.
  import { StatusChip, type StatusKind } from '$lib/components/ui';
  import type { Agent, AgentHealth } from '$lib/protocol/agents.js';

  let { agent, onopen }: { agent: Agent; onopen: (id: string) => void } = $props();

  /** Maps an agent health badge onto a StatusChip kind. */
  function healthKind(h: AgentHealth): StatusKind {
    switch (h) {
      case 'Healthy':
        return 'success';
      case 'Degraded':
        return 'warning';
      case 'Force-Stopped':
        return 'danger';
      case 'Paused':
      case 'Drained':
        return 'accent';
      default:
        return 'neutral';
    }
  }

  /** Truncates the agent_id for the card's monospace handle. */
  function shortID(id: string): string {
    return id.length > 12 ? `${id.slice(0, 12)}…` : id;
  }
</script>

<button
  type="button"
  class="card"
  data-testid="agent-card"
  data-agent-id={agent.id}
  onclick={() => onopen(agent.id)}
>
  <div class="card-head">
    <span class="name">{agent.name || agent.id}</span>
    <StatusChip kind={healthKind(agent.health)} label={agent.health} />
  </div>
  <span class="handle mono">{shortID(agent.id)}</span>
  {#if agent.description}
    <p class="description">{agent.description}</p>
  {/if}
  <div class="badges">
    <span class="badge" data-testid="agent-card-planner">
      {agent.planner_type || 'planner: n/a'}
    </span>
    <span class="badge">{agent.model || 'model: n/a'}</span>
    <span class="badge">{agent.tools_count} tools</span>
    <span class="badge">{agent.mcp_count} MCP</span>
  </div>
</button>

<style>
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3) var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    text-align: left;
    cursor: pointer;
    width: 100%;
  }

  .card:hover {
    background: var(--color-surface-raised);
  }

  .card-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .name {
    font-size: var(--text-md);
    font-weight: 600;
    color: var(--color-text);
  }

  .handle {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .description {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .badges {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }

  .badge {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }

  .mono {
    font-family: var(--font-mono);
  }
</style>
