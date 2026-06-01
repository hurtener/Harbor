<script lang="ts">
  // Harbor Console — Live Runtime page (`/live-runtime`), built on the
  // D-121 design-system foundation (CONVENTIONS.md; Phase 73b / D-126).
  //
  // The operator's present-tense workbench for initiating, observing,
  // and steering a live execution. Built entirely against the shared
  // foundation:
  //   - the four-state `<PageState>` async contract (CONVENTIONS.md §4):
  //     Disconnected / Loading / Error / Empty, mutually exclusive; the
  //     Error state has a working Retry and suppresses any stale view.
  //   - the shared `ui/` inventory (CONVENTIONS.md §3): `DetailRail` /
  //     `RailCard`, `Pagination`, `StatusChip`, `PageState`. A header row
  //     (the live status-counter strip + Refresh) + a tab-strip toolbar
  //     (Topology / Timeline / Metrics / Health) replace the saved-view
  //     FilterBar the mock does not have (Phase 108d). The primary view is
  //     the topology CANVAS — the shared `<TopologyCanvas>`. Live-Runtime
  //     pieces live in `components/live-runtime/`.
  //   - the unified `HarborClient` + `connection.ts` (CONVENTIONS.md §6):
  //     `client.tasks.list` (the status-counter-strip aggregate),
  //     `client.tasks.get` (per-task detail), `client.topology.snapshot`
  //     (the canvas), `client.events` (the SSE Event Stream), and the
  //     SHIPPED Phase 54 control verbs via `client.control.*`. No
  //     hand-rolled `fetch`, no page-local client, no direct
  //     `localStorage`.
  //
  // # NOT the chat module (D-091 + CLAUDE.md §4.5 #11)
  //
  // The composer (Start / Redirect / Inject / User message / Cancel /
  // Pause / Resume) is built with NON-CHAT Skeleton primitives — see
  // `components/live-runtime/composer/run-composer.svelte`. The chat
  // module's V1 first consumer is 73n Playground; a second in-V1
  // consumer would force extraction to `web/shared/chat/` (out of V1
  // scope). This page proves no import from `$lib/chat/`.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount, onDestroy } from 'svelte';
  import DetailRail from '$lib/components/ui/DetailRail.svelte';
  import RailCard from '$lib/components/ui/RailCard.svelte';
  import Pagination from '$lib/components/ui/Pagination.svelte';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  // The bottom status bar is rendered ONCE by the app shell
  // ((console)/+layout.svelte — CONVENTIONS.md §2, the global AppStatusBar).
  // Per-page footer strips duplicated it (post-83k N2 + Phase 108b); the
  // page-local LiveRuntimeFooter is removed.
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import StatusCounterStripView from '$lib/components/live-runtime/status-counter-strip.svelte';
  import {
    EMPTY_STRIP,
    applyTaskEvent,
    type StatusCounterStrip
  } from '$lib/live-runtime/strip.js';
  import TabStrip, { type LiveRuntimeTab } from '$lib/components/live-runtime/tab-strip.svelte';
  import TopologyCanvas from '$lib/components/live-runtime/topology-canvas.svelte';
  import TimelineTab from '$lib/components/live-runtime/timeline-tab.svelte';
  import MetricsTabEmpty from '$lib/components/live-runtime/metrics-tab-empty.svelte';
  import HealthTabEmpty from '$lib/components/live-runtime/health-tab-empty.svelte';
  import EventStreamDock from '$lib/components/live-runtime/event-stream-dock.svelte';
  import PerTaskDetailPane from '$lib/components/live-runtime/per-task-detail-pane.svelte';
  import RunComposer, {
    type ComposerVerb
  } from '$lib/components/live-runtime/composer/run-composer.svelte';
  import SessionDetailCard from '$lib/components/live-runtime/session-detail-card.svelte';
  import CurrentStepPanel from '$lib/components/live-runtime/current-step-panel.svelte';
  import RecentArtifactsPanel, {
    type RecentArtifact
  } from '$lib/components/live-runtime/recent-artifacts-panel.svelte';
  import InterventionsPanel, {
    type Intervention
  } from '$lib/components/live-runtime/interventions-panel.svelte';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError, isUnknownMethod } from '$lib/protocol/errors.js';
  import { EventsSubscription } from '$lib/events/subscription.svelte.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { TaskListResponse, TaskDetail } from '$lib/protocol/tasks.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import {
    resolveConnection,
    hasScope,
    DISCONNECTED_TOOLTIP,
    type RuntimeConnection
  } from '$lib/connection.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // The steering verbs are an elevated tier (D-066 / D-079) — without
  // the admin scope claim the composer + per-task control render
  // disabled-with-tooltip (CONVENTIONS.md §5).
  let canControl = $state(false);
  // The Phase 83r W2 disconnected predicate — drives the Refresh /
  // Save-view buttons + the composer (textarea + verbs).
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);
  // Phase 83w-F5 / D-164 — the friendly "topology not available on this
  // Runtime" info banner the page renders when topology.snapshot returns
  // `unknown_method` (planner/RunLoop runtimes — no engine graph). NOT
  // an error; the page is functional, the topology canvas just isn't
  // part of this Runtime's shape.
  let pageInfo = $state<{ headline: string; detail: string } | null>(null);

  /* ---- topology canvas (the §5 depth-bar primary view) ------------ */
  let projection = $state<TopologyProjection | null>(null);

  /* ---- header status-counter strip -------------------------------- */
  let strip = $state<StatusCounterStrip>({ ...EMPTY_STRIP });

  /* ---- main-canvas tab -------------------------------------------- */
  let activeTab = $state<LiveRuntimeTab>('topology');

  /* ---- node selection + per-task detail (nested PageState) -------- */
  let selectedNode = $state<string | null>(null);
  let detail = $state<TaskDetail | null>(null);
  let detailLoading = $state(false);

  /* ---- right-rail derived fields ---------------------------------- */
  let costUSD = $state(0);
  let lastError = $state<string | null>(null);
  let currentStep = $state<string | null>(null);
  let recentArtifacts = $state<RecentArtifact[]>([]);
  let interventions = $state<Intervention[]>([]);

  /* ---- composer --------------------------------------------------- */
  let composerPending = $state(false);
  let composerResult = $state<{ ok: boolean; message: string } | null>(null);

  /* ---- live event stream (the bottom-dock left pane) -------------- */
  let subscription = $state<EventsSubscription | null>(null);
  let traceOn = $state(false);

  /* ---- pagination (the recent-events page window) ----------------- */
  let pageIndex = $state(1);
  let pageSize = $state(50);

  /* ================================================================ */
  /* Derived                                                           */
  /* ================================================================ */

  // The Trace tab narrows the event stream to the selected node's run.
  // V1 uses the node name as the run-correlation key (the topology
  // projection is engine-scoped; the selected node names the run the
  // Trace tab correlates against — the D-082 run carrier).
  let traceRunID = $derived(selectedNode ?? '');

  // The run-scoped event slice the per-task detail's Trace tab renders.
  let traceEvents = $derived<Event[]>(
    subscription === null
      ? []
      : subscription.events.filter((ev) => traceRunID !== '' && ev.run === traceRunID)
  );

  // W10 (Phase 83x): the session-detail rail's `Status` field used to
  // mirror the PAGE's PageStatus (`'ready' ? 'active' : status`). That
  // wired the rail to react to a topology-snapshot failure — a load()
  // failure left the rail reading "error" forever, even after the task
  // itself had completed. Derive the session-level status from the
  // status-counter strip instead: it is the live aggregate of every
  // task event in the session and reflects the SESSION's lifecycle
  // truthfully (active while anything is in-flight; complete when
  // everything terminated cleanly; failed when something failed). The
  // page's own loading-state status remains separate and drives the
  // PageState chrome — the rail stops conflating the two.
  let sessionStatusLabel = $derived(
    status === 'disconnected'
      ? 'disconnected'
      : strip.running + strip.pending + strip.paused > 0
        ? 'active'
        : strip.failed > 0 && strip.completed === 0
          ? 'failed'
          : strip.completed > 0
            ? 'complete'
            : 'idle'
  );

  // The event-stream dock page window — real pagination over the
  // rolling event buffer (not a fake "load more").
  let pagedEvents = $derived<Event[]>(
    subscription === null
      ? []
      : subscription.events.slice((pageIndex - 1) * pageSize, pageIndex * pageSize)
  );
  let totalEvents = $derived(subscription === null ? 0 : subscription.events.length);
  // The SSE stream state — consumed by the EventStreamDock (the footer that
  // also read it was removed in Phase 108b; the dock keeps it).
  let streamState = $derived(subscription?.state ?? 'idle');

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

  // load resolves the topology snapshot + the status-counter-strip
  // aggregate. Both go through the typed client; a thrown ProtocolError
  // routes into PageState's Error state with a working Retry.
  //
  // Phase 83w-F5 / D-164 — `topology.snapshot` returning `unknown_method`
  // is not an error: the Runtime is planner/RunLoop-shaped (no engine
  // graph), so the topology surface is honestly not part of its shape.
  // The page maps that one error code to the friendly `info` branch
  // (a banner, not a red ERROR with a Retry that would always fail).
  // Every other error — `identity_required`, `unauthorized`, transport
  // failure — still routes into the Error state with a working Retry.
  async function load(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    pageInfo = null;
    try {
      // Round-8 F1 / phase 84a: gate the topology fetch behind the
      // runtime's advertised capabilities. A planner/RunLoop runtime
      // (the dev posture) omits `topology_snapshot` from
      // `runtime.info.capabilities`; the Console then short-circuits
      // to the info banner WITHOUT making the fetch, so the browser
      // network log stays clean. The D-164 `unknown_method` catch
      // remains the safety net for any runtime that advertises the
      // capability but rejects at the wire (defence-in-depth).
      const caps = await client.capabilities();
      if (!caps.has('topology_snapshot')) {
        projection = null;
        const taskResp = await client.tasks.list<TaskListResponse>({
          include_status_counter_strip: true
        });
        strip = taskResp.status_counter_strip ?? { ...EMPTY_STRIP };
        pageInfo = {
          headline: 'Topology view not available on this Runtime',
          detail:
            'This runtime is planner/RunLoop-shaped, not engine-graph-shaped. See docs/CONFIG.md for runtime shapes.'
        };
        status = 'info';
        return;
      }
      const [proj, taskResp] = await Promise.all([
        client.topology.snapshot<TopologyProjection>(),
        client.tasks.list<TaskListResponse>({ include_status_counter_strip: true })
      ]);
      projection = proj;
      strip = taskResp.status_counter_strip ?? { ...EMPTY_STRIP };
      status = proj.nodes.length === 0 ? 'empty' : 'ready';
    } catch (err) {
      // The Error / Info states both suppress any stale view — drop
      // last-good data so the canvas doesn't render a half-state.
      projection = null;
      strip = { ...EMPTY_STRIP };
      if (isUnknownMethod(err)) {
        pageInfo = {
          headline: 'Topology view not available on this Runtime',
          detail:
            'This runtime is planner/RunLoop-shaped, not engine-graph-shaped. See docs/CONFIG.md for runtime shapes.'
        };
        status = 'info';
      } else {
        pageError = toError(err);
        status = 'error';
      }
    }
  }

  async function loadDetail(node: string): Promise<void> {
    if (client === null) {
      return;
    }
    detailLoading = true;
    try {
      // V1: the topology node names the task the per-task pane inspects.
      detail = await client.tasks.get<TaskDetail>(node);
    } catch {
      // A node with no task projection is honest "no detail" — not an
      // error of the whole page (the Trace tab still renders the
      // run-scoped event slice).
      detail = null;
    } finally {
      detailLoading = false;
    }
  }

  /* ================================================================ */
  /* Live event stream                                                 */
  /* ================================================================ */

  // mirrorEvent keeps the header strip + rail live: a `task.*` event on
  // the SSE feed shifts the counts via the pure `applyTaskEvent` reducer
  // (in `$lib/live-runtime/strip.ts`) without re-polling `tasks.list`
  // (the aggregate call is the initial-load shape only). The deltas are
  // best-effort; the authoritative recount is the next `load()`.
  function mirrorEvent(ev: Event): void {
    strip = applyTaskEvent(strip, ev.type);
    if (ev.type === 'task.failed') {
      lastError = `task.failed (${ev.run ?? 'unknown run'})`;
    }
    if (ev.type.startsWith('planner.')) {
      currentStep = ev.type;
    }
  }

  // This effect mirrors new `task.*` / `planner.*` events into the live
  // strip + rail — Svelte's runes already make `subscription.events`
  // reactive, so the page reads it directly.
  let lastSeenSeq = 0;
  $effect(() => {
    if (subscription === null) {
      return;
    }
    for (const ev of subscription.events) {
      if (ev.sequence > lastSeenSeq) {
        mirrorEvent(ev);
      }
    }
    if (subscription.events.length > 0) {
      lastSeenSeq = Math.max(lastSeenSeq, subscription.events[0].sequence);
    }
  });

  /* ================================================================ */
  /* Node selection                                                    */
  /* ================================================================ */

  function selectNode(node: string): void {
    selectedNode = node;
    void loadDetail(node);
  }

  function setTab(tab: LiveRuntimeTab): void {
    activeTab = tab;
  }

  function toggleTrace(next: boolean): void {
    traceOn = next;
  }

  /* ================================================================ */
  /* Composer — the SHIPPED Phase 54 control surface (§13)             */
  /* ================================================================ */

  async function dispatchVerb(verb: ComposerVerb, text: string): Promise<void> {
    if (client === null) {
      return;
    }
    composerPending = true;
    composerResult = null;
    try {
      if (verb === 'start') {
        // `start` spawns a task; it is the owner-tier verb.
        await client.control.dispatch('start', '', { query: text });
      } else {
        // The remaining verbs target the selected node's run; without a
        // selected node there is no run to steer.
        const runID = selectedNode ?? '';
        if (runID === '' && verb !== 'user_message') {
          composerResult = { ok: false, message: 'Select a topology node to target a run.' };
          composerPending = false;
          return;
        }
        await client.control.dispatch(verb, runID, text ? { text } : undefined);
      }
      composerResult = { ok: true, message: `${verb} dispatched.` };
      interventions = [
        { label: verb, detail: text || '(no payload)', ok: true },
        ...interventions
      ];
    } catch (err) {
      const e = toError(err);
      composerResult = { ok: false, message: `${e.code}: ${e.message}` };
      interventions = [
        { label: verb, detail: `${e.code}: ${e.message}`, ok: false },
        ...interventions
      ];
    } finally {
      composerPending = false;
    }
  }

  async function prioritizeTask(taskID: string, priority: number): Promise<void> {
    if (client === null || !canControl) {
      return;
    }
    try {
      await client.control.prioritize(taskID, priority);
      interventions = [
        { label: 'prioritize', detail: `${taskID} → ${priority}`, ok: true },
        ...interventions
      ];
      if (selectedNode !== null) {
        await loadDetail(selectedNode);
      }
    } catch (err) {
      const e = toError(err);
      interventions = [
        { label: 'prioritize', detail: `${e.code}: ${e.message}`, ok: false },
        ...interventions
      ];
    }
  }

  /* ================================================================ */
  /* Boot                                                              */
  /* ================================================================ */

  onMount(() => {
    connection = resolveConnection();
    if (connection === null) {
      client = null;
      status = 'disconnected';
      return;
    }
    client = injectedClient ?? new HarborClient({ connection });
    canControl = hasScope(connection, 'admin');

    // Open the live event stream — the bottom-dock Event Stream pane. The
    // Runtime emits NAMED SSE frames, so the subscription MUST list the event
    // types it wants (108c named-event fix) — an empty open() receives nothing.
    const sub = new EventsSubscription(
      (client as HarborClient).events ?? new HarborClient({ connection }).events
    );
    sub.open({
      eventTypes: [
        'task.spawned',
        'task.started',
        'task.completed',
        'task.failed',
        'task.cancelled',
        'tool.invoked',
        'tool.completed',
        'tool.failed',
        'planner.decision',
        'planner.finish',
        'planner.error',
        'pause.requested',
        'pause.resumed',
        'tool.approval_requested',
        'tool.approved',
        'tool.rejected',
        'tool.auth_required',
        'control.received',
        'control.applied',
        'control.rejected',
        'session.opened',
        'session.closed',
        'llm.cost.recorded'
      ]
    });
    subscription = sub;

    void load();
  });

  onDestroy(() => {
    subscription?.close();
  });
