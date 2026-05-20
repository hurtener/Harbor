<script lang="ts">
  // Phase 73k (D-119) — MCP Connections per-server detail view.
  //
  // Six tabs: Tools / Resources / Prompts / OAuth & Auth / Health /
  // Policy. The Tools tab deep-links to `/tools?server=<name>`
  // (Phase 73f's surface — a pure URL consumer). Every Protocol call
  // routes through the typed `mcpApi` client. Raw-HTML trust toggle is
  // admin-gated (UI + server). All values are design tokens.
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { McpDetailState, type McpDetailTab } from '$lib/mcp-connections/state.svelte';

  const detail = new McpDetailState();
  const serverName = $derived(page.params.server ?? '');

  onMount(() => {
    if (serverName) {
      void detail.load(serverName);
    }
  });

  const TABS: { id: McpDetailTab; label: string }[] = [
    { id: 'tools', label: 'Tools' },
    { id: 'resources', label: 'Resources' },
    { id: 'prompts', label: 'Prompts' },
    { id: 'oauth', label: 'OAuth & Auth' },
    { id: 'health', label: 'Health' },
    { id: 'policy', label: 'Policy' }
  ];

  function selectTab(tab: McpDetailTab): void {
    void detail.selectTab(serverName, tab);
  }

  function toggleRawHTML(): void {
    const next = !(detail.server?.raw_html_trusted ?? false);
    void detail.setRawHTMLTrust(serverName, next);
  }
</script>

