<script lang="ts">
  // Harbor Console — agent-config add-MCP-connection form (92f).
  //
  // Adds a NEW MCP server connection (`agent_config.add_mcp_connection`):
  // the async dial + initialize handshake + possible OAuth path. The form
  // surfaces the terminal attach lifecycle from the response state
  // ("online" | "failed" | "auth_required") plus the live `mcp.connection.*`
  // events (pending → added / failed / auth_required). Secret auth headers
  // are entered as `Key: value` lines and are NEVER rendered back after
  // submit (the value is write-only — D-235). Writes are admin-gated
  // (disabled-with-tooltip — the 92b precedent). Page-specific; Tokens only;
  // Svelte 5 runes (D-092).
  import { StatusChip } from '$lib/components/ui';
  import type { StatusKind } from '$lib/components/ui';
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();

  /** Maps the attach state onto a chip kind + an operator advisory. */
  const stateKind = $derived<StatusKind>(
    panel.addConnState === 'online'
      ? 'success'
      : panel.addConnState === 'auth_required'
        ? 'warning'
        : panel.addConnState === 'failed'
          ? 'danger'
          : 'neutral'
  );
  const stateAdvisory = $derived(
    panel.addConnState === 'auth_required'
      ? 'Awaiting authorization — the connection paused for OAuth. Complete the flow to resume.'
      : panel.addConnState === 'failed'
        ? panel.addConnReason || 'The dial or initialize handshake failed.'
        : panel.addConnState === 'online'
          ? 'Connected — the server’s tools are available on the next run.'
          : ''
  );
</script>