</script>

<svelte:head>
  <title>Live Runtime · Harbor Console</title>
</svelte:head>

<div class="page" data-testid="live-runtime-page">
  <!-- Header row: the live status-counter strip (left) + Refresh (right).
       Runtime name / breadcrumb live in the app-shell chrome (108b). -->
  <div class="header-row">
    <StatusCounterStripView {strip} />
    <button
      type="button"
      class="refresh"
      data-testid="live-runtime-refresh"
      disabled={disconnected}
      title={disconnected ? DISCONNECTED_TOOLTIP : 'Refresh'}
      onclick={() => void load()}
    >
      <RotateCw size={14} aria-hidden="true" /> Refresh
    </button>
  </div>

  <!-- Tab strip toolbar (Topology | Timeline | Metrics | Health). The mock's
       Status/Type/Run/Time-range/Pause filters belong to the topology canvas
       controls (Stage 2); the saved-view bar the mock omits is removed. -->
  <div class="toolbar">
    <TabStrip active={activeTab} onselect={setTab} />
  </div>

  <div class="layout">
    <div class="main-col">
      <PageState status={status} error={pageError} info={pageInfo} onretry={() => void load()}>
        {#snippet skeleton()}
          <div class="canvas-skeleton" aria-hidden="true"></div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="live-runtime-empty">
            <p class="headline">The Runtime engine has no topology</p>
            <p class="detail">
              No engine graph is registered, or the Runtime exposes no
              nodes yet.
            </p>
            <button type="button" class="refresh" onclick={() => void load()}>
              Reload
            </button>
          </div>
        {/snippet}

        {#if activeTab === 'topology' && projection !== null}
          <TopologyCanvas
            projection={projection}
            selectedNode={selectedNode}
            onnodeclick={selectNode}
          />
        {:else if activeTab === 'timeline' && projection !== null}
          <TimelineTab
            projection={projection}
            selectedNode={selectedNode}
            onselect={selectNode}
          />
        {:else if activeTab === 'metrics'}
          <MetricsTabEmpty />
        {:else if activeTab === 'health'}
          <HealthTabEmpty />
        {/if}
      </PageState>

      {#if status === 'ready'}
        <Pagination
          page={pageIndex}
          pageSize={pageSize}
          total={totalEvents}
          onpage={(p) => (pageIndex = p)}
          onpagesize={(s) => {
            pageSize = s;
            pageIndex = 1;
          }}
        />
      {/if}

      <div class="bottom-dock">
        <EventStreamDock
          events={pagedEvents}
          streamState={streamState}
          traceOn={traceOn}
          traceRunID={traceRunID}
          ontracetoggle={toggleTrace}
        />
        {#if selectedNode !== null}
          <PerTaskDetailPane
            detail={detail}
            loading={detailLoading}
            traceEvents={traceEvents}
            canControl={canControl}
            onprioritize={(id, p) => void prioritizeTask(id, p)}
          />
        {:else}
          <RunComposer
            canControl={canControl}
            pending={composerPending}
            result={composerResult}
            {disconnected}
            onverb={(verb, text) => void dispatchVerb(verb, text)}
          />
        {/if}
      </div>

    </div>

    <DetailRail>
      <RailCard title="Session">
        {#if connection !== null}
          <SessionDetailCard
            identity={connection.identity}
            agentName="default agent"
            sessionStatus={sessionStatusLabel}
            costUSD={costUSD}
            lastError={lastError}
            tenant={connection.identity.tenant}
          />
        {:else}
          <p class="rail-note">Not connected to a Runtime.</p>
        {/if}
      </RailCard>
      <RailCard title="Current step">
        <CurrentStepPanel step={currentStep} detail="Derived from the live planner event stream." />
      </RailCard>
      <RailCard title="Recent artifacts">
        <RecentArtifactsPanel artifacts={recentArtifacts} />
      </RailCard>
      <RailCard title="Interventions">
        <InterventionsPanel interventions={interventions} />
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

  .bottom-dock {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-4);
    align-items: start;
  }

  .header-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    flex-wrap: wrap;
  }

  .toolbar {
    display: flex;
    align-items: center;
  }

  .refresh {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    background: var(--color-surface-raised);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .refresh:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .canvas-skeleton {
    height: var(--space-12);
    background: var(--color-surface-raised);
    border-radius: var(--radius-md);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-12) var(--space-4);
    text-align: center;
  }

  .empty-block .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-block .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rail-note {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
