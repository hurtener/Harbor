<script lang="ts">
  // Harbor Console — Memory page (`/memory`) — Phase 108n rebuild (D-186;
  // supersedes the Phase 73j / D-118 pre-chrome layout).
  //
  // The per-identity inspector for the runtime's memory subsystem. Phase 108n
  // rethemes it to the carded, viewport-locked Tools-108k / MCP-108m
  // composition (a filter card + a TABLE-left + a stacked right-rail of
  // health / live-events / selected-item cards), refactors the ~728-line king
  // file into a `MemoryPageState` controller + pure `derive.ts` projections +
  // focused `MemoryTable` / `MemoryEventsCard` components, and drops the
  // per-page page header (the breadcrumb / ⌘K / footer are app-shell chrome,
  // 108b).
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified against
  // the validation runtime's real `sqlite` / `rolling_summary` memory):
  //   - records + aggregates ← `memory.list`
  //   - selected-record detail ← `memory.get` (the value is base64-decoded —
  //     the pre-chrome viewer rendered raw base64; fixed in `derive.ts`)
  //   - right-rail health ← `memory.health`
  //   - right-rail event feed ← LIVE `events.subscribe` (`memory.identity_
  //     rejected` + `memory.health_changed` + `memory.recovery_dropped`) —
  //     this UPGRADES the pre-chrome deferred placeholder (D-132/W5) to a real
  //     live feed; honest-empty when quiet (§13)
  //   - saved-view chips + NDJSON/CSV export ← Console-local (D-061)
  //
  // V1 is VIEW-ONLY: the memory mutation surface (add / edit / evict) is NOT
  // shipped (page-memory.md §10), so the bulk-action bar stays disabled-with-
  // tooltip — never a fabricated action (§13). Strategy-trace + Promotions are
  // not shipped Protocol methods (a live probe returned 404), so this page
  // surfaces no fabricated tabs for them.
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient + connection.ts
  // only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import {
    FilterBar,
    SavedViewChips,
    BulkActionBar,
    DetailRail,
    RailCard,
    Pagination,
    PageState
  } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import MemoryTable from '$lib/components/memory/MemoryTable.svelte';
  import MemoryHealthCard from '$lib/components/memory/MemoryHealthCard.svelte';
  import MemoryEventsCard from '$lib/components/memory/MemoryEventsCard.svelte';
  import SelectedItemDetail from '$lib/components/memory/SelectedItemDetail.svelte';
  import StrategyTraceCard from '$lib/components/memory/StrategyTraceCard.svelte';
  import AddMemoryComposer from '$lib/components/memory/AddMemoryComposer.svelte';
  import StrategyOverlayChipRow from '$lib/components/memory/StrategyOverlayChipRow.svelte';
  import { MemoryPageState } from '$lib/memory/state.svelte.js';
  import type { MemoryItem } from '$lib/protocol/memory-types.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const mem = new MemoryPageState();
  const disconnected = $derived(mem.disconnected);
  let saveName = $state('');

  /** Six row indices for the loading skeleton. */
  const SKELETON_ROWS = [0, 1, 2, 3, 4, 5];

  onMount(() => {
    mem.load(injectedClient);
    // The live event subscription opens in load() only for the production
    // client; with an injected harness client the page opens it here so the
    // feed is exercised end-to-end without forcing an EventSource in unit tests.
    if (injectedClient !== undefined) mem.openEvents();
    return () => mem.close();
  });

  function onSaveView(): void {
    void mem.saveCurrentView(saveName);
    saveName = '';
  }

  /** Console-local NDJSON / CSV export of the current page (D-061). */
  function exportSnapshot(format: 'ndjson' | 'csv'): void {
    if (typeof globalThis.document === 'undefined') return;
    const items: MemoryItem[] = mem.items;
    let body: string;
    if (format === 'ndjson') {
      body = items.map((it) => JSON.stringify(it)).join('\n');
    } else {
      const header = 'key,strategy,scope,driver,size_bytes';
      const rows = items.map((it) => `${it.key},${it.strategy},${it.scope},${it.driver},${it.size_bytes}`);
      body = [header, ...rows].join('\n');
    }
    const blob = new globalThis.Blob([body], {
      type: format === 'ndjson' ? 'application/x-ndjson' : 'text/csv'
    });
    const url = globalThis.URL.createObjectURL(blob);
    const a = globalThis.document.createElement('a');
    a.href = url;
    a.download = `memory-export.${format}`;
    a.click();
    globalThis.URL.revokeObjectURL(url);
  }
