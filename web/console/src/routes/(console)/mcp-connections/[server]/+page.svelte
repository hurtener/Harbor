<script lang="ts">
  // Console MCP Connections — per-server detail view (D-121, MCP refactor).
  //
  // The tabbed detail route — six tabs (Tools / Resources / Prompts /
  // OAuth & Auth / Health / Policy). This is the page's depth-bar
  // tabbed-detail surface (CONVENTIONS.md §5: a page clears the depth bar
  // with a DetailRail OR a tabbed detail route — the list page carries
  // the rail, this route carries the tabs).
  //
  // The legacy detail page had a three-state loading chain with NO
  // Disconnected branch and a bespoke `mcpApi`. This refactor routes the
  // header load through the four-state `<PageState>` and every Protocol
  // call through the unified `HarborClient` (via `McpDetailState`). The
  // Tools tab deep-links to `/tools?server=<name>` — a pure URL consumer.
  // The raw-HTML trust toggle is admin-gated (UI + server). Svelte 5
  // runes mode (D-092); design tokens only.
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { PageState, StatusChip } from '$lib/components/ui/index.js';
  import {
    McpDetailState,
    MCP_DETAIL_TABS,
    type McpDetailTab
  } from '$lib/mcp-connections/state.svelte.js';
  import { mcpStatusKind, mcpStateLabel } from '$lib/mcp-connections/status.js';

  const detail = new McpDetailState();
  const serverName = $derived(page.params.server ?? '');

  onMount(() => {
    if (serverName) {
      void detail.load(serverName);
    }
  });

  function selectTab(tab: McpDetailTab): void {
    void detail.selectTab(serverName, tab);
  }

  function toggleRawHTML(): void {
    const next = !(detail.server?.raw_html_trusted ?? false);
    void detail.setRawHTMLTrust(serverName, next);
  }
</script>

<svelte:head>
  <title>{serverName} · MCP Connections · Harbor Console</title>
</svelte:head>

<section class="detail-page" data-testid="mcp-connections-detail">
  <a class="back-link" href="/mcp-connections">← MCP Connections</a>

  <PageState
    status={detail.status}
    error={detail.error}
    onretry={() => void detail.load(serverName)}
  >
    {#if detail.server}
      <header class="detail-head">
        <h1>{detail.server.name}</h1>
        <span data-testid="detail-status">
          <StatusChip
            kind={mcpStatusKind(detail.server.state)}
            label={mcpStateLabel(detail.server.state)}
          />
        </span>
        <div class="head-actions">
          <button
            type="button"
            class="action-btn"
            data-testid="refresh-discovery"
            onclick={() => void detail.refreshDiscovery(serverName)}
          >
            Refresh discovery
          </button>
          <button
            type="button"
            class="action-btn"
            data-testid="test-connection"
            onclick={() => void detail.probe(serverName)}
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
            {detail.server.raw_html_trusted
              ? 'Raw HTML: trusted'
              : 'Raw HTML: default-deny'}
          </button>
        </div>
      </header>

      {#if detail.lastActionError}
        <p class="action-error" data-testid="action-error" role="alert">
          {detail.lastActionError.code}: {detail.lastActionError.message}
        </p>
      {/if}

      <nav class="tab-strip" aria-label="Server detail tabs">
        {#each MCP_DETAIL_TABS as tab (tab.id)}
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
          <a
            class="deep-link"
            data-testid="tools-deep-link"
            href={`/tools?server=${serverName}`}
          >
            Open in Tools page →
          </a>
        {:else if detail.activeTab === 'resources'}
          <ul class="entity-list">
            {#each detail.resources as r (r.uri)}
              <li data-testid="resource-row">
                {r.uri}{r.mime_type ? ` · ${r.mime_type}` : ''}
              </li>
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
                  disabled={!detail.isAdmin}
                  title={detail.isAdmin
                    ? 'Start an OAuth re-bind flow for this principal'
                    : 'Requires the admin (tools.admin) scope claim'}
                  onclick={() => void detail.connectBinding(serverName, b.principal_id)}
                >
                  Connect
                </button>
                <button
                  type="button"
                  class="action-btn action-btn-sm"
                  data-testid="binding-revoke"
                  disabled={!detail.isAdmin}
                  title={detail.isAdmin
                    ? 'Revoke this OAuth binding'
                    : 'Requires the admin (tools.admin) scope claim'}
                  onclick={() => void detail.revokeBinding(serverName, b.principal_id)}
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
  </PageState>
</section>

<style>
  .detail-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-6);
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
    flex-wrap: wrap;
  }

  .detail-head h1 {
    font-size: var(--text-xl);
    margin: var(--space-0);
    color: var(--color-text);
  }

  .head-actions {
    display: flex;
    gap: var(--space-2);
    margin-left: auto;
  }

  .action-btn {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
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

  .action-error {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    background: var(--color-danger-soft);
    color: var(--color-danger);
    font-size: var(--text-sm);
  }

  .tab-strip {
    display: flex;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .tab {
    background: transparent;
    color: var(--color-text-muted);
    border: none;
    border-bottom: var(--border-emphasis-width) solid transparent;
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab-active {
    color: var(--color-text);
    border-bottom-color: var(--color-accent);
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
    border-bottom: var(--border-hairline);
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
</style>
