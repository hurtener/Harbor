<script lang="ts">
  // Harbor Console — Agents detail Memory tab (Phase 73e / D-124). The
  // configured memory strategy (Phase 24), TTL, and scope, from
  // `agents.memory`. Page-specific component.
  import type { AgentMemoryBinding } from '$lib/protocol/agents.js';

  let { binding }: { binding: AgentMemoryBinding } = $props();

  /** Formats a TTL in seconds as a human label. */
  function ttlLabel(seconds: number): string {
    if (seconds <= 0) return 'no TTL';
    if (seconds % 3600 === 0) return `${seconds / 3600}h`;
    if (seconds % 60 === 0) return `${seconds / 60}m`;
    return `${seconds}s`;
  }

  const hasStrategy = $derived(binding.strategy_id !== '');
</script>

<div class="memory-tab" data-testid="agent-memory-tab">
  {#if hasStrategy}
    <dl class="kv">
      <dt>Strategy</dt>
      <dd data-testid="memory-strategy">{binding.strategy_id}</dd>
      <dt>Scope</dt>
      <dd>{binding.scope || '—'}</dd>
      <dt>TTL</dt>
      <dd>{ttlLabel(binding.ttl_seconds)}</dd>
    </dl>
    <a class="inspect-link" href="/memory">Inspect memory →</a>
  {:else}
    <p class="empty">No memory strategy configured for this agent.</p>
  {/if}
</div>

<style>
  .memory-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-2) var(--space-4);
    margin: var(--space-0);
  }

  dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .inspect-link {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
