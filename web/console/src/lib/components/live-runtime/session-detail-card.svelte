<script lang="ts">
  // Harbor Console — Live Runtime session detail card (Phase 73b /
  // D-126). The right-rail header card: the session identity triple,
  // the bound agent, the session status, and the Cost / Last error /
  // Tenant fields.
  //
  // NO session-level `priority` field anywhere — D-065 carve-out. Only
  // task-level priority (on the per-task detail pane) is a V1 surface.
  //
  // Svelte 5 runes mode (D-092); design tokens only.
  import type { ConnectionIdentity } from '$lib/connection.js';

  let {
    identity,
    agentName,
    sessionStatus,
    costUSD,
    lastError,
    tenant
  }: {
    /** The session's `(tenant, user, session)` triple. */
    identity: ConnectionIdentity;
    /** The agent bound to the session (single-agent default in V1). */
    agentName: string;
    /** The session's lifecycle status. */
    sessionStatus: string;
    /** The aggregated session cost in USD (from `llm.cost.recorded`). */
    costUSD: number;
    /** The most recent failure summary, or null when none. */
    lastError: string | null;
    /** The session's tenant — from the identity tuple, never widened. */
    tenant: string;
  } = $props();
</script>

<dl class="session-card" data-testid="session-detail-card">
  <dt>Session</dt>
  <dd data-testid="session-detail-session">{identity.session}</dd>
  <dt>User</dt>
  <dd>{identity.user}</dd>
  <dt>Tenant</dt>
  <dd data-testid="session-detail-tenant">{tenant}</dd>
  <dt>Agent</dt>
  <dd>{agentName}</dd>
  <dt>Status</dt>
  <dd>{sessionStatus}</dd>
  <dt>Cost</dt>
  <dd data-testid="session-detail-cost">${costUSD.toFixed(4)}</dd>
  <dt>Last error</dt>
  <dd data-testid="session-detail-last-error">{lastError ?? 'none'}</dd>
</dl>

<style>
  .session-card {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .session-card dt {
    color: var(--color-text-muted);
  }

  .session-card dd {
    margin: var(--space-0);
    color: var(--color-text);
    overflow-wrap: anywhere;
  }
</style>
