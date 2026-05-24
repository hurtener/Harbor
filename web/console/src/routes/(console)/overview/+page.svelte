<script lang="ts">
  // Harbor Console — Overview page (`/overview`), built on the D-121
  // design-system foundation (CONVENTIONS.md; Phase 73a / D-127).
  //
  // The operator's at-a-glance hub — the default route on a fresh
  // attach. It is COMPOSITION over Stage-1 primitives, not a new
  // catalog surface (page-overview.md §1, brief 11 §"Overview view"):
  //   - `runtime.counters` (Phase 72f) — the 4-card counter row;
  //   - `runtime.health` (Phase 72f) — the sub-header health-chip strip;
  //   - `pause.list` (Phase 72e) — the intervention queue;
  //   - `events.subscribe` (Phase 60/72) — the recent-activity feed +
  //     counter sparklines + cost rollup, all folded client-side;
  //   - `approve` / `reject` (Phase 54 — SHIPPED) — the intervention
  //     queue's Approve / Reject row actions (NO parallel impl — §13).
  //
  // Phase 73a ships NO new Protocol method. Every Runtime read routes
  // through the unified `HarborClient` + `connection.ts` (CONVENTIONS.md
  // §6) — no hand-rolled `fetch`, no page-local client, no direct
  // `localStorage`. The page clears the §5 depth bar: PageHeader +
  // FilterBar (SavedViewChips + facets + search) + primary DataTable
  // (the intervention queue) + DetailRail + real Pagination +
  // ConnectionFooter + the four-state PageState (with nested PageState
  // per panel). Svelte 5 runes (D-092); design tokens only.
  import { onMount, onDestroy } from 'svelte';
  import PageHeader from '$lib/components/ui/PageHeader.svelte';
  import FilterBar from '$lib/components/ui/FilterBar.svelte';
  import SavedViewChips, { type SavedView } from '$lib/components/ui/SavedViewChips.svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  // ConnectionFooter is rendered ONCE by the app shell ((console)/+layout.svelte —
  // CONVENTIONS.md §2). Per-page imports were duplicating the footer at the
  // bottom of every page (post-83k walkthrough N2); they are removed.
  import CounterCard from '$lib/components/overview/CounterCard.svelte';
  import HealthChipStrip from '$lib/components/overview/HealthChipStrip.svelte';
  import CostRollupCard from '$lib/components/overview/CostRollupCard.svelte';
  import InterventionQueue, {
    type RowActionResult
  } from '$lib/components/overview/InterventionQueue.svelte';
  import RecentActivityFeed from '$lib/components/overview/RecentActivityFeed.svelte';
  import QuickLinksGrid from '$lib/components/overview/QuickLinksGrid.svelte';
  import NewMenu from '$lib/components/overview/NewMenu.svelte';
  import OverviewFooter from '$lib/components/overview/Footer.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { EventsSubscription } from '$lib/events/subscription.svelte.js';
  import type { RuntimeCounters, RuntimeHealth } from '$lib/protocol/posture.js';
  import type { PauseListResponse, PauseSnapshot } from '$lib/protocol/pause.js';
  import { DEFAULT_PAUSE_LIST_PAGE_SIZE } from '$lib/protocol/pause.js';
  import {
    resolveConnection,
    hasScope,
    DISCONNECTED_TOOLTIP,
    type RuntimeConnection
  } from '$lib/connection.js';
  import {
    aggregateRate,
    eventsPerMinute,
    COUNTER_WINDOWS,
    type CounterWindow
  } from '$lib/overview/aggregations.js';
  import { projectActivity } from '$lib/overview/activity.js';
  import { projectCost } from '$lib/overview/cost.js';
  import { openListPageDB } from '$lib/db/console_db.js';
  import { operatorIdOf } from '$lib/db/schema.js';
  import {
    OverviewSavedFilters,
    type OverviewViewSpec
  } from '$lib/db/saved_filters_overview.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // Approve / Reject are control-plane verbs (D-066). Without the admin
  // scope the intervention-queue buttons render disabled-with-tooltip;
  // the same claim drives the per-tenant cost-rollup elevation (D-079).
  let canControl = $state(false);
  // The Phase 83r W1/W2/W3 disconnected predicate — drives the
  // Refresh button + Save view + filter chips disabled state. The
  // synthetic-data CostRollupCard reads it directly so it stops
  // rendering `$0.00 · No cost recorded` against a phantom Runtime.
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- counter row (runtime.counters) ----------------------------- */
  let counters = $state<RuntimeCounters | null>(null);

  /* ---- health strip — its own NESTED PageState -------------------- */
  let healthStatus = $state<PageStatus>('loading');
  let health = $state<RuntimeHealth | null>(null);
  let healthError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- intervention queue — its own NESTED PageState -------------- */
  let queueStatus = $state<PageStatus>('loading');
  let queueResp = $state<PauseListResponse | null>(null);
  let queueError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let queuePage = $state(1);
  let queuePageSize = $state(DEFAULT_PAUSE_LIST_PAGE_SIZE);
  let actionPendingToken = $state<string | null>(null);
  let actionResults = $state<Map<string, RowActionResult>>(new Map());

  /* ---- live event stream (recent activity + sparklines + cost) ---- */
  let subscription = $state<EventsSubscription | null>(null);

  /* ---- FilterBar facets ------------------------------------------- */
  let counterWindow = $state<CounterWindow>('5m');
  let activitySearch = $state('');

  /* ---- the rolling reference clock (relative timestamps) ---------- */
  let nowMillis = $state(Date.now());
  let clockTimer: ReturnType<typeof setInterval> | null = null;

  /* ---- saved views (Console-DB-backed, D-061) --------------------- */
  let savedFilters = $state<OverviewSavedFilters | null>(null);
  let savedViews = $state<SavedView[]>([]);
  let savedSpecs = $state<Map<string, OverviewViewSpec>>(new Map());
  let activeSavedId = $state<string | null>(null);
  let saveName = $state('');

  /* ---- footer constants ------------------------------------------- */
  const PROTOCOL_VERSION = 'v1';
  const CONSOLE_VERSION = 'dev';

  /* ================================================================ */
  /* Derived — folded client-side off the events.subscribe cursor      */
  /* ================================================================ */

  let liveEvents = $derived(subscription?.events ?? []);
  let streamState = $derived(subscription?.state ?? 'idle');

  // The Events/min counter card's sparkline — windowed rate aggregation
  // from the SSE cursor (page-overview.md §12 — subscription-derived, no
  // new Protocol method).
  let rateSeries = $derived(aggregateRate(liveEvents, counterWindow, nowMillis));
  let eventsPerMin = $derived(eventsPerMinute(rateSeries));

  // The recent-activity feed — projected + free-text filtered.
  let activityRows = $derived(
    projectActivity(liveEvents).filter((row) => {
      if (activitySearch.trim() === '') {
        return true;
      }
      const q = activitySearch.trim().toLowerCase();
      return (
        row.type.toLowerCase().includes(q) ||
        row.session.toLowerCase().includes(q) ||
        row.description.toLowerCase().includes(q)
      );
    })
  );

  // The cost rollup — per-agent by default, per-tenant on admin.
  let costRollup = $derived(projectCost(liveEvents, canControl ? 'tenant' : 'agent'));

  // The intervention-queue snapshot page.
  let queueSnapshots = $derived<PauseSnapshot[]>(queueResp?.snapshots ?? []);
  let queueTotal = $derived(queueResp?.total_rows ?? 0);

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */

  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) {
      return { code: err.code, message: err.message };
    }
    return {
      code: 'runtime_error',
      message: err instanceof Error ? err.message : 'unknown error'
    };
  }

  // loadCounters resolves runtime.counters — the 4-card counter row.
  async function loadCounters(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      counters = await client.runtime.counters();
      status = 'ready';
    } catch (err) {
      // The Error state suppresses any stale view — drop last-good data.
      counters = null;
      pageError = toError(err);
      status = 'error';
    }
  }

  // loadHealth resolves runtime.health into the NESTED health-strip
  // PageState — a health-load failure surfaces in the strip, not the
  // whole page (CONVENTIONS.md §4).
  async function loadHealth(): Promise<void> {
    if (client === null) {
      healthStatus = 'disconnected';
      return;
    }
    healthStatus = 'loading';
    healthError = null;
    try {
      const resp = await client.runtime.health();
      health = resp;
      healthStatus = resp.subsystems.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      health = null;
      healthError = toError(err);
      healthStatus = 'error';
    }
  }

  // loadQueue resolves pause.list into the NESTED intervention-queue
  // PageState. Empty filter = the operator's own identity scope,
  // status=paused (the intervention-queue default — D-110).
  async function loadQueue(): Promise<void> {
    if (client === null) {
      queueStatus = 'disconnected';
      return;
    }
    queueStatus = 'loading';
    queueError = null;
    try {
      const resp = await client.pause.list({
        page: queuePage,
        page_size: queuePageSize
      });
      queueResp = resp;
      queueStatus = resp.snapshots.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      queueResp = null;
      queueError = toError(err);
      queueStatus = 'error';
    }
  }

  // The page-level Retry re-invokes every loader (CONVENTIONS.md §4).
  async function reloadAll(): Promise<void> {
    await Promise.all([loadCounters(), loadHealth(), loadQueue()]);
  }

  /* ================================================================ */
  /* Intervention-queue actions — the SHIPPED Phase 54 verbs (§13)     */
  /* ================================================================ */

  async function approveRow(snapshot: PauseSnapshot): Promise<void> {
    await dispatchRowAction(snapshot, 'approve');
  }

  async function rejectRow(snapshot: PauseSnapshot): Promise<void> {
    await dispatchRowAction(snapshot, 'reject');
  }

  // dispatchRowAction invokes the SHIPPED Phase 54 `approve` / `reject`
  // control verbs against the paused run — there is NO parallel pause
  // mutation here (CLAUDE.md §13). The run is keyed by the pause
  // record's run id; the verb is re-checked server-side for scope.
  async function dispatchRowAction(
    snapshot: PauseSnapshot,
    verb: 'approve' | 'reject'
  ): Promise<void> {
    if (client === null || !canControl) {
      return;
    }
    const runID = snapshot.identity.run ?? '';
    actionPendingToken = snapshot.token;
    try {
      if (verb === 'approve') {
        await client.control.approve(runID);
      } else {
        await client.control.reject(runID);
      }
      setActionResult(snapshot.token, { token: snapshot.token, ok: true, message: `${verb}d` });
      // The snapshot is now resolved — refresh the queue page.
      await loadQueue();
    } catch (err) {
      const e = toError(err);
      setActionResult(snapshot.token, {
        token: snapshot.token,
        ok: false,
        message: `${e.code}: ${e.message}`
      });
    } finally {
      actionPendingToken = null;
    }
  }

  function setActionResult(token: string, result: RowActionResult): void {
    const next = new Map(actionResults);
    next.set(token, result);
    actionResults = next;
  }

  /* ================================================================ */
  /* Saved views (Console-DB-backed, D-061)                            */
  /* ================================================================ */

  async function refreshSavedViews(): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    try {
      const records = await savedFilters.list();
      savedViews = records.map((r) => ({ id: r.id, name: r.name }));
      savedSpecs = new Map(records.map((r) => [r.id, r.viewSpec]));
    } catch {
      savedViews = [];
      savedSpecs = new Map();
    }
  }

  function applySavedView(id: string): void {
    const spec = savedSpecs.get(id);
    if (spec === undefined) {
      return;
    }
    counterWindow = spec.window;
    activeSavedId = id;
  }

  async function deleteSavedView(id: string): Promise<void> {
    if (savedFilters === null) {
      return;
    }
    await savedFilters.delete(id);
    if (activeSavedId === id) {
      activeSavedId = null;
    }
    await refreshSavedViews();
  }

  async function saveCurrentView(): Promise<void> {
    const name = saveName.trim();
    if (name.length === 0 || savedFilters === null) {
      return;
    }
    const created = await savedFilters.create(name, {
      window: counterWindow,
      activityTypes: []
    });
    saveName = '';
    await refreshSavedViews();
    activeSavedId = created.id;
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  onMount(() => {
    connection = resolveConnection();
    if (connection === null) {
      client = null;
      status = 'disconnected';
      healthStatus = 'disconnected';
      queueStatus = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');

    // Open the live event stream — the recent-activity / sparkline /
    // cost-rollup feed. Narrowed to the operator-relevant topics so the
    // cursor is not flooded by every bus event. The `events` namespace
    // is read off the resolved (or injected) ProtocolClient — no
    // hand-rolled per-page client (CONVENTIONS.md §6).
    const sub = new EventsSubscription(client.events);
    sub.open({
      eventTypes: [
        'session.opened',
        'session.closed',
        'task.completed',
        'task.failed',
        'task.cancelled',
        'agent.registered',
        'agent.restarted',
        'llm.cost.recorded'
      ]
    });
    subscription = sub;

    // The relative-timestamp clock ticks once a second.
    clockTimer = setInterval(() => {
      nowMillis = Date.now();
    }, 1000);

    void (async () => {
      try {
        const db = await openListPageDB(connection!);
        const operator = await operatorIdOf(
          connection!.identity.tenant,
          connection!.identity.user
        );
        savedFilters = new OverviewSavedFilters(db, operator);
        await refreshSavedViews();
      } catch {
        savedFilters = null;
      }
    })();

    void reloadAll();
  });

  onDestroy(() => {
    subscription?.close();
    if (clockTimer !== null) {
      clearInterval(clockTimer);
    }
  });
</script>

<svelte:head>
  <title>Overview · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="overview-page">
  <PageHeader
    title="Overview"
    subtitle="The operator's at-a-glance hub — counters, interventions, and activity"
  >
    {#snippet actions()}
      <NewMenu />
      <button
        type="button"
        class="control"
        data-testid="overview-refresh"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onclick={() => void reloadAll()}
      >
        Refresh
      </button>
    {/snippet}
  </PageHeader>

  <!-- Sub-header health-chip strip — its own NESTED PageState so a
       health-load failure surfaces here, not on the whole page. -->
  <PageState status={healthStatus} error={healthError} onretry={() => void loadHealth()}>
    {#snippet skeleton()}
      <div class="strip-skeleton" aria-hidden="true"></div>
    {/snippet}
    {#snippet empty()}
      <p class="strip-empty" data-testid="health-chip-strip-empty">
        The Runtime reported no registered subsystems.
      </p>
    {/snippet}
    {#if health !== null}
      <HealthChipStrip subsystems={health.subsystems} />
    {/if}
  </PageState>

  <FilterBar>
    {#snippet saved()}
      <SavedViewChips
        views={savedViews}
        activeId={activeSavedId}
        onselect={applySavedView}
        ondelete={(id) => void deleteSavedView(id)}
      />
      <input
        class="control save-input"
        type="text"
        placeholder="Save current as…"
        bind:value={saveName}
        data-testid="overview-save-name"
        disabled={savedFilters === null || disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
        onkeydown={(e) => e.key === 'Enter' && void saveCurrentView()}
      />
      <button
        type="button"
        class="control"
        data-testid="overview-save-view"
        disabled={savedFilters === null || saveName.trim().length === 0 || disconnected}
        title={disconnected
          ? DISCONNECTED_TOOLTIP
          : savedFilters === null
            ? 'Console-local saved-view store unavailable'
            : undefined}
        onclick={() => void saveCurrentView()}
      >
        Save view
      </button>
    {/snippet}

    {#snippet facets()}
      <div class="window-facet" data-testid="overview-window-facet">
        {#each COUNTER_WINDOWS as w (w)}
          <button
            type="button"
            class="window-chip"
            data-active={counterWindow === w}
            data-testid={`overview-window-${w}`}
            disabled={disconnected}
            title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
            onclick={() => (counterWindow = w)}
          >
            {w}
          </button>
        {/each}
      </div>
    {/snippet}

    {#snippet search()}
      <input
        class="control"
        type="search"
        placeholder="Filter recent activity…"
        bind:value={activitySearch}
        data-testid="overview-activity-search"
        disabled={disconnected}
        title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
      />
    {/snippet}
  </FilterBar>

  <div class="layout">
    <div class="main-col">
      <!-- Row 2 — the 4-card counter row. The page-level PageState
           drives it: the counter row is the page's primary view. -->
      <PageState status={status} error={pageError} onretry={() => void reloadAll()}>
        {#snippet skeleton()}
          <div class="counter-skeleton" aria-hidden="true">
            <span class="card-skeleton"></span>
            <span class="card-skeleton"></span>
            <span class="card-skeleton"></span>
            <span class="card-skeleton"></span>
          </div>
        {/snippet}
        {#snippet empty()}
          <p class="block-empty">No runtime counters available.</p>
        {/snippet}

        {#if counters !== null}
          <div class="counter-row" data-testid="counter-row">
            <!-- N11 (Phase 83x): every counter card on this row reads
                 a CURRENTLY-active gauge (not a window-cumulative
                 count). The legacy labels did not say so — an operator
                 expecting "tasks completed in the last hour" read
                 zero after a successful 13s run and assumed the
                 counter was broken. Suffix each label with "(now)" so
                 the semantics are explicit. The Events/min card is
                 already a per-minute RATE so it keeps its own label. -->
            <CounterCard
              testid="counter-events"
              label="Events/min"
              value={eventsPerMin.toFixed(1)}
              href="/events"
              series={rateSeries}
            />
            <CounterCard
              testid="counter-tasks"
              label="Tasks Running (now)"
              value={String(counters.tasks_running)}
              href="/tasks"
            />
            <CounterCard
              testid="counter-jobs"
              label="Background Jobs (now)"
              value={String(counters.background_jobs_active)}
              href="/background-jobs"
            />
            <CounterCard
              testid="counter-mcp"
              label="MCP Connections (now)"
              value={String(counters.mcp_connections_healthy)}
              href="/mcp-connections"
            />
          </div>
        {/if}
      </PageState>

      <!-- Row 3 — two-column split: intervention queue + cost rollup. -->
      <div class="row-3">
        <section class="queue-panel">
          <h2 class="panel-title">Intervention queue</h2>
          <PageState
            status={queueStatus}
            error={queueError}
            onretry={() => void loadQueue()}
          >
            {#snippet skeleton()}
              <div class="table-skeleton" aria-hidden="true"></div>
            {/snippet}
            {#snippet empty()}
              <div class="block-empty-pad" data-testid="intervention-queue-state-empty">
                <p class="block-empty">No pending interventions in scope.</p>
              </div>
            {/snippet}

            <InterventionQueue
              snapshots={queueSnapshots}
              canControl={canControl}
              pendingToken={actionPendingToken}
              results={actionResults}
              onapprove={(s) => void approveRow(s)}
              onreject={(s) => void rejectRow(s)}
            />
          </PageState>
          {#if queueStatus === 'ready' || queueStatus === 'empty'}
            <Pagination
              page={queuePage}
              pageSize={queuePageSize}
              total={queueTotal}
              onpage={(p) => {
                queuePage = p;
                void loadQueue();
              }}
              onpagesize={(s) => {
                queuePageSize = s;
                queuePage = 1;
                void loadQueue();
              }}
            />
          {/if}
        </section>

        <section class="cost-panel">
          <h2 class="panel-title">Cost rollup</h2>
          <CostRollupCard rollup={costRollup} canElevate={canControl} {disconnected} />
        </section>
      </div>

      <!-- Row 4 — full-width recent-activity feed. -->
      <section class="activity-panel">
        <h2 class="panel-title">Recent activity</h2>
        <RecentActivityFeed rows={activityRows} now={nowMillis} />
      </section>

      <!-- Row 5 — the 2×3 Quick Links grid. -->
      <section class="quick-links-panel">
        <h2 class="panel-title">Quick Links</h2>
        <QuickLinksGrid />
      </section>

      <OverviewFooter
        runtimeName={connection?.baseURL ?? 'no runtime'}
        protocolVersion={PROTOCOL_VERSION}
        streamState={streamState}
        consoleVersion={CONSOLE_VERSION}
      />
    </div>

    <DetailRail>
      <RailCard title="Runtime">
        {#if connection !== null}
          <dl class="rail-dl">
            <dt>Runtime</dt>
            <dd class="mono">{connection.baseURL}</dd>
            <dt>Tenant</dt>
            <dd class="mono">{connection.identity.tenant}</dd>
            <dt>Session</dt>
            <dd class="mono">{connection.identity.session}</dd>
            <dt>Scope</dt>
            <dd>{canControl ? 'admin (elevated)' : 'operator'}</dd>
          </dl>
        {:else}
          <p class="rail-note">Not connected to a Runtime.</p>
        {/if}
      </RailCard>
      <RailCard title="Counters">
        {#if counters !== null}
          <dl class="rail-dl">
            <dt>Sessions active</dt>
            <dd>{counters.sessions_active}</dd>
            <dt>Tasks running</dt>
            <dd>{counters.tasks_running}</dd>
            <dt>Background jobs</dt>
            <dd>{counters.background_jobs_active}</dd>
            <dt>MCP connections</dt>
            <dd>{counters.mcp_connections_healthy}</dd>
          </dl>
        {:else}
          <p class="rail-note">Counters unavailable.</p>
        {/if}
      </RailCard>
      <RailCard title="Pending interventions">
        <p class="rail-note" data-testid="rail-intervention-count">
          {queueTotal} run{queueTotal === 1 ? '' : 's'} awaiting a decision.
        </p>
      </RailCard>
    </DetailRail>
  </div>
</div>

<style>
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
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
    gap: var(--space-4);
    min-width: var(--space-0);
  }

  .counter-row {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-3);
  }

  .row-3 {
    display: grid;
    grid-template-columns: 3fr 2fr;
    gap: var(--space-4);
    align-items: start;
  }

  .queue-panel,
  .cost-panel,
  .activity-panel,
  .quick-links-panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .panel-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .control {
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .save-input {
    width: var(--size-search-min);
  }

  .window-facet {
    display: inline-flex;
    gap: var(--space-1);
  }

  .window-chip {
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    cursor: pointer;
  }

  .window-chip[data-active='true'] {
    color: var(--color-accent);
    border-color: var(--color-accent);
  }

  .counter-skeleton {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: var(--space-3);
  }

  .card-skeleton {
    height: var(--space-12);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
    animation: pulse var(--motion-slow) ease-in-out infinite alternate;
  }

  .strip-skeleton {
    height: var(--space-6);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .table-skeleton {
    height: var(--space-12);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }

  .strip-empty,
  .block-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .block-empty-pad {
    padding: var(--space-6) var(--space-4);
    text-align: center;
  }

  .rail-dl {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
    font-size: var(--text-sm);
  }

  .rail-dl dt {
    color: var(--color-text-muted);
  }

  .rail-dl dd {
    margin: var(--space-0);
    text-align: right;
  }

  .rail-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    overflow-wrap: anywhere;
  }

  @keyframes pulse {
    from {
      opacity: 0.4;
    }
    to {
      opacity: 0.8;
    }
  }
</style>