<div class="card-body" data-testid="agentcfg-add-connection">
  <p class="note">
    Attach a new MCP server to this agent. Harbor dials, runs the initialize
    handshake, and — for servers behind OAuth — pauses for authorization. Secret
    headers are used for the live attach only and are never persisted or shown back.
  </p>

  <form
    onsubmit={(e) => {
      e.preventDefault();
      void panel.addConnection();
    }}
  >
    <div class="form-grid">
      <label class="field">
        <span class="field-label">Name</span>
        <input
          type="text"
          data-testid="agentcfg-conn-name"
          placeholder="github"
          bind:value={panel.connName}
          disabled={!panel.hasAdminScope}
        />
      </label>
      <label class="field">
        <span class="field-label">Transport</span>
        <select
          data-testid="agentcfg-conn-transport"
          bind:value={panel.connTransport}
          disabled={!panel.hasAdminScope}
        >
          <option value="stdio">stdio</option>
          <option value="http">http</option>
        </select>
      </label>
      {#if panel.connTransport === 'stdio'}
        <label class="field field-wide">
          <span class="field-label">Command (argv, space-separated)</span>
          <input
            type="text"
            data-testid="agentcfg-conn-command"
            placeholder="npx -y @modelcontextprotocol/server-github"
            bind:value={panel.connCommand}
            disabled={!panel.hasAdminScope}
          />
        </label>
      {:else}
        <label class="field field-wide">
          <span class="field-label">URL</span>
          <input
            type="text"
            data-testid="agentcfg-conn-url"
            placeholder="https://mcp.example.com/sse"
            bind:value={panel.connUrl}
            disabled={!panel.hasAdminScope}
          />
        </label>
      {/if}
      <label class="field field-wide">
        <span class="field-label">Secret headers (Key: value, one per line)</span>
        <textarea
          rows="2"
          data-testid="agentcfg-conn-headers"
          placeholder="Authorization: Bearer …"
          bind:value={panel.connHeaders}
          disabled={!panel.hasAdminScope}
        ></textarea>
      </label>
    </div>

    <div class="actions">
      <button
        type="submit"
        class="primary"
        data-testid="agentcfg-conn-add"
        disabled={!panel.hasAdminScope || panel.addConnBusy}
        title={panel.hasAdminScope
          ? 'Dial and attach the MCP server'
          : 'Adding an MCP connection requires the admin scope claim'}
      >
        {panel.addConnBusy ? 'Connecting…' : 'Add connection'}
      </button>
      {#if !panel.hasAdminScope}
        <span class="hint" data-testid="agentcfg-conn-disabled-hint">
          Requires the admin scope claim.
        </span>
      {/if}
      {#if panel.addConnState}
        <span data-testid="agentcfg-conn-state">
          <StatusChip kind={stateKind} label={panel.addConnState} />
        </span>
      {/if}
    </div>

    {#if stateAdvisory}
      <p class="advisory" data-testid="agentcfg-conn-advisory">{stateAdvisory}</p>
    {/if}
    {#if panel.addConnError}
      <p class="form-error" data-testid="agentcfg-conn-error">
        {panel.addConnError.code}: {panel.addConnError.message}
      </p>
    {/if}
  </form>

  <!-- The runtime-added connections editor: per-connection OAuth-discovery
       allow-list (grant/revoke — set_mcp_discovery_origins) + removal
       (remove_mcp_connection). The write is single-homed here beside
       diff/rollback; the MCP Connections page deep-links to it (D-302). -->
  {#if panel.activeConnections.length > 0}
    <div class="conn-list" data-testid="agentcfg-conn-list">
      <p class="field-label">Runtime-added connections</p>
      {#each panel.activeConnections as conn (conn.name)}
        <div class="conn-row" data-testid="agentcfg-conn-row-{conn.name}">
          <div class="conn-head">
            <span class="conn-name">{conn.name}</span>
            <span class="conn-meta">{conn.transport}{conn.url ? ` · ${conn.url}` : ''}</span>
            <button
              type="button"
              class="danger"
              data-testid="agentcfg-conn-remove-{conn.name}"
              disabled={!panel.hasAdminScope || panel.removeBusy !== ''}
              title={panel.hasAdminScope
                ? 'Remove this runtime-added connection (records a revision; detaches next run)'
                : 'Removing a connection requires the admin scope claim'}
              onclick={() => void panel.removeConnection(conn.name)}
            >
              {panel.removeBusy === conn.name ? 'Removing…' : 'Remove'}
            </button>
          </div>
          {#if conn.transport === 'http'}
            <label class="field field-wide">
              <span class="field-label">OAuth-discovery allowed origins (one per line)</span>
              <textarea
                rows="2"
                data-testid="agentcfg-conn-origins-{conn.name}"
                placeholder="https://auth.example.com"
                value={panel.discoveryInputFor(conn)}
                disabled={!panel.hasAdminScope}
                oninput={(e) => (panel.discoveryDraft[conn.name] = e.currentTarget.value)}
              ></textarea>
            </label>
            <div class="actions">
              <button
                type="button"
                class="primary"
                data-testid="agentcfg-conn-origins-save-{conn.name}"
                disabled={!panel.hasAdminScope || panel.discoveryBusy !== ''}
                title={panel.hasAdminScope
                  ? 'Full-replace this connection’s discovery allow-list (applies live)'
                  : 'Editing discovery origins requires the admin scope claim'}
                onclick={() => void panel.setDiscoveryOrigins(conn.name)}
              >
                {panel.discoveryBusy === conn.name ? 'Saving…' : 'Save origins'}
              </button>
            </div>
          {/if}
        </div>
      {/each}
      {#if panel.discoveryError}
        <p class="form-error" data-testid="agentcfg-conn-origins-error">
          {panel.discoveryError.code}: {panel.discoveryError.message}
        </p>
      {/if}
      {#if panel.removeError}
        <p class="form-error" data-testid="agentcfg-conn-remove-error">
          {panel.removeError.code}: {panel.removeError.message}
        </p>
      {/if}
    </div>
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .note {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  form {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2) var(--space-3);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field-wide {
    grid-column: 1 / -1;
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  input,
  select,
  textarea {
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  textarea {
    resize: vertical;
    font-family: var(--font-mono);
  }
  input:disabled,
  select:disabled,
  textarea:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .primary {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .primary:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .advisory {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
  .conn-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border-top: var(--border-hairline);
    padding-top: var(--space-2);
  }
  .conn-row {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
  }
  .conn-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .conn-name {
    font-weight: var(--font-weight-medium);
  }
  .conn-meta {
    flex: 1;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .danger {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-surface);
    color: var(--color-danger);
    border-color: var(--color-danger);
  }
  .danger:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
