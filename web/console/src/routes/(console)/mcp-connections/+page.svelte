<script lang="ts">
  // Console MCP Connections — list view (D-121, MCP refactor).
  //
  // The operator control plane for Harbor's MCP southbound surface. The
  // legacy page shipped the THINNEST of the five Console pages: a raw
  // `<table>`, a hand-rolled state-chip nav, a three-state loading chain
  // with NO Disconnected branch, no detail rail, no pagination, no
  // footer. This refactor brings it to the CONVENTIONS.md §5 depth bar:
  //
  //   PageHeader + FilterBar (SavedViewChips + state facets + search) +
  //   DataTable + DetailRail (server summary on row-select) +
  //   Pagination + ConnectionFooter + the four-state PageState.
  //
  // Every Runtime read routes through the unified `HarborClient` via
  // `McpListState` — no hand-rolled `fetch`, no `import.meta.env` URL
  // read (CONVENTIONS.md §6). Svelte 5 runes mode (D-092); tokens only.
  import { onMount } from 'svelte';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    DetailRail,
    RailCard,
    StatusChip,
    Pagination,
    PageState,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  import StateFacetChips from '$lib/components/mcp-connections/StateFacetChips.svelte';
  import { McpListState, DEFAULT_PAGE_SIZE } from '$lib/mcp-connections/state.svelte.js';
  import { McpSavedViews } from '$lib/mcp-connections/saved_views.svelte.js';
  import { mcpStatusKind, mcpStateLabel } from '$lib/mcp-connections/status.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import type { MCPServerView } from '$lib/protocol/mcp.js';

  const list = new McpListState();
  const savedViews = new McpSavedViews();
  // Phase 83r N8 — when `<PageState>` is in the disconnected branch,
  // status chips desaturate (the kind-coloured pill is meaningless
  // without a backing Runtime), and the Save view button disables.
  const disconnected = $derived(list.status === 'disconnected');

  /** The currently row-selected server (drives the detail rail). */
  let selected = $state<MCPServerView | null>(null);

  const COLUMNS: DataTableColumn[] = [
    { key: 'name', label: 'Server name' },
    { key: 'status', label: 'Status' },
    { key: 'endpoint', label: 'Endpoint' },
    { key: 'tools', label: 'Tools', numeric: true },
    { key: 'last_connect', label: 'Last connect' },
    { key: 'oauth', label: 'OAuth', numeric: true }
  ];

  onMount(() => {
    void list.load();
    void savedViews.load();
  });

  function rowKey(row: unknown): string {
    return (row as MCPServerView).name;
  }

  function onRowClick(row: unknown): void {
    selected = row as MCPServerView;
  }

  function applySavedView(id: string): void {
    const filter = savedViews.filterFor(id);
    if (filter !== null) {
      selected = null;
      list.applyFilter(filter, id);
    }
  }

  async function deleteSavedView(id: string): Promise<void> {
    await savedViews.remove(id);
    if (list.activeSavedViewId === id) {
      list.activeSavedViewId = null;
    }
  }

  async function saveCurrentView(): Promise<void> {
    const name = (
      typeof window !== 'undefined' ? window.prompt('Name this saved view') : null
    )?.trim();
    if (name) {
      await savedViews.create(name, list.filter);
    }
  }
</script>

<svelte:head>
  <title>MCP Connections · Harbor Console</title>
</svelte:head>

