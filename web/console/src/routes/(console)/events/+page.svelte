<script lang="ts">
  // Console Events page — the runtime event-bus stream as a full-screen,
  // query-driven investigative surface. Phase 108h (D-180) rethemes it to
  // the carded, viewport-locked composition; the data layer (Phase 73g /
  // D-125) is unchanged.
  //
  // PURE UI consumer of already-shipped Protocol surface (no new method):
  //   - `events.subscribe` (Phase 72, GET /v1/events SSE) — the LIVE table feed.
  //   - `events.aggregate` (Phase 72a) — the event-rate sparkline.
  //   - `artifacts.get_ref` (Phase 73l) — heavy-payload `Open artifact`.
  //
  // Every Runtime read routes through the unified `HarborClient` via the
  // `EventsPageState` controller — no hand-rolled `fetch`, no direct
  // `localStorage` (CONVENTIONS.md §6).
  //
  // Phase 108h layout: carded `.panel.card` regions (filter strip + rate
  // sparkline + events table), viewport-locked (PAGE-POLISH §6 — the
  // Playground / Sessions pattern): the filter strip + sparkline are
  // fixed-height; the events table scrolls internally behind a sticky
  // header; the right rail scrolls internally; the document never
  // full-page-scrolls. The idle right rail (no row selected) shows the
  // live subscription status. Svelte 5 runes (D-092); design tokens only.
  import { onMount, onDestroy } from 'svelte';
  import {
    FilterBar,
    SavedViewChips,
    DataTable,
    Pagination,
    PageState,
    StatusChip,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import EventFilterChips from '$lib/components/events/EventFilterChips.svelte';
  import EventRateSparkline from '$lib/components/events/EventRateSparkline.svelte';
  import EventDetailRail from '$lib/components/events/EventDetailRail.svelte';
  import PauseStreamToggle from '$lib/components/events/PauseStreamToggle.svelte';
  import ExportMenu from '$lib/components/events/ExportMenu.svelte';
  import { EventsPageState } from '$lib/events/state.svelte.js';
  import { EventsSavedViews } from '$lib/events/saved-views.svelte.js';
  import { categoryKind, categoryOf } from '$lib/events/taxonomy.js';
  import { WINDOW_SPEC } from '$lib/protocol/events.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { EventFacetState } from '$lib/events/filters.js';

  const state = new EventsPageState();
  const savedViews = new EventsSavedViews();
  const disconnected = $derived(state.status === 'disconnected');

  /** The event table columns, in page-events.md §12 mockup order. */
  const COLUMNS: DataTableColumn[] = [
    { key: 'time', label: 'Time' },
    { key: 'event', label: 'Event' },
    { key: 'identity', label: 'Identity' },
    { key: 'source', label: 'Source' },
    { key: 'span', label: 'Span' }
  ];

  /** The active window span in seconds — drives the rate chart's per-second rate. */
  const windowSeconds = $derived(WINDOW_SPEC[state.facets.window].windowNs / 1_000_000_000);

  onMount(() => {
    void state.load();
    void savedViews.load();
    return () => state.close();
  });

  // Keep the rate sparkline live: the `events.aggregate` series is a
  // point-in-time fetch, so re-fetch it (throttled to ~1.5s) as the live
  // subscription cursor advances — the sparkline then tracks the same
  // stream that feeds the table (page-events.md §12) instead of showing
  // a single stale snapshot taken before any events fired.
  let sparkTimer: ReturnType<typeof setTimeout> | null = null;
  $effect(() => {
    // Track the cursor so this effect re-runs as events arrive.
    const cursor = state.subscription?.cursor ?? 0;
    if (cursor === 0 || state.aggregator === null || sparkTimer !== null) {
      return;
    }
    sparkTimer = setTimeout(() => {
      sparkTimer = null;
      void state.aggregator?.refresh();
    }, 1500);
  });
  onDestroy(() => {
    if (sparkTimer !== null) {
      clearTimeout(sparkTimer);
    }
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

  /** A friendly stream-state label for the idle subscription-status card. */
  const streamLabel = $derived(
    state.subscription?.streamPaused
      ? 'paused'
      : (state.subscription?.state ?? 'idle')
  );
</script>

<svelte:head>
  <title>Events · Harbor Console</title>
</svelte:head>

<section class="events-page" data-testid="events-page">
  <section class="panel card filter-card">
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
          title={disconnected
            ? DISCONNECTED_TOOLTIP
            : 'Searching the loaded page only — runtime-side search lands with Phase 72c'}
          value={state.search}
          disabled={disconnected}
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
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
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
  </section>

  <div class="layout">
    <div class="main-col">
      {#if state.aggregator}
        <section class="panel card sparkline-card">
          <h2 class="panel-title">Event rate over time (per second)</h2>
          <EventRateSparkline
            series={state.aggregator.series}
            {windowSeconds}
            onpin={(type) => state.toggleType(type)}
          />
        </section>
      {/if}

      <section class="panel card table-card">
        <PageState
          status={state.displayStatus}
          error={state.error}
          onretry={() => void state.load()}
        >
          {#snippet empty()}
            <!-- Phase 108h: the table feed is the LIVE `events.subscribe`
                 SSE — it streams events going forward, with no historical
                 backfill on an in-memory event driver. A quiet window for
                 the caller's own session is empty until events fire; widen
                 the scope (pin a session / tenant), generate activity, or
                 run a `durable` event driver for read-back. -->
            <div class="empty-block" data-testid="events-empty">
              <p class="empty-headline">No events in this window</p>
              <p class="empty-detail">
                The table streams live from <code>events.subscribe</code>,
                scoped to the active filter (the caller's own session by
                default). A quiet window shows no rows until events fire —
                widen the scope (pin a session or tenant), generate activity,
                or set <code>events.driver: durable</code> in
                <code>harbor.yaml</code> for historical read-back. Otherwise
                clear filters or widen the time range.
              </p>
            </div>
          {/snippet}

          <div class="table-scroll">
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
          </div>
        </PageState>

        {#if state.displayStatus === 'ready' || state.displayStatus === 'empty'}
          <Pagination
            page={state.page}
            pageSize={state.pageSize}
            total={state.total}
            pageSizeOptions={[50, 100, 250]}
            onpage={(p) => state.goToPage(p)}
            onpagesize={(s) => state.setPageSize(s)}
          />
        {/if}
      </section>
    </div>

    <!-- Right column — ONE packed card (Phase 108h): the Event Details
         when a row is selected, else the live subscription status. The
         card fills the column and scrolls INTERNALLY, so the detail never
         drives a page-level scroll. -->
    {#if state.selected && state.artifacts}
      <EventDetailRail
        event={state.selected}
        artifacts={state.artifacts}
        onpin={pinFacet}
        onclose={() => (state.selected = null)}
      />
    {:else}
      <section class="panel card status-card">
        <h2 class="panel-title">Subscription</h2>
        <dl class="status-grid" data-testid="subscription-status">
          <dt>Stream</dt>
          <dd><StatusChip kind={streamLabel === 'open' ? 'success' : streamLabel === 'paused' ? 'warning' : 'neutral'} label={streamLabel} /></dd>
          <dt>Cursor</dt>
          <dd class="mono">{state.subscription?.cursor ?? 0}</dd>
          <dt>Dropped</dt>
          <dd class="mono">{state.subscription?.droppedCount ?? 0}</dd>
          <dt>Loaded</dt>
          <dd class="mono">{state.subscription?.events.length ?? 0}</dd>
        </dl>
        <p class="status-hint">Click a row to inspect its typed payload + identity.</p>
      </section>
    {/if}
  </div>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls; only the events table + the right rail scroll
     internally (PAGE-POLISH §6 — the Playground / Sessions pattern). */
  .events-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  /* The carded surface — same vocabulary as the Overview / Sessions pages.
     Tighter padding than the default card to pack the dense Events page
     into one viewport without a page scroll. */
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

  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  /* Kill the shared FilterBar's own vertical padding inside the Events
     filter card so the strip packs tight (Phase 108h — the card already
     supplies the padding; the doubled space made the filter sprawl). */
  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .sparkline-card {
    flex-shrink: 0;
  }

  /* The layout fills the remaining height; both columns scroll internally. */
  .layout {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  /* The right-column status card (idle) fills + scrolls like the detail. */
  .status-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-height: 0;
    overflow-y: auto;
  }

  .main-col {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: var(--space-0);
    min-height: 0;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  /* The events table scrolls inside this region behind a sticky header. */
  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .search-input {
    width: 100%;
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
  }

  .bar-action {
    background: var(--color-bg);
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

  .empty-block {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4) var(--space-0);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    font-weight: 600;
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .empty-detail code {
    font-family: var(--font-mono);
    color: var(--color-text);
  }

  .status-grid {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    align-items: center;
  }

  .status-grid dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .status-grid dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
    text-align: right;
  }

  .status-hint {
    margin: var(--space-2) var(--space-0) var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
