<script lang="ts">
  // Harbor Console — Agents detail Identity tab (Phase 73e / D-124).
  //
  // Registration identity (agent_id / incarnation / version_hash —
  // D-059), hosting (locally-hosted vs connect-to-remote — D-060), and
  // the remote A2A AgentCard reference when remote. Read-only inspector
  // (page-agents.md §10 — authoring is CLI-side). Page-specific component.
  import type { AgentGetResponse } from '$lib/protocol/agents.js';

  let { detail }: { detail: AgentGetResponse } = $props();
</script>

<dl class="kv" data-testid="agent-identity-tab">
  <dt>Agent ID</dt>
  <dd class="mono" data-testid="identity-agent-id">{detail.agent.id}</dd>
  <dt>Incarnation</dt>
  <dd>{detail.agent.incarnation}</dd>
  <dt>Version hash</dt>
  <dd class="mono">{detail.agent.version_hash || '—'}</dd>
  <dt>Registration key</dt>
  <dd class="mono">{detail.agent.owner}</dd>
  <dt>Hosting</dt>
  <dd>{detail.agent.hosting}</dd>
  {#if detail.agent_card_ref}
    <dt>Remote AgentCard</dt>
    <dd class="mono">{detail.agent_card_ref}</dd>
  {/if}
  <dt>Registered</dt>
  <dd>{detail.agent.registered_at}</dd>
  <dt>Updated</dt>
  <dd>{detail.agent.updated_at}</dd>
</dl>

<style>
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

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow-wrap: anywhere;
  }
</style>
