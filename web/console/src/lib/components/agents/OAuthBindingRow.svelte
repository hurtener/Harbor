<script lang="ts">
  // Harbor Console — Agents detail Tools-tab OAuth binding row (Phase
  // 73e / D-124). One tool binding + its per-binding OAuth status
  // (D-083) + the Connect / Reconnect / Revoke affordances.
  //
  // # OAuth action degradation (CONVENTIONS.md §5, CLAUDE.md §13)
  //
  // Connect / Reconnect / Revoke initiate the SHIPPED `tool.auth_required`
  // event flow (D-083) — the runtime owns token storage; the Console
  // never receives a raw token. The flow is initiated from the per-tool
  // detail surface on the Tools page; this row deep-links there rather
  // than re-implementing a parallel OAuth path (CLAUDE.md §13 — no two
  // parallel implementations of one feature). A binding whose
  // `auth_status` carries no OAuth (`no_auth` / `headers`) shows no OAuth
  // action — the buttons are not faked, they are simply absent.
  import { StatusChip, type StatusKind } from '$lib/components/ui';
  import type { AgentToolBinding } from '$lib/protocol/agents.js';

  let { binding }: { binding: AgentToolBinding } = $props();

  /** Reports whether this binding carries an OAuth credential. */
  const isOAuth = $derived(
    binding.auth_status === 'oauth_user_bound' ||
      binding.auth_status === 'oauth_agent_bound' ||
      binding.auth_status === 'oauth_expired'
  );

  /** Reports whether the binding's OAuth token is expired. */
  const isExpired = $derived(binding.auth_status === 'oauth_expired');

  /** Maps the auth status onto a StatusChip kind. */
  function authKind(status: string): StatusKind {
    if (status === 'oauth_expired') return 'danger';
    if (status === 'oauth_user_bound' || status === 'oauth_agent_bound') {
      return 'success';
    }
    return 'neutral';
  }
</script>

<div class="binding-row" data-testid="agent-oauth-binding" data-tool-id={binding.tool_id}>
  <div class="binding-meta">
    <a class="tool-name" href={`/tools/${binding.tool_id}`}>
      {binding.tool_name || binding.tool_id}
    </a>
    <span class="transport">{binding.transport}</span>
  </div>
  <div class="binding-auth">
    <StatusChip kind={authKind(binding.auth_status)} label={binding.auth_status} />
    {#if binding.binding_scope}
      <span class="scope">scope: {binding.binding_scope}</span>
    {/if}
  </div>
  {#if isOAuth}
    <div class="oauth-actions" data-testid="agent-oauth-actions">
      <!-- The Connect / Reconnect / Revoke flow is initiated from the
           per-tool Tools-page detail (the shipped tool.auth_required
           surface — D-083). This row deep-links there rather than
           cloning the OAuth flow. -->
      <a
        class="oauth-link"
        data-testid="agent-oauth-manage"
        href={`/tools/${binding.tool_id}`}
        title="Manage this binding's OAuth on the Tools page"
      >
        {isExpired ? 'Reconnect' : 'Manage OAuth'}
      </a>
    </div>
  {/if}
</div>

<style>
  .binding-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .binding-meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .tool-name {
    font-size: var(--text-sm);
    color: var(--color-accent);
  }

  .transport {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .binding-auth {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .scope {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .oauth-link {
    font-size: var(--text-xs);
    color: var(--color-accent);
  }
</style>
