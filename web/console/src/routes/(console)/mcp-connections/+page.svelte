<script lang="ts">
  // Harbor Console — MCP Connections page (`/mcp-connections`) — Phase 108m
  // rebuild (D-185; supersedes the Phase 73k / D-119 pre-chrome layout).
  //
  // The operator control plane for Harbor's MCP southbound surface — the
  // configured MCP servers that supply tools / resources / prompts to the
  // runtime's agents. Phase 108m rethemes it to the carded, viewport-locked
  // Events-108h / Tools-108k composition (a filter card + a master-detail
  // layout: the servers TABLE on the left, a right-rail server detail on the
  // right — the table stays visible, the rail shows the selected server's
  // full detail or the catalog-overview idle state). It drops the per-page
  // header (the breadcrumb / ⌘K / footer are app-shell chrome, 108b) and
  // the separate tabbed-detail route (the rail is now the single detail
  // surface — §13: no two parallel implementations of one feature).
  //
  // It is a PURE Protocol consumer — the `mcp.servers.*` surface shipped in
  // Phase 73k / D-119; this phase builds NO new Protocol method. Every datum
  // + action is real-wired (PAGE-POLISH §3 — live-verified against the
  // validation runtime's `youtube` MCP server): the catalog ← `mcp.servers.
  // list`; the detail header + tabs ← `mcp.servers.{get,resources,prompts,
  // bindings.list,policy}` + `tools.list`; Refresh discovery / Test
  // connection ← REAL `mcp.servers.{refresh_discovery,probe}` (honest re-read
  // / actual probe outcome, §13); the raw-HTML toggle + OAuth admin verbs ←
  // REAL admin `mcp.servers.*` (admin-gated, D-079); the Recent-events card ←
  // LIVE `events.subscribe` (honest-empty). Saved-view chips ← Console-local
  // (D-061).
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient + connection.ts
  // only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import { FilterBar, SavedViewChips, Pagination, PageState } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import StateFacetChips from '$lib/components/mcp-connections/StateFacetChips.svelte';
  import ServersTable from '$lib/components/mcp-connections/ServersTable.svelte';
  import McpDetailRail from '$lib/components/mcp-connections/McpDetailRail.svelte';
  import McpOverviewCard from '$lib/components/mcp-connections/McpOverviewCard.svelte';
  import { McpListState, McpDetailState, DEFAULT_PAGE_SIZE } from '$lib/mcp-connections/state.svelte.js';
  import { McpSavedViews } from '$lib/mcp-connections/saved_views.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const list = new McpListState();
  const detail = new McpDetailState();
  const savedViews = new McpSavedViews();
  const disconnected = $derived(list.disconnected);

  /** The currently row-selected server name (drives the detail rail). */
  let selectedName = $state<string | null>(null);

  onMount(() => {
    void list.load();
    void savedViews.load();
    detail.boot(injectedClient);
    return () => detail.close();
  });

  function onSelect(name: string): void {
    selectedName = name;
    void detail.load(name, injectedClient);
  }

  function closeDetail(): void {
    selectedName = null;
    detail.clear();
  }

  function applySavedView(id: string): void {
    const filter = savedViews.filterFor(id);
    if (filter !== null) {
      closeDetail();
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
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={savedViews.views}
          activeId={list.activeSavedViewId}
          onselect={applySavedView}
          ondelete={(id) => void deleteSavedView(id)}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="save-view"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : 'Save the current filter as a view'}
          onclick={() => void saveCurrentView()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <StateFacetChips
          active={list.activeStateFilter}
          {disconnected}
          onselect={(state) => {
            closeDetail();
            list.setStateFilter(state);
          }}
        />
      {/snippet}

      {#snippet search()}
        <input
          type="search"
          class="bar-input search-input"
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
          data-testid="mcp-clear-filters"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => {
            closeDetail();
            list.clearFilters();
          }}
        >
          Clear
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="mcp-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => void list.load()}
        >
          Refresh
        </button>
      {/snippet}
    </FilterBar>
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState status={list.displayStatus} error={list.error} onretry={() => void list.load()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="list-empty">
            <p class="empty-headline">No MCP servers match this view</p>
            <p class="empty-detail">
              No MCP servers are configured (add servers in your runtime config
              and restart), or the active filters yield zero rows.
            </p>
            <button type="button" class="bar-action" onclick={() => { closeDetail(); list.clearFilters(); }}>
              Clear filters
            </button>
          </div>
        {/snippet}

        <div class="table-scroll">
          <ServersTable
            rows={list.visibleServers}
            activeName={selectedName}
            {disconnected}
            onselect={onSelect}
          />
        </div>
      </PageState>

      {#if list.displayStatus === 'ready' || list.displayStatus === 'empty'}
        <Pagination
          page={list.page}
          pageSize={list.pageSize}
          total={list.total}
          pageSizeOptions={[DEFAULT_PAGE_SIZE, 50, 100]}
          onpage={(p) => list.goToPage(p)}
          onpagesize={(s) => list.setPageSize(s)}
        />
      {/if}
    </section>

    <!-- Right column — the per-server detail when a row is selected (and the
         Console is connected), else the catalog-overview idle state. Both
         fill the column + scroll INTERNALLY (PAGE-POLISH §6). The
         `!disconnected` guard keeps a disconnected page on the single idle
         overview (with `—` placeholders) rather than a second empty surface. -->
    {#if !disconnected && selectedName !== null}
      <McpDetailRail {detail} onclose={closeDetail} />
    {:else}
      <section class="panel card idle-card" data-testid="mcp-rail-idle">
        <h2 class="panel-title">Catalog overview</h2>
        <McpOverviewCard counts={list.overview} {disconnected} />
        <p class="idle-hint">
          Click a server to inspect its detail — transport, discovery counts,
          tools, resources, prompts, OAuth bindings, policy, and live events.
        </p>
      </section>
    {/if}
  </div>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the catalog table + the right rail scroll
     internally (PAGE-POLISH §6 — the Events / Tools pattern). */
  .mcp-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-title {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  /* ---- filter card (fixed height) ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  /* Kill the shared FilterBar's own vertical padding so the strip packs
     tight (the card already supplies the padding — Events-108h pattern). */
  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .bar-input {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .search-input {
    flex: 1;
    min-width: var(--size-input-compact);
  }

  .bar-action {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
    text-decoration: none;
  }

  .bar-action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  /* ---- layout (fills remaining height; both columns scroll internally) ---- */
  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  .table-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .idle-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
    overflow-y: auto;
  }

  .idle-hint {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--space-8);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-8) var(--space-4);
    text-align: center;
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    max-width: var(--size-modal-width);
  }
</style>