<section class="mcp-page" data-testid="mcp-connections-list">
  <PageHeader
    title="MCP Connections"
    subtitle="The configured MCP southbound servers supplying tools, resources, and prompts to this runtime's agents."
  />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews.views}
        activeId={list.activeSavedViewId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
    {/snippet}
    {#snippet facets()}
      <StateFacetChips
        active={list.activeStateFilter}
        {disconnected}
        onselect={(state) => {
          selected = null;
          list.setStateFilter(state);
        }}
      />
    {/snippet}
    {#snippet search()}
      <input
        type="search"
        class="search-input"
        placeholder="Search servers…"
        data-testid="mcp-search"
        value={list.search}
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        oninput={(e) => list.setSearch((e.currentTarget as HTMLInputElement).value)}
      />
    {/snippet}
    {#snippet actions()}
      <button
        type="button"
        class="bar-action"
        data-testid="save-view"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}
  </FilterBar>

  <div class="layout">
    <div class="main-col">
      <PageState
        status={list.status}
        error={list.error}
        onretry={() => void list.load()}
      >
        {#snippet empty()}
          <p class="empty-headline" data-testid="list-empty">
            No MCP servers configured — add servers in your runtime config
            and restart.
          </p>
        {/snippet}

        <DataTable
          columns={COLUMNS}
          rows={list.visibleServers}
          {rowKey}
          onrowclick={onRowClick}
        >
          {#snippet row(r)}
            {@const srv = r as MCPServerView}
            <td data-testid={`server-row-${srv.name}`}>
              <a
                class="server-link"
                href={`/mcp-connections/${srv.name}`}
                onclick={(e) => e.stopPropagation()}
              >
                {srv.name}
              </a>
            </td>
            <td>
              <span data-testid={`status-${srv.name}`}>
                <StatusChip
                  kind={mcpStatusKind(srv.state)}
                  label={mcpStateLabel(srv.state)}
                  desaturated={disconnected}
                />
              </span>
            </td>
            <td class="endpoint">{srv.url_or_command}</td>
            <td class="numeric">{srv.tool_count}</td>
            <td>{srv.last_discovery_at}</td>
            <td class="numeric">{srv.oauth_binding_count}</td>
          {/snippet}
          {#snippet empty()}
            <span>No servers match the current filter.</span>
          {/snippet}
        </DataTable>
      </PageState>

      {#if list.status === 'ready' || list.status === 'empty'}
        <Pagination
          page={list.page}
          pageSize={list.pageSize}
          total={list.total}
          pageSizeOptions={[DEFAULT_PAGE_SIZE, 50, 100]}
          onpage={(p) => list.goToPage(p)}
          onpagesize={(s) => list.setPageSize(s)}
        />
      {/if}
    </div>

    <DetailRail>
      {#if selected}
        <RailCard title="Server">
          <p class="rail-name" data-testid="rail-server-name">{selected.name}</p>
          <StatusChip
            kind={mcpStatusKind(selected.state)}
            label={mcpStateLabel(selected.state)}
          />
        </RailCard>
        <RailCard title="Transport">
          <dl class="rail-grid">
            <dt>Transport</dt>
            <dd>{selected.transport}</dd>
            <dt>Endpoint</dt>
            <dd class="mono">{selected.url_or_command}</dd>
            <dt>Last connect</dt>
            <dd>{selected.last_discovery_at}</dd>
          </dl>
        </RailCard>
        <RailCard title="Discovery">
          <dl class="rail-grid">
            <dt>Tools</dt>
            <dd>{selected.tool_count}</dd>
            <dt>Resources</dt>
            <dd>{selected.resource_count}</dd>
            <dt>Prompts</dt>
            <dd>{selected.prompt_count}</dd>
            <dt>OAuth bindings</dt>
            <dd>{selected.oauth_binding_count}</dd>
          </dl>
        </RailCard>
        <RailCard title="Actions">
          <a class="rail-link" href={`/mcp-connections/${selected.name}`}>
            Open server detail →
          </a>
        </RailCard>
      {:else}
        <RailCard title="Server">
          <p class="rail-hint">Select a server row to see its summary.</p>
        </RailCard>
      {/if}
    </DetailRail>
  </div>
</section>

<style>
  .mcp-page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-6);
  }

  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: var(--space-0);
  }

  .search-input {
    width: 100%;
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .bar-action {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
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

  td.numeric {
    text-align: right;
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-name {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-weight: 600;
    color: var(--color-text);
  }

  .rail-grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .rail-grid dt {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .rail-grid dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .rail-grid dd.mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    word-break: break-all;
  }

  .rail-hint,
  .rail-link {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-link {
    color: var(--color-accent);
    text-decoration: none;
  }
</style>
