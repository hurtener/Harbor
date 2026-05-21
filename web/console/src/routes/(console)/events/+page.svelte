<script lang="ts">
  // Console Events page — the runtime event-bus stream as a full-screen,
  // query-driven investigative surface (Phase 73g / D-125).
  //
  // This page is a PURE UI consumer of already-shipped Protocol surface
  // (CLAUDE.md §13 primitive-with-consumer rule, satisfied trivially —
  // 73g IS the consumer Phase 72a's primitives wait for):
  //
  //   - `events.subscribe` (Phase 72, GET /v1/events SSE) — the table feed.
  //   - `events.aggregate` (Phase 72a) — the event-rate sparkline.
  //   - `artifacts.get_ref` (Phase 73l) — heavy-payload `Open artifact`.
  //
  // This phase ships NO new Protocol method. Every Runtime read routes
  // through the unified `HarborClient` via the `EventsPageState`
  // controller — no hand-rolled `fetch`, no direct `localStorage`
  // (CONVENTIONS.md §6). The page clears the §5 depth bar: PageHeader +
  // FilterBar (SavedViewChips + facets + search + export) + sparkline +
  // DataTable + DetailRail + Pagination + ConnectionFooter + the
  // four-state PageState. Svelte 5 runes (D-092); design tokens only.
  import { onMount } from 'svelte';
  import {
    PageHeader,
    FilterBar,
    SavedViewChips,
    DataTable,
    DetailRail,
    Pagination,
    ConnectionFooter,
    PageState,
    StatusChip,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  import EventFilterChips from '$lib/components/events/EventFilterChips.svelte';
  import EventRateSparkline from '$lib/components/events/EventRateSparkline.svelte';
  import EventDetailRail from '$lib/components/events/EventDetailRail.svelte';
  import PauseStreamToggle from '$lib/components/events/PauseStreamToggle.svelte';
  import ExportMenu from '$lib/components/events/ExportMenu.svelte';
  import { EventsPageState } from '$lib/events/state.svelte.js';
  import { EventsSavedViews } from '$lib/events/saved-views.svelte.js';
  import { categoryKind, categoryOf } from '$lib/events/taxonomy.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { EventFacetState } from '$lib/events/filters.js';

  const state = new EventsPageState();
  const savedViews = new EventsSavedViews();

  /** The event table columns, in page-events.md §12 mockup order. */
  const COLUMNS: DataTableColumn[] = [
    { key: 'time', label: 'Time' },
    { key: 'event', label: 'Event' },
    { key: 'identity', label: 'Identity' },
    { key: 'source', label: 'Source' },
    { key: 'span', label: 'Span' }
  ];

  onMount(() => {
    void state.load();
    void savedViews.load();
    return () => state.close();
  });

  function applySavedView(id: string): void {
    const spec = savedViews.filterFor(id);
    if (spec !== null) {
      state.applySavedView(id, spec);
    }
  }

  async function deleteSavedView(id: string): Promise<void> {
    await savedViews.remove(id);
    if (state.activeSavedViewId === id) {
      state.activeSavedViewId = null;
    }
  }

  async function saveCurrentView(): Promise<void> {
    const name = (
      typeof window !== 'undefined' ? window.prompt('Name this saved view') : null
    )?.trim();
    if (name) {
      await savedViews.create(name, state.facets);
    }
  }

  /** Pin a facet from a Quick Action / sparkline click (Console-local). */
  function pinFacet(
    axis: 'type' | 'tenant' | 'user' | 'session' | 'run',
    value: string
  ): void {
    if (axis === 'type') {
      state.toggleType(value);
      return;
    }
    const next: EventFacetState = { ...state.facets, [axis]: value };
    state.applyFacets(next);
  }

  /** Toggle the pause-stream view gate (Console-local — no Protocol call). */
  function togglePause(): void {
    const sub = state.subscription;
    if (sub === null) {
      return;
    }
    if (sub.streamPaused) {
      sub.resume();
    } else {
      sub.pause();
    }
  }

  /** Compresses the identity triple for the table cell. */
  function identityCell(e: Event): string {
    return `${e.tenant}/${e.user}/${e.session}`;
  }

  /** The last-8 of a span/trace id when present in the event sidecar. */
  function spanCell(e: Event): string {
    const span = e.extra?.span_id ?? e.extra?.trace_id ?? '';
    return span === '' ? '—' : span.slice(-8);
  }
</script>

<svelte:head>
  <title>Events · Harbor Console</title>
</svelte:head>

<section class="events-page" data-testid="events-page">
  <PageHeader
    title="Events"
    subtitle="The runtime event-bus stream as a full-screen, query-driven investigative surface."
  />

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews.views}
        activeId={state.activeSavedViewId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
    {/snippet}
    {#snippet facets()}
      <EventFilterChips
        facets={state.facets}
        isAdmin={state.isAdmin}
        ownTenant={state.ownTenant}
        onapply={(next) => state.applyFacets(next)}
      />
    {/snippet}
    {#snippet search()}
      <input
        type="search"
        class="search-input"
        placeholder="Search events…"
        data-testid="events-search"
        title="Searching the loaded page only — runtime-side search lands with Phase 72c"
        value={state.search}
        oninput={(e) => state.setSearch((e.currentTarget as HTMLInputElement).value)}
      />
    {/snippet}
    {#snippet actions()}
      <PauseStreamToggle
        paused={state.subscription?.streamPaused ?? false}
        ontoggle={togglePause}
      />
      <ExportMenu events={state.pagedEvents} />
      <button
        type="button"
        class="bar-action"
        data-testid="save-view"
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}
  </FilterBar>

  {#if state.crossTenant}
    <p class="admin-notice" data-testid="cross-tenant-notice">
      Cross-tenant fan-in active — this subscription emits an
      <code>audit.admin_scope_used</code> event the table below surfaces.
    </p>
  {/if}

  {#if (state.subscription?.droppedCount ?? 0) > 0}
    <p class="dropped-strip" data-testid="bus-dropped-strip">
      {state.subscription?.droppedCount} bus-drop event(s) in this window — some
      events were dropped by the subscription buffer.
    </p>
  {/if}

  <div class="layout">
    <div class="main-col">
      <PageState
        status={state.status}
        error={state.error}
        onretry={() => void state.load()}
      >
        {#snippet empty()}
          <p class="empty-headline" data-testid="events-empty">
            No events match these filters in this window — clear filters or
            widen the time range.
          </p>
        {/snippet}

        {#if state.aggregator}
          <EventRateSparkline
            series={state.aggregator.series}
            onpin={(type) => state.toggleType(type)}
          />
        {/if}

        <DataTable
          columns={COLUMNS}
          rows={state.pagedEvents}
          rowKey={(r) => String((r as Event).sequence)}
          onrowclick={(r) => state.selectEvent(r as Event)}
        >
          {#snippet row(r)}
            {@const ev = r as Event}
            <td class="mono" data-testid={`event-row-${ev.sequence}`}>{ev.occurred_at}</td>
            <td>
              <span class="event-cell">
                <StatusChip
                  kind={categoryKind(categoryOf(ev.type))}
                  label={categoryOf(ev.type)}
                />
                <span class="event-name mono">{ev.type}</span>
              </span>
            </td>
            <td class="mono identity-cell">{identityCell(ev)}</td>
            <td class="mono">{ev.extra?.source ?? categoryOf(ev.type)}</td>
            <td class="mono">{spanCell(ev)}</td>
          {/snippet}
          {#snippet empty()}
            <span>No events match the current filter.</span>
          {/snippet}
        </DataTable>
      </PageState>

      {#if state.status === 'ready' || state.status === 'empty'}
        <Pagination
          page={state.page}
          pageSize={state.pageSize}
          total={state.total}
          pageSizeOptions={[50, 100, 250]}
          onpage={(p) => state.goToPage(p)}
          onpagesize={(s) => state.setPageSize(s)}
        />
      {/if}
    </div>

    <DetailRail>
      {#if state.artifacts}
        <EventDetailRail
          event={state.selected}
          artifacts={state.artifacts}
          onpin={pinFacet}
        />
      {/if}
    </DetailRail>
  </div>

  <ConnectionFooter />
</section>

<style>
  .events-page {
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

  .admin-notice,
  .dropped-strip {
    margin: var(--space-0);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
  }

  .admin-notice {
    color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .dropped-strip {
    color: var(--color-danger);
    background: var(--color-danger-soft);
  }

  .event-cell {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }

  .event-name {
    font-size: var(--text-xs);
  }

  .identity-cell {
    color: var(--color-text-muted);
  }

  td.mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
