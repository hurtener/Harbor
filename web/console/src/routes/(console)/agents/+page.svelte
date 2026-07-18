<script lang="ts">
  // Harbor Console — Agents page (`/agents`), list mode — Phase 108l
  // rebuild (D-184; supersedes the Phase 73e / D-124 pre-chrome layout).
  //
  // The registered-agent catalog browser. Phase 108l rethemes it to the
  // carded, viewport-locked Events-108h / Background-Jobs-108j / Tools-108k
  // composition (a hero rollup + a filter card + a cards canvas that scrolls
  // internally) and refactors the ~503-line king file into an
  // `AgentsListPageState` controller + pure `derive.ts` projections. It drops
  // the per-page header (the breadcrumb / ⌘K / footer are app-shell chrome,
  // 108b).
  //
  // Every datum + action is real-wired (PAGE-POLISH §3 — live-verified against
  // the validation runtime's seeded registry):
  //   - hero rollup ← `agents.metrics` (registry-wide)
  //   - cards + the filtered total ← `agents.list`
  //   - status / planner facets + search re-issue `agents.list`
  //   - saved-view chips ← Console-local (D-061)
  //
  // V1 is INSPECTOR-ONLY for authoring (page-agents.md §10): the Console never
  // creates/edits agents — that is CLI-side (RFC §7.4). The five fleet-control
  // verbs live on the detail route (now LIVE — D-184). Svelte 5 runes (D-092);
  // design tokens only; HarborClient + connection.ts only (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { FilterBar, SavedViewChips, Pagination, PageState } from '$lib/components/ui';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import TopMetricsRollup from '$lib/components/agents/TopMetricsRollup.svelte';
  import CardsGrid from '$lib/components/agents/CardsGrid.svelte';
  import { AgentsListPageState } from '$lib/agents/state.svelte.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const state = new AgentsListPageState();
  const disconnected = $derived(state.disconnected);

  /** Six card indices for the loading skeleton. */
  const SKELETON_CARDS = [0, 1, 2, 3, 4, 5];

  function openAgent(id: string): void {
    void goto(`/agents/${encodeURIComponent(id)}`);
  }

  onMount(() => {
    state.load(injectedClient);
    return () => state.close();
  });
</script>

<svelte:head>
  <title>Agents · Harbor Console</title>
</svelte:head>

<section class="agents-page" data-testid="agents-page">
  <section class="panel card hero-card">
    <TopMetricsRollup metrics={state.metrics} />
  </section>

  <section class="panel card filter-card">
    <FilterBar>
      {#snippet saved()}
        <SavedViewChips
          views={state.savedViews}
          activeId={state.activeSavedId}
          onselect={(id) => state.applySavedView(id)}
          ondelete={(id) => void state.deleteSavedView(id)}
        />
        <input
          class="bar-input save-input"
          type="text"
          placeholder="Save current as…"
          bind:value={state.saveName}
          data-testid="agents-save-view-name"
          disabled={state.savedFilters === null || disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onkeydown={(e) => e.key === 'Enter' && void state.saveCurrentView()}
        />
        <button
          type="button"
          class="bar-action"
          data-testid="agents-save-view"
          disabled={state.savedFilters === null || state.saveName.trim().length === 0 || disconnected}
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : state.savedFilters === null
              ? 'Saved views need a Console profile'
              : 'Save the current filter as a view'}
          onclick={() => void state.saveCurrentView()}
        >
          Save view
        </button>
      {/snippet}

      {#snippet facets()}
        <label class="facet">
          <span>Status</span>
          <select
            value={state.statusFacet}
            data-testid="agents-status-facet"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => state.applyStatusFacet(e.currentTarget.value)}
          >
            <option value="">All</option>
            <option value="active">active</option>
            <option value="paused">paused</option>
            <option value="drained">drained</option>
            <option value="force_stopped">force_stopped</option>
          </select>
        </label>
        <label class="facet">
          <span>Planner</span>
          <select
            value={state.plannerFacet}
            data-testid="agents-planner-facet"
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onchange={(e) => state.applyPlannerFacet(e.currentTarget.value)}
          >
            <option value="">All</option>
            <option value="react">react</option>
            <option value="deterministic">deterministic</option>
          </select>
        </label>
      {/snippet}

      {#snippet search()}
        <input
          class="bar-input search-input"
          type="search"
          placeholder="Search agents by name…"
          value={state.searchText}
          data-testid="agents-search"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onchange={(e) => {
            state.setSearch(e.currentTarget.value);
            state.submitSearch();
          }}
        />
      {/snippet}

      {#snippet actions()}
        <button
          type="button"
          class="bar-action"
          data-testid="agents-clear-filters"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.clearFilters()}
        >
          Clear
        </button>
        <button
          type="button"
          class="bar-action"
          data-testid="agents-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.refresh()}
        >
          Refresh
        </button>
      {/snippet}
    </FilterBar>
  </section>

  <section class="panel card grid-card">
    <PageState status={state.displayStatus} error={state.pageError} onretry={() => state.refresh()}>
      {#snippet skeleton()}
        <div class="cards-skeleton" aria-hidden="true">
          {#each SKELETON_CARDS as i (i)}
            <span class="skeleton-card"></span>
          {/each}
        </div>
      {/snippet}
      {#snippet empty()}
        <!-- The Agents catalog now surfaces the runtime's synthetic default
             agent as a first-class row marked `is_default` (a "Default" chip
             on its card), so a runtime actively serving through its boot agent
             is never an empty page. This empty state is reached only when a
             FILTER excludes every row (including the default one). -->
        <div class="empty-block" data-testid="agents-catalog-empty">
          <p class="empty-headline">No agents match these filters</p>
          <p class="empty-detail">
            No agents match the current view. The runtime's synthetic
            <strong>default agent</strong> — the boot-configured agent it serves
            through, shown with a <strong>Default</strong> marker — is a
            first-class row here; a filter is currently excluding it. To register
            a named agent, scaffold one with <code>harbor scaffold</code> and run
            it with <code>harbor dev</code>.
          </p>
          <button type="button" class="bar-action" onclick={() => state.clearFilters()}>
            Clear filters
          </button>
        </div>
      {/snippet}

      <div class="cards-scroll">
        <CardsGrid agents={state.agents} onopen={openAgent} />
      </div>
    </PageState>

    {#if state.displayStatus === 'ready' || state.displayStatus === 'empty'}
      <Pagination
        page={state.page}
        pageSize={state.pageSize}
        total={state.totalRows}
        onpage={(p) => state.changePage(p)}
        onpagesize={(s) => state.changePageSize(s)}
      />
    {/if}
  </section>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the cards canvas scrolls internally (PAGE-POLISH
     §6 — the Events / Background-Jobs / Tools pattern). */
  .agents-page {
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

  .hero-card {
    flex-shrink: 0;
  }

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

  /* The cards canvas fills the remaining height + scrolls internally. */
  .grid-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  .grid-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .cards-scroll {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    scrollbar-gutter: stable;
  }

  .cards-skeleton {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(var(--size-card-min), 1fr));
    gap: var(--space-3);
  }

  .skeleton-card {
    height: var(--size-sparkline-height);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
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

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
