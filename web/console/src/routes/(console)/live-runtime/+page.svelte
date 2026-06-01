<script lang="ts">
  // Harbor Console — Live Runtime page (`/live-runtime`) — the single-runtime
  // capability-adaptive operations COCKPIT (Phase 108e / D-177; reframes the
  // 108d topology-first build). The Overview(fleet) → runtime drill-down: one
  // runtime selected at a time, its composition a PURE function of the
  // runtime's advertised `runtime.info.capabilities`.
  //
  // # The cockpit composition (panels.ts::resolvePanels)
  //
  // A declarative capability→panel registry yields an always-present SPINE
  // (posture · activity counters · needs-attention · live events · active
  // sessions · health · cost) plus CAPABILITY-GATED panels (topology, gated on
  // `topology_snapshot`; future multi-agent / workflow shapes are additive).
  // The page reads `resolvePanels(capabilities)` and renders only the panels it
  // returns — adding a runtime shape adds a registry entry, never a page
  // rewrite. On a planner/RunLoop runtime (no topology) the cockpit is a full
  // viewport of meaningful panels, not a topology hero void.
  //
  // # No Playground overlap (D-062 / D-091)
  //
  // The free-floating Start/Redirect/Inject/User-message composer is REMOVED —
  // conversational steering belongs to the Playground (chat is ONE panel, not
  // this page). Run-level steering is a drill into a session → Playground. NO
  // tabs, NO bottom-dock, NO chat-module import.
  //
  // # Data sources — all REAL, honest fallback, zero fabrication (CLAUDE.md §13)
  //
  // Every panel sources a shipped Protocol surface through the typed
  // HarborClient (CONVENTIONS.md §6): `client.capabilities()` (the composition
  // input), `client.runtime.counters/health()`, `client.pause.list()` +
  // `client.control.approve/reject/resume()`, `client.events` (the SSE stream),
  // `client.agents.list()` (the runtime label), `client.topology.snapshot()`
  // (gated), `client.posture.info()` (the Protocol version). A surface that
  // throws / returns `unknown_method` degrades to an honest empty state.
  //
  // # Layout — viewport-locked (D-177 layout bar)
  //
  // The page is a flex column that fills the shell content region: rows 1–2
  // (posture header + counter strip) are fixed; row 3 is a two-column grid
  // (3fr | 2fr) that flex-grows to fill the remaining viewport, with
  // `min-height: 0` so its tall panels (Live events, Needs attention) scroll
  // INTERNALLY — the PAGE never grows a full-page scrollbar.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import { onMount, onDestroy } from 'svelte';
  import CircleCheck from '@lucide/svelte/icons/circle-check';
  import PageState, { type PageStatus } from '$lib/components/ui/PageState.svelte';
  import RuntimePostureHeader from '$lib/components/live-runtime/runtime-posture-header.svelte';
  import StatusCounterStripView from '$lib/components/live-runtime/status-counter-strip.svelte';
  import NeedsAttentionPanel, {
    type AttentionActionResult
  } from '$lib/components/live-runtime/needs-attention-panel.svelte';
  import EventStreamDock from '$lib/components/live-runtime/event-stream-dock.svelte';
  import ActiveSessionsPanel, {
    type RecentSession
  } from '$lib/components/live-runtime/active-sessions-panel.svelte';
  import HealthPanel from '$lib/components/live-runtime/health-panel.svelte';
  import CostGovernancePanel from '$lib/components/live-runtime/cost-governance-panel.svelte';
  import TopologyPanel from '$lib/components/live-runtime/topology-panel.svelte';
  import { resolvePanels, CAP_TOPOLOGY_SNAPSHOT } from '$lib/live-runtime/panels.js';
  import {
    EMPTY_STRIP,
    applyTaskEvent,
    type StatusCounterStrip
  } from '$lib/live-runtime/strip.js';
  import {
    LIVE_RUNTIME_EVENT_TYPES,
    toError,
    nodeStateForEvent,
    sessionStatusLabel as deriveSessionStatusLabel,
    foldRecentSessions
  } from '$lib/live-runtime/page-data.js';
  import { HarborClient, type ProtocolClient } from '$lib/protocol/harbor.js';
  import { ProtocolError, isUnknownMethod } from '$lib/protocol/errors.js';
  import { EventsSubscription } from '$lib/events/subscription.svelte.js';
  import type { Event } from '$lib/protocol/events.js';
  import type { TaskListResponse } from '$lib/protocol/tasks.js';
  import type { TopologyProjection } from '$lib/protocol/topology.js';
  import type { NodeState } from '$lib/live-runtime/topology-adapter.js';
  import type { RuntimeCounters, RuntimeHealth } from '$lib/protocol/posture.js';
  import type { PauseListResponse, PauseSnapshot } from '$lib/protocol/pause.js';
  import { DEFAULT_PAUSE_LIST_PAGE_SIZE } from '$lib/protocol/pause.js';
  import { projectCost } from '$lib/overview/cost.js';
  import { resolveConnection, hasScope, type RuntimeConnection } from '$lib/connection.js';

  /* ---- connection + client (CONVENTIONS.md §6) -------------------- */
  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  let connection = $state<RuntimeConnection | null>(null);
  let client = $state<ProtocolClient | null>(null);
  // Control verbs are an elevated tier (D-066 / D-079) — without the admin
  // scope claim the Needs-attention verbs render disabled-with-tooltip.
  let canControl = $state(false);
  let disconnected = $derived(connection === null);

  /* ---- page-level async state (the four-state contract) ----------- */
  let status = $state<PageStatus>('loading');
  let pageError = $state<ProtocolError | { code: string; message: string } | null>(null);

  /* ---- the advertised capability set (the composition input) ------ */
  let capabilities = $state<ReadonlySet<string>>(new Set());
  let capabilityList = $derived([...capabilities]);
  let panels = $derived(resolvePanels(capabilities));
  let topologyAvailable = $derived(capabilities.has(CAP_TOPOLOGY_SNAPSHOT));

  /* ---- posture header --------------------------------------------- */
  let runtimeLabel = $state('This runtime');
  let protocolVersion = $state('');

  /* ---- activity strip --------------------------------------------- */
  let strip = $state<StatusCounterStrip>({ ...EMPTY_STRIP });
  let counters = $state<RuntimeCounters | null>(null);

  /* ---- needs-attention (nested PageState) ------------------------- */
  let queueStatus = $state<PageStatus>('loading');
  let queueResp = $state<PauseListResponse | null>(null);
  let queueError = $state<ProtocolError | { code: string; message: string } | null>(null);
  let queuePage = $state(1);
  let queuePageSize = $state(DEFAULT_PAUSE_LIST_PAGE_SIZE);
  let actionPendingToken = $state<string | null>(null);
  let actionResults = $state<Map<string, AttentionActionResult>>(new Map());
  let queueSnapshots = $derived<PauseSnapshot[]>(queueResp?.snapshots ?? []);

  /* ---- health (self-probing spine panel) -------------------------- */
  let health = $state<RuntimeHealth | null>(null);

  /* ---- topology (capability-gated) -------------------------------- */
  let projection = $state<TopologyProjection | null>(null);
  let nodeStates = $state<Record<string, NodeState>>({});
  let selectedNode = $state<string | null>(null);
  let streamPaused = $state(false);

  /* ---- live event stream ------------------------------------------ */
  let subscription = $state<EventsSubscription | null>(null);
  let traceOn = $state(false);

  /* ---- rail derived fields (cost / last error) -------------------- */
  let lastError = $state<string | null>(null);

  /* ================================================================ */
  /* Derived — folded client-side off the event stream                 */
  /* ================================================================ */
  let liveEvents = $derived<Event[]>(subscription?.events ?? []);
  let pagedEvents = $derived<Event[]>(liveEvents.slice(0, 80));
  let streamState = $derived(subscription?.state ?? 'idle');
  let traceRunID = $derived(selectedNode ?? '');
  let recentSessions = $derived<RecentSession[]>(foldRecentSessions(liveEvents));
  let costRollup = $derived(projectCost(liveEvents, 'model'));
  let costUSD = $derived(costRollup.totalUSD);
  // W10 (Phase 83x): the session status reads the DERIVED strip label, not the
  // page's own PageStatus — a topology/load failure never poisons the rail.
  let sessionStatusLabel = $derived(deriveSessionStatusLabel(strip, status));

  /* ================================================================ */
  /* Loading                                                           */
  /* ================================================================ */

  // load() resolves the cockpit's composition + spine data in one pass:
  //   1. `client.capabilities()` — the panel registry input (drives which
  //      panels render + whether topology is gated in/out).
  //   2. the always-on spine surfaces (counters strip via tasks.list, the
  //      pause queue, runtime.health, posture.info) — each self-probing and
  //      degrading honestly on a throw.
  //   3. the topology snapshot ONLY when `topology_snapshot` is advertised
  //      (no wasted fetch on a planner runtime — the D-164 short-circuit).
  // A capabilities()/counters() failure routes the PAGE into Error with a
  // working Retry; the per-panel surfaces self-probe so one weak surface never
  // empties the whole cockpit.
  async function load(): Promise<void> {
    if (client === null) {
      status = 'disconnected';
      queueStatus = 'disconnected';
      return;
    }
    status = 'loading';
    pageError = null;
    try {
      const caps = await client.capabilities();
      capabilities = caps;

      // The status-counter strip (initial-load aggregate) — the page's
      // primary always-on view; a failure here flips the page to Error.
      const taskResp = await client.tasks.list<TaskListResponse>({
        include_status_counter_strip: true
      });
      strip = taskResp.status_counter_strip ?? { ...EMPTY_STRIP };
      status = 'ready';

      // The remaining spine surfaces self-probe in parallel (a throw on any
      // one degrades just that panel — never the page).
      void loadCounters();
      void loadHealth();
      void loadQueue();

      // Topology only when advertised — the D-164 short-circuit keeps the
      // network log clean on planner/RunLoop runtimes.
      if (caps.has(CAP_TOPOLOGY_SNAPSHOT)) {
        void loadTopology();
      } else {
        projection = null;
      }
    } catch (err) {
      strip = { ...EMPTY_STRIP };
      pageError = toError(err);
      status = 'error';
    }
  }

  async function loadCounters(): Promise<void> {
    if (client === null) return;
    try {
      counters = await client.runtime.counters();
    } catch {
      counters = null;
    }
  }

  async function loadHealth(): Promise<void> {
    if (client === null) return;
    try {
      health = await client.runtime.health();
    } catch {
      // unknown_method / unavailable → honest "not available" (no fabrication).
      health = null;
    }
  }

  async function loadTopology(): Promise<void> {
    if (client === null) return;
    try {
      projection = await client.topology.snapshot<TopologyProjection>();
    } catch (err) {
      // A runtime that advertises the capability but rejects at the wire
      // (defence-in-depth) falls back to the honest panel-empty state.
      if (isUnknownMethod(err)) {
        projection = null;
      } else {
        projection = null;
      }
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

  /* ---- needs-attention actions — the SHIPPED Phase 54 verbs -------- */
  function setActionResult(token: string, result: AttentionActionResult): void {
    const next = new Map(actionResults);
    next.set(token, result);
    actionResults = next;
  }

  async function dispatchRowAction(
    snapshot: PauseSnapshot,
    verb: 'approve' | 'reject' | 'resume'
  ): Promise<void> {
    if (client === null || !canControl) return;
    const runID = snapshot.identity.run ?? '';
    actionPendingToken = snapshot.token;
    try {
      if (verb === 'approve') await client.control.approve(runID);
      else if (verb === 'reject') await client.control.reject(runID);
      else await client.control.resume(runID);
      setActionResult(snapshot.token, { token: snapshot.token, ok: true, message: `${verb}d` });
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

  /* ================================================================ */
  /* Live event stream — fold task.* deltas into the strip             */
  /* ================================================================ */
  function mirrorEvent(ev: Event): void {
    strip = applyTaskEvent(strip, ev.type);
    if (ev.type === 'task.failed') {
      lastError = `task.failed (${ev.run ?? 'unknown run'})`;
    }
    const nodeKey = ev.extra?.source ?? ev.run;
    if (nodeKey !== undefined && nodeKey !== '') {
      const next = nodeStateForEvent(ev.type, ev.payload);
      if (next !== null) {
        nodeStates = { ...nodeStates, [nodeKey]: next };
      }
    }
  }

  let lastSeenSeq = 0;
  $effect(() => {
    if (subscription === null) return;
    if (streamPaused) return;
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
  /* Node selection (topology) + trace toggle                          */
  /* ================================================================ */
  function selectNode(node: string): void {
    selectedNode = node;
  }

  function toggleTrace(next: boolean): void {
    traceOn = next;
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

    // The runtime label: the agent-registry display name when available, else
    // the runtime host (the Overview pattern — no fabrication).
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

    // The Protocol version banner — runtime.info is universally advertised.
    void client.posture
      .info<{ protocol_version?: string }>()
      .then((info) => {
        if (info.protocol_version) protocolVersion = info.protocol_version;
      })
      .catch(() => {
        /* keep empty — never fabricate a version */
      });

    // Open the live event stream — NAMED SSE frames (108c named-event fix).
    const sub = new EventsSubscription(client.events);
    sub.open({ eventTypes: LIVE_RUNTIME_EVENT_TYPES });
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
  <!-- Row 1 — runtime posture header (full width). -->
  <RuntimePostureHeader
    runtimeLabel={runtimeLabel}
    protocolVersion={protocolVersion}
    capabilities={capabilityList}
    disconnected={disconnected}
    onRefresh={() => void load()}
  />

  <!-- Rows 2+3 — the page-level four-state contract. A catastrophic
       capabilities()/tasks.list failure routes here into Error with a working
       Retry; loading shows a shape-matched skeleton; disconnected defers to
       the app-shell redirect. On ready the cockpit body renders (its spine
       panels self-probe their surfaces independently). -->
  <div class="page-body">
    <PageState status={status} error={pageError} onretry={() => void load()}>
      {#snippet skeleton()}
        <div class="body-skeleton" aria-hidden="true">
          <span class="card-skeleton"></span>
          <span class="card-skeleton"></span>
        </div>
      {/snippet}
      {#snippet empty()}
        <p class="block-empty">Waiting for runtime activity.</p>
      {/snippet}

      <!-- Row 2 — the activity status-counter strip (full width). -->
      <section class="panel card" data-testid="cockpit-activity">
        <h2 class="panel-title">Activity</h2>
        <StatusCounterStripView {strip} />
      </section>

      <!-- Row 3 — the cockpit grid (3fr | 2fr); flex-grows to fill the viewport.
           LEFT spine: Needs attention + Live events (internal scroll).
           RIGHT: Active sessions + Health + Cost + Topology (gated). -->
      <div class="cockpit" data-testid="cockpit-grid">
    <div class="col-left">
      {#each panels.filter((p) => p.column === 'left') as panel (panel.id)}
        {#if panel.id === 'needs-attention'}
          <section class="panel card attention-card" data-testid="panel-needs-attention">
            <h2 class="panel-title">Needs attention</h2>
            <PageState
              status={queueStatus}
              error={queueError}
              onretry={() => void loadQueue()}
              nested
            >
              {#snippet skeleton()}
                <div class="row-skeleton" aria-hidden="true"></div>
              {/snippet}
              {#snippet empty()}
                <div class="panel-empty" data-testid="needs-attention-empty">
                  <span class="empty-icon" data-tone="success">
                    <CircleCheck size={20} aria-hidden="true" />
                  </span>
                  <p class="empty-headline">No pending interventions</p>
                  <p class="empty-detail">
                    No runs are parked awaiting an operator decision.
                  </p>
                </div>
              {/snippet}
              <NeedsAttentionPanel
                snapshots={queueSnapshots}
                canControl={canControl}
                pendingToken={actionPendingToken}
                results={actionResults}
                onapprove={(s) => void dispatchRowAction(s, 'approve')}
                onreject={(s) => void dispatchRowAction(s, 'reject')}
                onresume={(s) => void dispatchRowAction(s, 'resume')}
              />
            </PageState>
          </section>
        {:else if panel.id === 'topology'}
          <section class="panel card topology-card" data-testid="panel-topology">
            <h2 class="panel-title">Topology</h2>
            <TopologyPanel
              available={topologyAvailable}
              projection={projection}
              selectedNode={selectedNode}
              nodeStates={nodeStates}
              streamPaused={streamPaused}
              onnodeclick={selectNode}
              onstreamtoggle={(next) => (streamPaused = next)}
            />
          </section>
        {:else if panel.id === 'live-events'}
          <section class="panel card events-card" data-testid="panel-live-events">
            <h2 class="panel-title">Live events</h2>
            <div class="events-scroll">
              <EventStreamDock
                events={pagedEvents}
                streamState={streamState}
                traceOn={traceOn}
                traceRunID={traceRunID}
                ontracetoggle={toggleTrace}
              />
            </div>
          </section>
        {/if}
      {/each}
    </div>

    <div class="col-right">
      {#each panels.filter((p) => p.column === 'right') as panel (panel.id)}
        {#if panel.id === 'active-sessions'}
          <section class="panel card" data-testid="panel-active-sessions">
            <h2 class="panel-title">Active sessions</h2>
            <ActiveSessionsPanel
              activeCount={counters?.sessions_active ?? 0}
              sessions={recentSessions}
              identity={connection?.identity ?? null}
              sessionStatusLabel={sessionStatusLabel}
              costUSD={costUSD}
              lastError={lastError}
            />
          </section>
        {:else if panel.id === 'health'}
          <section class="panel card" data-testid="panel-health">
            <h2 class="panel-title">Health</h2>
            <HealthPanel health={health} />
          </section>
        {:else if panel.id === 'cost'}
          <section class="panel card" data-testid="panel-cost">
            <h2 class="panel-title">Cost</h2>
            <CostGovernancePanel rollup={costRollup} />
          </section>
        {/if}
      {/each}
    </div>
      </div>
    </PageState>
  </div>
</div>

<style>
  /* The page fills the shell content region as a flex column so the cockpit
     grid (row 3) can flex-grow and the page never grows a full-page
     scrollbar (D-177 viewport-lock). */
  .page {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
    height: 100%;
    min-height: var(--space-0);
  }

  /* The page body (rows 2+3) flex-grows so the cockpit grid fills the
     remaining viewport; it is itself a flex column so the row-2 strip is
     fixed and the row-3 grid grows. */
  .page-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    flex: 1 1 auto;
    min-height: var(--space-0);
    min-width: var(--space-0);
  }

  .panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-width: var(--space-0);
  }

  .card {
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

  /* Row 3 — the two-column cockpit grid. It flex-grows to fill the remaining
     viewport height; `min-height: 0` lets its tall children scroll INTERNALLY
     rather than push the page taller. */
  .cockpit {
    display: grid;
    grid-template-columns: 3fr 2fr;
    gap: var(--space-4);
    align-items: start;
    flex: 1 1 auto;
    min-height: var(--space-0);
  }

  .col-left,
  .col-right {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: var(--space-0);
    min-height: var(--space-0);
    height: 100%;
  }

  /* The Live events card is the vertical-space filler in the left column: it
     grows to consume the remaining height and scrolls internally. */
  .events-card {
    flex: 1 1 auto;
    min-height: var(--space-0);
  }

  .events-scroll {
    flex: 1 1 auto;
    min-height: var(--space-0);
    overflow: auto;
  }

  /* Needs attention is compact at the top of the left spine and scrolls
     internally when the queue is long, never pushing the page taller. */
  .attention-card {
    max-height: var(--size-graph-max-height);
    overflow: auto;
  }

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

  .row-skeleton {
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    height: var(--size-sparkline-height);
  }

  .body-skeleton {
    display: grid;
    grid-template-columns: 3fr 2fr;
    gap: var(--space-4);
  }

  .card-skeleton {
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    height: var(--size-card-min);
  }

  .block-empty {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }
</style>