</script>

<svelte:head>
  <title>Memory · Harbor Console</title>
</svelte:head>

<section class="memory-page" data-testid="memory-page">
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={mem.savedViews}
          activeId={mem.activeViewId}
          onselect={(id) => void mem.applySavedView(id)}
          ondelete={(id) => void mem.deleteSavedView(id)}
        />
        <input
          class="bar-input save-input"
          type="text"
          placeholder="Save current as…"
          bind:value={saveName}
          data-testid="memory-save-view-name"
          disabled={mem.savedFiltersDB === null || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => e.key === 'Enter' && onSaveView()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="memory-save-view"
          disabled={mem.savedFiltersDB === null || saveName.trim().length === 0 || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : mem.savedFiltersDB === null
              ? 'Saved views need a profile unlocked in Settings'
              : 'Save the current filter as a view'}
          onclick={onSaveView}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <label class="facet">
          <span>Scope</span>
          <select
            value={mem.scopeFacet}
            data-testid="memory-scope-facet"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => mem.applyScopeFacet(e.currentTarget.value)}
          >
            <option value="">All</option>
            <option value="session">session</option>
            <option value="user">user</option>
            <option value="tenant">tenant</option>
          </select>
        </label>
        <label class="facet">
          <span>Driver</span>
          <select
            value={mem.driverFacet}
            data-testid="memory-driver-facet"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => mem.applyDriverFacet(e.currentTarget.value)}
          >
            <option value="">All</option>
            <option value="inmem">inmem</option>
            <option value="sqlite">sqlite</option>
            <option value="postgres">postgres</option>
          </select>
        </label>
      {/snippet}

      {#snippet search()}
        <input
          class="bar-input search-input"
          type="search"
          placeholder="Search memory content…"
          data-testid="memory-content-search"
          value={mem.contentSearch}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onchange={(e) => mem.applyContentSearch(e.currentTarget.value)}
        />
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="memory-clear-filters"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => mem.clearFilters()}
        >
          Clear
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="memory-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => mem.refresh()}
        >
          Refresh
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="memory-export-ndjson"
          disabled={mem.items.length === 0}
          onclick={() => exportSnapshot('ndjson')}
        >
          Export NDJSON
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="memory-export-csv"
          disabled={mem.items.length === 0}
          onclick={() => exportSnapshot('csv')}
        >
          Export CSV
        </button>
      {/snippet}
    </FilterBar>

    <StrategyOverlayChipRow
      selected={mem.strategyOverlay}
      onSelect={(s) => mem.applyStrategyOverlay(s)}
    />

    <BulkActionBar count={mem.checkedKeys.size} onclear={() => mem.setSelection(new Set())}>
      {#snippet actions()}
        <!-- Phase 108n (D-186): the memory mutation surface is now LIVE.
             "Evict selected" calls the REAL admin `memory.delete` per checked
             turn (D-079) — disabled-with-tooltip without the admin claim, never
             a fabricated success (§13). "Refresh TTL" / "Pin" remain absent:
             TTL refresh + a pin dimension are not shipped Protocol surfaces
             (D-065 — no priority/pin field), so no fabricated buttons. -->
        <button
          type="button"
          class="bar-action"
          data-testid="memory-evict-selected"
          disabled={!mem.canAdmin || mem.mutationBusy}
          title={mem.canAdmin
            ? 'Evict the selected memory turns (audited)'
            : 'Requires the admin scope claim — memory.delete is an admin Protocol method (D-079).'}
          onclick={() => void mem.evictSelected()}
        >
          {mem.mutationBusy ? 'Evicting…' : 'Evict selected'}
        </button>
      {/snippet}
    </BulkActionBar>
    {#if mem.mutationResult !== null}
      <p
        class="inline-result"
        class:ok={mem.mutationResult.ok}
        class:err={!mem.mutationResult.ok}
        data-testid="memory-mutation-result"
      >
        {mem.mutationResult.message}
      </p>
    {/if}
  </section>

  <div class="layout">
    <section class="panel card table-card">
      <PageState status={mem.displayStatus} error={mem.pageError} onretry={() => mem.refresh()}>
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each SKELETON_ROWS as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="memory-empty">
            <p class="empty-headline">No memory items in this scope</p>
            <p class="empty-detail">
              Memory builds up during runs — open
              <a href="/live-runtime">Live Runtime</a> to start one, or clear the
              active filters.
            </p>
            <button type="button" class="bar-action" onclick={() => mem.clearFilters()}>
              Clear filters
            </button>
          </div>
        {/snippet}

        <div class="table-scroll">
          <MemoryTable
            rows={mem.items}
            selected={mem.checkedKeys}
            activeKey={mem.selectedKey}
            onselect={(key) => void mem.loadDetail(key)}
            onselectionchange={(s) => mem.setSelection(s)}
          />
        </div>
      </PageState>

      {#if mem.displayStatus === 'ready' || mem.displayStatus === 'empty'}
        <Pagination
          page={mem.page}
          pageSize={mem.pageSize}
          total={mem.totalRows}
          onpage={(p) => mem.changePage(p)}
          onpagesize={(s) => mem.changePageSize(s)}
        />
      {/if}
    </section>

    <DetailRail>
      <RailCard title="Memory health">
        <MemoryHealthCard aggregate={mem.health} />
      </RailCard>
      <RailCard title="Strategy trace">
        <StrategyTraceCard trace={mem.strategyTrace} />
      </RailCard>
      <RailCard title="Memory events">
        <MemoryEventsCard entries={mem.recentEvents} streamState={mem.streamState} />
      </RailCard>
      <RailCard title="Add memory">
        <AddMemoryComposer
          canAdmin={mem.canAdmin}
          busy={mem.mutationBusy}
          onadd={(u, a) => void mem.addTurn(u, a)}
        />
      </RailCard>
      <RailCard title="Selected item">
        <!-- The rail gets its OWN nested <PageState> (CONVENTIONS.md §4): a
             rail-load failure surfaces in the rail, not the whole page. -->
        <PageState
          status={mem.detailStatus}
          error={mem.detailError}
          onretry={() => mem.selectedKey && void mem.loadDetail(mem.selectedKey)}
        >
          {#snippet empty()}
            <p class="rail-empty">Select a memory row to inspect its detail.</p>
          {/snippet}
          {#if mem.detail}
            <SelectedItemDetail
              detail={mem.detail}
              canEvict={mem.canAdmin}
              onEvict={(key) => void mem.evict(key)}
            />
          {/if}
        </PageState>
      </RailCard>
    </DetailRail>
  </div>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the records table + the right rail scroll
     internally (PAGE-POLISH §6 — the Tools / MCP pattern). */
  .memory-page {
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

  /* ---- filter card (fixed height) ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .facet {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .facet select {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-sm);
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

  .save-input {
    width: var(--size-input-compact);
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

  .inline-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .inline-result.ok {
    color: var(--color-success);
  }

  .inline-result.err {
    color: var(--color-danger);
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

  /* The DetailRail fills the column height + scrolls its stacked cards. */
  .layout :global(.detail-rail) {
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--layout-table-row-height);
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

  .empty-detail a {
    color: var(--color-accent);
  }

  .rail-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
