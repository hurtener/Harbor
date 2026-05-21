<script lang="ts">
  // Harbor Console — Agents detail header (Phase 73e / D-124). The
  // detail-mode row-1: name + status badge + version_hash + the five
  // fleet-control buttons + "Open in Playground" + copy-id. Page-specific
  // component; composes the shared StatusChip + the page's ControlButtons.
  import { StatusChip, type StatusKind } from '$lib/components/ui';
  import ControlButtons from './ControlButtons.svelte';
  import type { Agent, AgentHealth } from '$lib/protocol/agents.js';

  let {
    agent
  }: {
    agent: Agent;
  } = $props();

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

  function copyID(): void {
    void globalThis.navigator?.clipboard?.writeText(agent.id);
  }
</script>

<header class="detail-header" data-testid="agent-detail-header">
  <div class="title-row">
    <div class="title-block">
      <h1 data-testid="agent-detail-name">{agent.name || agent.id}</h1>
      <StatusChip kind={healthKind(agent.health)} label={agent.health} />
      <StatusChip kind="neutral" label={agent.status} />
    </div>
    <div class="meta-row">
      <span class="version mono" data-testid="agent-detail-version">
        {agent.version_hash || 'no version'}
      </span>
      <button
        type="button"
        class="meta-btn"
        data-testid="agent-copy-id"
        onclick={copyID}
        title="Copy agent ID"
      >
        Copy ID
      </button>
      <a
        class="meta-btn"
        data-testid="agent-open-playground"
        href={`/live-runtime?agent=${encodeURIComponent(agent.id)}`}
      >
        Open in Playground
      </a>
    </div>
  </div>
  <ControlButtons />
</header>

<style>
  .detail-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .title-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  .title-block {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  h1 {
    margin: var(--space-0);
    font-size: var(--text-xl);
    color: var(--color-text);
  }

  .meta-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .version {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .meta-btn {
    font-size: var(--text-xs);
    color: var(--color-text);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
  }

  .mono {
    font-family: var(--font-mono);
    overflow-wrap: anywhere;
  }
</style>
