<script lang="ts">
  // Harbor Console — Overview page (`/overview`) — Phase 108c rebuild.
  //
  // The operator's at-a-glance hub, rebuilt to verbatim parity with
  // `docs/rfc/assets/console-overview-page.png`. It is COMPOSITION over the
  // shipped data layer (page-overview.md §1; D-127) — NO new Protocol method:
  //   - `runtime.counters` (72f) — the 4 KPI cards (sampled into a client-side
  //     ring buffer for real trend sparklines + deltas);
  //   - `runtime.health` (72f) — the context-row health pill;
  //   - `pause.list` (72e) + `approve`/`reject` (54) — the intervention queue;
  //   - `events.subscribe` (60/72) — recent activity + the events/min rate
  //     sparkline + the cost rollup + the alerts strip + the audit ribbon,
  //     all folded client-side.
  //
  // Canvas rows (mock): context+audit → alerts → 4 KPI cards → interventions |
  // cost(by-model) → recent activity → quick links. NO right detail-rail, NO
  // page-level top filter bar, NO per-page search/+New (those are app-shell
  // chrome — 108b). Every datum is real-wired; deferred items (notifications
  // bell, by-agent cost, personal layouts §10) are absent, never faked.
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient + connection.ts
  // only (CONVENTIONS.md §6).
  import { onMount, onDestroy } from 'svelte';
  import Activity from '@lucide/svelte/icons/activity';
  import ListChecks from '@lucide/svelte/icons/list-checks';
  import Layers from '@lucide/svelte/icons/layers';
  import Plug from '@lucide/svelte/icons/plug';
  import CircleCheck from '@lucide/svelte/icons/circle-check';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import CounterCard from '$lib/components/overview/CounterCard.svelte';
  import ContextAuditRow from '$lib/components/overview/ContextAuditRow.svelte';
  import AlertsStrip from '$lib/components/overview/AlertsStrip.svelte';
  import CostRollupCard from '$lib/components/overview/CostRollupCard.svelte';
  import InterventionQueue, {
    type RowActionResult
  } from '$lib/components/overview/InterventionQueue.svelte';
  import RecentActivityFeed from '$lib/components/overview/RecentActivityFeed.svelte';
  import QuickLinksGrid from '$lib/components/overview/QuickLinksGrid.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { EventsSubscription } from '$lib/events/subscription.svelte.js';
  import type { RuntimeCounters, RuntimeHealth } from '$lib/protocol/posture.js';
  import type { PauseListResponse, PauseSnapshot } from '$lib/protocol/pause.js';
  import { DEFAULT_PAUSE_LIST_PAGE_SIZE } from '$lib/protocol/pause.js';
  import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';
  import { aggregateRate, eventsPerMinute } from '$lib/overview/aggregations.js';
  import { projectActivity } from '$lib/overview/activity.js';
  import { projectCost, type CostBreakdown } from '$lib/overview/cost.js';
  import { projectAlerts, auditScopeCount, ALERT_TYPES, AUDIT_TYPE } from '$lib/overview/alerts.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();
  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  let canControl = $state(false);
  let disconnected = $derived(connection === null);

  /* ---- page-level state (counter row is the primary view) --------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let counters = $state<RuntimeCounters | null>(null);

  /* ---- counter trend history (sampled gauges → real sparklines) --- */
  // runtime.counters is a point-in-time snapshot; we sample it on an interval
  // into per-metric ring buffers so the sparklines + deltas are REAL sampled
  // data (trend since the page opened), never fabricated (procedure §1).
  const HISTORY_CAP = 16;
  let hist = $state<{ tasks: number[]; jobs: number[]; mcp: number[] }>({
    tasks: [],
    jobs: [],
    mcp: []
  });
  let sampleTimer: ReturnType<typeof setInterval> | null = null;

  /* ---- health (context row) --------------------------------------- */
  let health = $state<RuntimeHealth | null>(null);

  /* ---- intervention queue — nested PageState ---------------------- */
  let queueStatus = $state<PageStatus>('loading');
  let queueResp = $state<PauseListResponse | null>(null);
  let queueError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let queuePage = $state(1);
  let queuePageSize = $state(DEFAULT_PAUSE_LIST_PAGE_SIZE);
  let actionPendingToken = $state<string | null>(null);
  let actionResults = $state<Map<string, RowActionResult>>(new Map());

  /* ---- live event stream + rolling clock -------------------------- */
  let subscription = $state<EventsSubscription | null>(null);
  let nowMillis = $state(Date.now());
  let clockTimer: ReturnType<typeof setInterval> | null = null;

  /* ================================================================ */
  /* Derived — folded client-side off the events.subscribe cursor      */
  /* ================================================================ */
  let liveEvents = $derived(subscription?.events ?? []);
  let rateSeries = $derived(aggregateRate(liveEvents, '15m', nowMillis));
  let eventsPerMin = $derived(eventsPerMinute(rateSeries));
  let eventsValues = $derived(rateSeries.buckets.map((b) => b.count));
  let activityRows = $derived(projectActivity(liveEvents));
  // Cost by MODEL (default) or by runtime/AGENT ("each runtime is an agent").
  // `runtimeLabel` is the agent-registry name when available, else the runtime
  // host (set in onMount). See cost.ts for the no-per-agent-attribution finding.
  let costBreakdown = $state<CostBreakdown>('model');
  let runtimeLabel = $state('This runtime');
  let costRollup = $derived(projectCost(liveEvents, costBreakdown, runtimeLabel));
  let alerts = $derived(projectAlerts(liveEvents, nowMillis));
  let auditCount = $derived(auditScopeCount(liveEvents, nowMillis));
  let queueSnapshots = $derived<PauseSnapshot[]>(queueResp?.snapshots ?? []);

  /** A real delta from a numeric series — null unless ≥2 samples and a positive base. */
  function deltaOf(values: number[]): { pct: number; dir: 'up' | 'down' } | null {
    if (values.length < 2) return null;
    const first = values[0];
    const last = values[values.length - 1];
    if (first <= 0) return null;
    const pct = ((last - first) / first) * 100;
    return { pct, dir: last >= first ? 'up' : 'down' };
  }
  let eventsDelta = $derived(deltaOf(eventsValues));
  let tasksDelta = $derived(deltaOf(hist.tasks));
  let jobsDelta = $derived(deltaOf(hist.jobs));
  let mcpDelta = $derived(deltaOf(hist.mcp));

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */
  function toError(err: unknown): { code: string; message: string } {
    if (err instanceof ProtocolError) return { code: err.code, message: err.message };
    return { code: 'runtime_error', message: err instanceof Error ? err.message : 'unknown error' };
  }

  function pushSample(c: RuntimeCounters): void {
    const cap = (arr: number[], v: number) => [...arr, v].slice(-HISTORY_CAP);
    hist = {
      tasks: cap(hist.tasks, c.tasks_running),
      jobs: cap(hist.jobs, c.background_jobs_active),
      mcp: cap(hist.mcp, c.mcp_connections_healthy)
    };
  }

  async function loadCounters(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      counters = await client.runtime.counters();
      pushSample(counters);
      status = 'ready';
    } catch (err) {
      counters = null;
      pageError = toError(err);
      status = 'error';
    }
  }

  // A lightweight sampler that refreshes the live counter values + appends to
  // the trend ring buffers WITHOUT touching page status (so a transient sample
  // failure never flips the whole page to Error).
  async function sampleCounters(): Promise<void> {
    if (client === null) return;
    try {
      const c = await client.runtime.counters();
      counters = c;
      pushSample(c);
    } catch {
      /* keep last-good; the next tick retries */
    }
  }

  async function loadHealth(): Promise<void> {
    if (client === null) return;
    try {
      health = await client.runtime.health();
    } catch {
      health = null;
    }
  }

  async function loadQueue(): Promise<void> {
    if (client === null) {
      queueStatus = 'disconnected';
      return;
    }
    queueStatus = 'loading';
    queueError = null;
    try {
      const resp = await client.pause.list({ page: queuePage, page_size: queuePageSize });
      queueResp = resp;
      queueStatus = resp.snapshots.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      queueResp = null;
      queueError = toError(err);
      queueStatus = 'error';
    }
  }

  async function reloadAll(): Promise<void> {
    await Promise.all([loadCounters(), loadHealth(), loadQueue()]);
  }

  /* ---- intervention-queue actions — the SHIPPED Phase 54 verbs ----- */
  async function dispatchRowAction(snapshot: PauseSnapshot, verb: 'approve' | 'reject'): Promise<void> {
    if (client === null || !canControl) return;
    const runID = snapshot.identity.run ?? '';
    actionPendingToken = snapshot.token;
    try {
      if (verb === 'approve') await client.control.approve(runID);
      else await client.control.reject(runID);
      setActionResult(snapshot.token, { token: snapshot.token, ok: true, message: `${verb}d` });
      await loadQueue();
    } catch (err) {
      const e = toError(err);
      setActionResult(snapshot.token, { token: snapshot.token, ok: false, message: `${e.code}: ${e.message}` });
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
  /* Boot                                                              */
  /* ================================================================ */
  onMount(() => {
    connection = resolveConnection();
    if (connection === null) {
      client = null;
      status = 'disconnected';
      queueStatus = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');

    // The runtime/agent label for the cost "Agent" axis: the agent-registry
    // name when the runtime has a registered agent, else the runtime host.
    try {
      runtimeLabel = new URL(connection.baseURL).host;
    } catch {
      runtimeLabel = 'This runtime';
    }
    void client.agents
      .list<{ agents?: Array<{ name?: string; display_name?: string; agent_id?: string }> }>()
      .then((r) => {
        const a = r.agents?.[0];
        const name = a?.display_name ?? a?.name ?? a?.agent_id;
        if (name) runtimeLabel = name;
      })
      .catch(() => {
        /* registry unavailable — keep the host label */
      });

    // The live event stream — activity + sparkline + cost + alerts + audit.
    const sub = new EventsSubscription(client.events);
    sub.open({
      eventTypes: [
        'session.opened',
        'session.closed',
        'task.started',
        'task.completed',
        'task.failed',
        'task.cancelled',
        'tool.failed',
        'agent.registered',
        'agent.restarted',
        'llm.cost.recorded',
        AUDIT_TYPE,
        ...ALERT_TYPES
      ]
    });
    subscription = sub;

    clockTimer = setInterval(() => (nowMillis = Date.now()), 1000);
    // Sample the gauges every 5s so the snapshot-counter sparklines fill with
    // real data over the session.
    sampleTimer = setInterval(() => void sampleCounters(), 5000);

    void reloadAll();
  });

  onDestroy(() => {
    subscription?.close();
    if (clockTimer !== null) clearInterval(clockTimer);
    if (sampleTimer !== null) clearInterval(sampleTimer);
  });
</script>

<svelte:head>
  <title>Overview · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="overview-page">
  <!-- Row 1 — slim context (health pill + audit ribbon). Runtime name /
       version / Protocol are app-shell chrome (108b), not duplicated here. -->
  <ContextAuditRow {health} {auditCount} {disconnected} onRefresh={() => void reloadAll()} />

  <!-- Row 1b — alerts strip (renders only when real in-window alerts exist). -->
  <AlertsStrip {alerts} now={nowMillis} />

  <!-- Row 2 — the 4 KPI cards. The page-level PageState drives this row. -->
  <PageState status={status} error={pageError} onretry={() => void reloadAll()}>
    {#snippet skeleton()}
      <div class="counter-row" aria-hidden="true">
        <span class="card-skeleton"></span>
        <span class="card-skeleton"></span>
        <span class="card-skeleton"></span>
        <span class="card-skeleton"></span>
      </div>
    {/snippet}
    {#snippet empty()}
      <p class="block-empty">Waiting for runtime activity.</p>
    {/snippet}

    {#if counters !== null}
      <div class="counter-row" data-testid="counter-row">
        <CounterCard
          testid="counter-events"
          label="Events / min"
          value={eventsPerMin.toFixed(1)}
          href="/events"
          icon={Activity}
          viewLabel="View Events"
          values={eventsValues}
          delta={eventsDelta}
        />
        <CounterCard
          testid="counter-tasks"
          label="Tasks Running (now)"
          value={String(counters.tasks_running)}
          href="/tasks"
          icon={ListChecks}
          viewLabel="View Tasks"
          values={hist.tasks}
          delta={tasksDelta}
        />
        <CounterCard
          testid="counter-jobs"
          label="Background Jobs (now)"
          value={String(counters.background_jobs_active)}
          href="/background-jobs"
          icon={Layers}
          viewLabel="View Background Jobs"
          values={hist.jobs}
          delta={jobsDelta}
          tone={counters.background_jobs_active > 0 ? 'warn' : 'accent'}
        />
        <CounterCard
          testid="counter-mcp"
          label="MCP Connections (now)"
          value={String(counters.mcp_connections_healthy)}
          href="/mcp-connections"
          icon={Plug}
          viewLabel="View MCP Connections"
          values={hist.mcp}
          delta={mcpDelta}
          tone="success"
        />
      </div>
    {/if}
  </PageState>

  <!-- Row 3 — interventions (left) + cost by-model (right). -->
  <div class="row-3">
    <section class="panel card queue-panel">
      <h2 class="panel-title">Interventions</h2>
      <PageState status={queueStatus} error={queueError} onretry={() => void loadQueue()} nested>
        {#snippet skeleton()}
          <div class="row-skeleton" aria-hidden="true"></div>
        {/snippet}
        {#snippet empty()}
          <div class="panel-empty" data-testid="intervention-queue-state-empty">
            <span class="empty-icon" data-tone="success"><CircleCheck size={20} aria-hidden="true" /></span>
            <p class="empty-headline">No pending interventions</p>
            <p class="empty-detail">No runs are parked awaiting an operator decision.</p>
          </div>
        {/snippet}
        <InterventionQueue
          snapshots={queueSnapshots}
          canControl={canControl}
          pendingToken={actionPendingToken}
          results={actionResults}
          onapprove={(s) => void dispatchRowAction(s, 'approve')}
          onreject={(s) => void dispatchRowAction(s, 'reject')}
        />
      </PageState>
    </section>

    <section class="panel card cost-panel">
      <h2 class="panel-title">Cost (last 24 hours)</h2>
      <CostRollupCard
        rollup={costRollup}
        breakdown={costBreakdown}
        onbreakdown={(b) => (costBreakdown = b)}
        {disconnected}
      />
    </section>
  </div>

  <!-- Row 4 — recent activity. -->
  <section class="panel card activity-panel">
    <h2 class="panel-title">Recent activity</h2>
    <RecentActivityFeed rows={activityRows} now={nowMillis} />
  </section>

  <!-- Row 5 — quick links + (deferred) layout customization. -->
  <section class="panel card quick-links-panel">
    <div class="quick-links-head">
      <h2 class="panel-title">Quick Links</h2>
      <button
        type="button"
        class="customize"
        data-testid="overview-customize"
        disabled
        title="Personal overview layouts — coming soon (page-overview.md §10 deferred)"
      >
        Customize overview
      </button>
    </div>
    <QuickLinksGrid />
  </section>
</div>

<style>
  .page {
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

  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  /* The shared carded surface for the row-3/row-4/row-5 panels — gives the
     interventions / cost / activity / quick-links sections the same panel
     background the mock shows (no more bare tables drifting on the page bg). */
  .card {
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .panel-title {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  .quick-links-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .customize {
    background: transparent;
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
  }

  .customize:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .block-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  /* Compact, intentional empty state for a carded panel — centred icon +
     headline + detail, modest height (no full-table dead space). */
  .panel-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-5) var(--space-2);
    text-align: center;
  }

  .empty-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: var(--radius-md);
    margin-bottom: var(--space-1);
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .card-skeleton {
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    height: var(--size-card-min);
  }

  .row-skeleton {
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    height: var(--size-sparkline-height);
  }
</style>
