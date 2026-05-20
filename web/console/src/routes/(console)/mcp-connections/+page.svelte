<script lang="ts">
  // Phase 73k (D-119) — MCP Connections list view.
  //
  // The operator control plane for Harbor's MCP southbound surface. The
  // page is a pure Protocol client: every row round-trips through the
  // typed `mcpApi` client (no hand-rolled fetch — CLAUDE.md §13). All
  // colour / spacing values are design tokens (CLAUDE.md §4.5 #3).
  import { onMount } from 'svelte';
  import { McpListState } from '$lib/mcp-connections/state.svelte';
  import type { MCPServerState } from '$lib/mcp-connections/api';

  const list = new McpListState();

  onMount(() => {
    void list.load();
  });

  const STATE_FILTERS: { label: string; value: MCPServerState | null }[] = [
    { label: 'All servers', value: null },
    { label: 'Online', value: 'online' },
    { label: 'Reconnecting', value: 'reconnecting' },
    { label: 'Offline', value: 'offline' },
    { label: 'Auth pending', value: 'auth_pending' },
    { label: 'Errored', value: 'error' }
  ];

  function stateClass(state: MCPServerState): string {
    return `chip chip-${state}`;
  }
</script>

<section class="mcp-page" data-testid="mcp-connections-list">
  <header class="page-head">
    <h1>MCP Connections</h1>
    <p class="subtitle">
      The configured MCP southbound servers supplying tools, resources, and
      prompts to this runtime's agents.
    </p>
  </header>

  <nav class="filter-bar" aria-label="State filters">
    {#each STATE_FILTERS as f (f.label)}
      <button
        type="button"
        class="filter-chip"
        data-testid={`filter-${f.value ?? 'all'}`}
        onclick={() => list.setStateFilter(f.value)}
      >
        {f.label}
      </button>
    {/each}
  </nav>

  {#if list.loading}
    <p class="state-msg" data-testid="list-loading">Loading MCP servers…</p>
  {:else if list.error}
    <p class="state-msg state-msg-error" data-testid="list-error">{list.error}</p>
  {:else if list.servers.length === 0}
    <p class="state-msg" data-testid="list-empty">
      No MCP servers configured — add servers in your runtime config and
      restart.
    </p>
  {:else}
    <table class="servers-table" data-testid="servers-table">
      <thead>
        <tr>
          <th>Server name</th>
          <th>Status</th>
          <th>Endpoint</th>
          <th>Tools</th>
          <th>Last connect</th>
          <th>OAuth</th>
        </tr>
      </thead>
      <tbody>
        {#each list.servers as srv (srv.name)}
          <tr data-testid={`server-row-${srv.name}`}>
            <td>
              <a class="server-link" href={`/console/mcp-connections/${srv.name}`}>
                {srv.name}
              </a>
            </td>
            <td>
              <span class={stateClass(srv.state)} data-testid={`status-${srv.name}`}>
                {srv.state}
              </span>
            </td>
            <td class="endpoint">{srv.url_or_command}</td>
            <td>{srv.tool_count}</td>
            <td>{srv.last_discovery_at}</td>
            <td>{srv.oauth_binding_count}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</section>

<style>
  .mcp-page {
    padding: var(--space-8);
  }

  .page-head h1 {
    font-size: var(--text-xl);
    margin: var(--space-0) var(--space-0) var(--space-2);
  }

  .subtitle {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    margin: var(--space-0) var(--space-0) var(--space-6);
  }

  .filter-bar {
    display: flex;
    gap: var(--space-2);
    margin-bottom: var(--space-6);
    flex-wrap: wrap;
  }

  .filter-chip {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--space-0) solid transparent;
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    transition: background var(--motion-fast) var(--motion-ease);
  }

  .filter-chip:hover {
    background: var(--color-border);
  }

  .state-msg {
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .state-msg-error {
    color: var(--color-danger);
  }

  .servers-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .servers-table th,
  .servers-table td {
    text-align: left;
    padding: var(--space-3);
    border-bottom: var(--border-thin) solid var(--color-border);
  }

  .servers-table th {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
  }

  .server-link {
    color: var(--color-accent);
    text-decoration: none;
  }

  .server-link:hover {
    text-decoration: underline;
  }

  .endpoint {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .chip {
    display: inline-block;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    color: var(--color-bg);
  }

  .chip-online {
    background: var(--color-mcp-online);
  }

  .chip-reconnecting {
    background: var(--color-mcp-reconnecting);
  }

  .chip-offline {
    background: var(--color-mcp-offline);
  }

  .chip-auth_pending {
    background: var(--color-mcp-auth-pending);
  }

  .chip-error {
    background: var(--color-mcp-error);
  }
</style>