<section class="detail-page" data-testid="mcp-connections-detail">
  <a class="back-link" href="/mcp-connections">← MCP Connections</a>

  {#if detail.loading}
    <p class="state-msg" data-testid="detail-loading">Loading {serverName}…</p>
  {:else if detail.error}
    <p class="state-msg state-msg-error" data-testid="detail-error">{detail.error}</p>
  {:else if detail.server}
    <header class="detail-head">
      <h1>{detail.server.name}</h1>
      <span class={`chip chip-${detail.server.state}`} data-testid="detail-status">
        {detail.server.state}
      </span>
      <div class="head-actions">
        <button
          type="button"
          class="action-btn"
          data-testid="refresh-discovery"
          onclick={() => detail.refreshDiscovery(serverName)}
        >
          Refresh discovery
        </button>
        <button
          type="button"
          class="action-btn"
          data-testid="test-connection"
          onclick={() => detail.probe(serverName)}
        >
          Test connection
        </button>
        <button
          type="button"
          class="action-btn"
          data-testid="raw-html-toggle"
          disabled={!detail.isAdmin}
          title={detail.isAdmin
            ? 'Toggle raw-HTML rendering trust for this server'
            : 'Requires the admin (tools.admin) scope claim'}
          onclick={toggleRawHTML}
        >
          {detail.server.raw_html_trusted ? 'Raw HTML: trusted' : 'Raw HTML: default-deny'}
        </button>
      </div>
    </header>

    {#if detail.lastActionError}
      <p class="state-msg state-msg-error" data-testid="action-error">
        {detail.lastActionError}
      </p>
    {/if}

    <nav class="tab-strip" aria-label="Server detail tabs">
      {#each TABS as tab (tab.id)}
        <button
          type="button"
          class="tab"
          class:tab-active={detail.activeTab === tab.id}
          data-testid={`tab-${tab.id}`}
          onclick={() => selectTab(tab.id)}
        >
          {tab.label}
        </button>
      {/each}
    </nav>

    <div class="tab-body" data-testid={`tab-body-${detail.activeTab}`}>
      {#if detail.activeTab === 'tools'}
        <p class="tab-note">
          This server exposes {detail.server.tool_count} tool(s).
        </p>
        <a class="deep-link" data-testid="tools-deep-link" href={`/tools?server=${serverName}`}>
          Open in Tools page →
        </a>
      {:else if detail.activeTab === 'resources'}
        <ul class="entity-list">
          {#each detail.resources as r (r.uri)}
            <li data-testid="resource-row">{r.uri}{r.mime_type ? ` · ${r.mime_type}` : ''}</li>
          {/each}
          {#if detail.resources.length === 0}
            <li class="tab-note">No resources advertised.</li>
          {/if}
        </ul>
      {:else if detail.activeTab === 'prompts'}
        <ul class="entity-list">
          {#each detail.prompts as p (p.name)}
            <li data-testid="prompt-row">{p.name}</li>
          {/each}
          {#if detail.prompts.length === 0}
            <li class="tab-note">No prompts advertised.</li>
          {/if}
        </ul>
      {:else if detail.activeTab === 'oauth'}
        <ul class="entity-list">
          {#each detail.bindings as b (b.principal_id + b.binding_scope)}
            <li data-testid="binding-row">
              <span class="binding-scope">{b.binding_scope}</span>
              {b.principal_id}
              <button
                type="button"
                class="action-btn action-btn-sm"
                data-testid="binding-connect"
                onclick={() => detail.connectBinding(serverName, b.principal_id)}
              >
                Connect
              </button>
              <button
                type="button"
                class="action-btn action-btn-sm"
                data-testid="binding-revoke"
                onclick={() => detail.revokeBinding(serverName, b.principal_id)}
              >
                Revoke
              </button>
            </li>
          {/each}
          {#if detail.bindings.length === 0}
            <li class="tab-note">No OAuth bindings configured.</li>
          {/if}
        </ul>
      {:else if detail.activeTab === 'health'}
        <p class="tab-note" data-testid="health-error-rate">
          Transport error rate: {detail.health?.transport_error_rate ?? 0} / min
        </p>
      {:else if detail.activeTab === 'policy'}
        <dl class="policy-grid" data-testid="policy-grid">
          <dt>Timeout (ms)</dt>
          <dd>{detail.toolPolicy?.timeout_ms ?? 0}</dd>
          <dt>Max retries</dt>
          <dd>{detail.toolPolicy?.max_retries ?? 0}</dd>
          <dt>Concurrency cap</dt>
          <dd>{detail.toolPolicy?.concurrency_cap ?? 0}</dd>
        </dl>
      {/if}
    </div>
  {/if}
</section>

<style>
  .detail-page {
    padding: var(--space-8);
  }

  .back-link {
    color: var(--color-accent);
    text-decoration: none;
    font-size: var(--text-sm);
  }

  .detail-head {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    margin: var(--space-4) var(--space-0) var(--space-6);
    flex-wrap: wrap;
  }

  .detail-head h1 {
    font-size: var(--text-xl);
    margin: var(--space-0);
  }

  .head-actions {
    display: flex;
    gap: var(--space-2);
    margin-left: auto;
  }

  .action-btn {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--space-0) solid var(--color-border);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .action-btn-sm {
    padding: var(--space-1) var(--space-2);
    margin-left: var(--space-2);
  }

  .action-btn:disabled {
    color: var(--color-text-muted);
    cursor: not-allowed;
  }

  .tab-strip {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-thin) solid var(--color-border);
    margin-bottom: var(--space-4);
  }

  .tab {
    background: transparent;
    color: var(--color-text-muted);
    border: var(--space-0) solid transparent;
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab-active {
    color: var(--color-text);
    border-bottom: var(--border-thick) solid var(--color-accent);
  }

  .tab-body {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .tab-note {
    color: var(--color-text-muted);
  }

  .deep-link {
    color: var(--color-accent);
    text-decoration: none;
  }

  .entity-list {
    list-style: none;
    padding: var(--space-0);
    margin: var(--space-0);
  }

  .entity-list li {
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-thin) solid var(--color-border);
  }

  .binding-scope {
    display: inline-block;
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    margin-right: var(--space-2);
  }

  .policy-grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-2) var(--space-6);
  }

  .policy-grid dt {
    color: var(--color-text-muted);
  }

  .policy-grid dd {
    margin: var(--space-0);
    font-family: var(--font-mono);
  }

  .state-msg {
    color: var(--color-text-muted);
  }

  .state-msg-error {
    color: var(--color-danger);
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
