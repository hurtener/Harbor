<script lang="ts">
  // Harbor Console — Sessions detail bottom-dock tab strip (Phase 108g /
  // D-179; rewritten from the Phase 73c / D-122 placeholder blurbs).
  //
  // The five tabs (Trajectory | Events | Cost History | Control History |
  // Interventions) each render REAL data — a session-filtered projection
  // of the shipped `events.subscribe` SSE (the same stream the Events,
  // Overview and Live Runtime pages consume). The dock owns ONE
  // session-scoped subscription; each tab is a derived view over it. No
  // new Protocol method (PAGE-POLISH §3):
  //   - Trajectory    ← planner.* / tool.* / task.* lifecycle → step timeline
  //   - Events        ← the raw session-filtered event log
  //   - Cost History  ← `llm.cost.recorded` summed client-side (overview/cost)
  //   - Control       ← `control.received` / `control.applied` / `control.rejected`
  //   - Interventions ← `pause.*` / `tool.approval_*` / `tool.auth_*` events
  //                     + a `pause.list` backfill of still-pending pauses,
  //                     with a real Resume (`approve`) / Reject action (D-066).
  //
  // The subscription opens a per-session client (identity.session =
  // sessionID) so the runtime scopes the SSE server-side to this session
  // and replays its buffered events on connect (stream.go). A session
  // whose events have aged out of an in-memory bus renders an honest
  // empty tab — never a fabricated history (CLAUDE.md §13).
  //
  // Sessions-specific component. Svelte 5 runes (D-092); tokens only.
  import { HarborClient } from '$lib/protocol/harbor.js';
  import type { RuntimeConnection } from '$lib/connection.js';
  import { EventsSubscription } from '$lib/events/subscription.svelte.js';
  import { EVENT_TYPES, categoryOf, categoryKind } from '$lib/events/taxonomy.js';
  import { projectTrajectory } from '$lib/sessions/trajectory.js';
  import { projectCost } from '$lib/overview/cost.js';
  import { exportEventsNDJSON, exportMeta, triggerDownload } from '$lib/events/export.js';
  import { formatRelative } from '$lib/sessions/format.js';
  import { StatusChip } from '$lib/components/ui/index.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import type { PauseSnapshot } from '$lib/protocol/pause.js';

  let {
    connection,
    sessionID,
    canControl = false
  }: {
    /** The live Runtime connection — the dock builds a session-scoped client. */
    connection: RuntimeConnection;
    /** The session whose event stream the dock projects. */
    sessionID: string;
    /** Whether the operator holds the control scope (D-066) — gates Resume. */
    canControl?: boolean;
  } = $props();

  // The dock subscribes to the session's lifecycle / control / cost /
  // intervention / audit / runtime events — the union the five tabs
  // render. Enumerated (not empty) so the named-SSE-frame listeners
  // register (subscription.svelte.ts) and the URL stays bounded.
  const DOCK_TYPES = EVENT_TYPES.filter(
    (t) =>
      t.startsWith('planner.') ||
      t.startsWith('tool.') ||
      t.startsWith('task.') ||
      t.startsWith('control.') ||
      t.startsWith('pause.') ||
      t.startsWith('session.') ||
      t.startsWith('audit.') ||
      t.startsWith('runtime.') ||
      t === 'llm.cost.recorded'
  );

  // A session-scoped client so the SSE filters server-side to this
  // session (the runtime reads `id.SessionID` from the ?session param —
  // stream.go) and `pause.list` defaults to the session's pauses.
  // Reactive on (connection, sessionID): SvelteKit reuses the `[id]`
  // page across param changes, so the dock must re-scope when the route
  // session changes (a stale subscription would tail the old session).
  const client = $derived(
    new HarborClient({
      connection: { ...connection, identity: { ...connection.identity, session: sessionID } }
    })
  );

  // ---- the live subscription (re-opened when the session changes) ----
  let sub = $state<EventsSubscription | null>(null);

  // ---- pending interventions (pause.list backfill) ----
  let pending = $state<PauseSnapshot[]>([]);
  let actionBusy = $state<string | null>(null);
  let actionResult = $state<Map<string, string>>(new Map());

  // ---- derived, session-scoped tab views ----
  const events = $derived(sub?.events ?? []);
  const streamState = $derived(sub?.state ?? 'idle');
  // Defensive client-side filter on top of the server-side session scope.
  const sessionEvents = $derived(events.filter((e) => e.session === sessionID));
  const trajectory = $derived(projectTrajectory(sessionEvents));
  const costRollup = $derived(projectCost(sessionEvents, 'model'));
  const controlEvents = $derived(sessionEvents.filter((e) => e.type.startsWith('control.')));
  const interventionEvents = $derived(
    sessionEvents.filter(
      (e) =>
        e.type.startsWith('pause.') ||
        e.type.startsWith('tool.approval_') ||
        e.type.startsWith('tool.auth_')
    )
  );

  const TABS = [
    { key: 'trajectory', label: 'Trajectory' },
    { key: 'events', label: 'Events' },
    { key: 'cost', label: 'Cost History' },
    { key: 'control', label: 'Control History' },
    { key: 'interventions', label: 'Interventions' }
  ];
  let active = $state('trajectory');

  /** Loads the still-pending pauses for the session (`pause.list`). */
  async function loadPending(): Promise<void> {
    try {
      const resp = await client.pause.list({
        filter: { session_ids: [sessionID], status: ['paused'] }
      });
      pending = resp.snapshots ?? [];
    } catch {
      // Best-effort backfill — the live `pause.*` events still render.
      pending = [];
    }
  }

  /** Resolves a pending pause via the shipped `approve` / `reject` verbs. */
  async function resolve(snap: PauseSnapshot, verb: 'approve' | 'reject'): Promise<void> {
    const run = snap.identity.run ?? '';
    if (run === '' || actionBusy !== null) return;
    actionBusy = snap.token;
    try {
      if (verb === 'approve') await client.control.approve(run);
      else await client.control.reject(run);
      const m = new Map(actionResult);
      m.set(snap.token, verb === 'approve' ? 'approved' : 'rejected');
      actionResult = m;
      void loadPending();
    } catch (err) {
      const m = new Map(actionResult);
      m.set(snap.token, err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err));
      actionResult = m;
    } finally {
      actionBusy = null;
    }
  }

  /** Exports the session's loaded events as JSONL (Console-local — D-061). */
  function exportEvents(): void {
    const meta = exportMeta('ndjson');
    triggerDownload(
      `session-${sessionID}-events.${meta.ext}`,
      meta.mime,
      exportEventsNDJSON(sessionEvents)
    );
  }

  // Open a fresh subscription whenever the session-scoped client changes
  // (session change or first mount); close it on teardown / re-scope.
  $effect(() => {
    const s = new EventsSubscription(client.events);
    s.open({ eventTypes: DOCK_TYPES as string[] });
    sub = s;
    pending = [];
    actionResult = new Map();
    void loadPending();
    return () => s.close();
  });
</script>

<section class="dock" data-testid="bottom-dock">
  <div class="tab-strip" role="tablist" aria-label="Session detail tabs">
    {#each TABS as tab (tab.key)}
      <button
        type="button"
        role="tab"
        class="tab"
        class:active={active === tab.key}
        aria-selected={active === tab.key}
        data-testid={`dock-tab-${tab.key}`}
        onclick={() => (active = tab.key)}
      >
        {tab.label}
      </button>
    {/each}
    <span class="stream-state" data-testid="dock-stream-state" data-state={streamState}>
      {streamState === 'open' ? '● live' : streamState === 'connecting' ? '○ connecting' : streamState}
    </span>
  </div>

  <div class="tab-panel" role="tabpanel" data-testid="dock-panel">
    {#if active === 'trajectory'}
      {#if trajectory.length === 0}
        <p class="empty" data-testid="dock-empty-trajectory">
          No planner steps for this session in the live stream.
        </p>
      {:else}
        <ol class="timeline" data-testid="trajectory-list">
          {#each trajectory as step (step.sequence)}
            <li class="step">
              <span class="marker" data-kind={step.kind}></span>
              <div class="step-body">
                <div class="step-head">
                  <span class="step-label">{step.label}</span>
                  <span class="step-age">{formatRelative(step.occurred_at)}</span>
                </div>
                {#if step.detail}<p class="step-detail">{step.detail}</p>{/if}
              </div>
            </li>
          {/each}
        </ol>
      {/if}
    {:else if active === 'events'}
      <div class="panel-head">
        <span class="panel-count">{sessionEvents.length} events</span>
        <button
          type="button"
          class="mini"
          data-testid="dock-export-events"
          disabled={sessionEvents.length === 0}
          onclick={exportEvents}
        >
          Export JSONL
        </button>
      </div>
      {#if sessionEvents.length === 0}
        <p class="empty" data-testid="dock-empty-events">
          No events for this session in the live stream.
        </p>
      {:else}
        <ul class="event-log" data-testid="events-list">
          {#each sessionEvents as ev (ev.sequence)}
            <li class="event-row">
              <StatusChip kind={categoryKind(categoryOf(ev.type))} label={categoryOf(ev.type)} />
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'cost'}
      <div class="panel-head">
        <span class="panel-count">${costRollup.totalUSD.toFixed(4)} total</span>
      </div>
      {#if costRollup.rows.length === 0}
        <p class="empty" data-testid="dock-empty-cost">
          No <code>llm.cost.recorded</code> events for this session in the live stream.
        </p>
      {:else}
        <table class="cost-table" data-testid="cost-list">
          <thead>
            <tr><th>Model</th><th class="num">Calls</th><th class="num">Cost (USD)</th></tr>
          </thead>
          <tbody>
            {#each costRollup.rows as r (r.key)}
              <tr>
                <td class="mono">{r.key}</td>
                <td class="num">{r.events}</td>
                <td class="num mono">${r.costUSD.toFixed(4)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    {:else if active === 'control'}
      {#if controlEvents.length === 0}
        <p class="empty" data-testid="dock-empty-control">
          No control instructions recorded for this session in the live stream.
        </p>
      {:else}
        <ul class="event-log" data-testid="control-list">
          {#each controlEvents as ev (ev.sequence)}
            <li class="event-row">
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {:else if active === 'interventions'}
      {#if pending.length > 0}
        <ul class="pending-list" data-testid="interventions-pending">
          {#each pending as snap (snap.token)}
            {@const result = actionResult.get(snap.token)}
            <li class="pending-row">
              <div class="pending-meta">
                <span class="pending-reason">{snap.reason}</span>
                <span class="event-age">{formatRelative(snap.paused_at)}</span>
              </div>
              <div class="pending-actions">
                {#if result}
                  <span class="action-result" data-testid="intervention-result">{result}</span>
                {:else}
                  <button
                    type="button"
                    class="mini"
                    data-testid="intervention-approve"
                    disabled={!canControl || actionBusy === snap.token || (snap.identity.run ?? '') === ''}
                    title={canControl ? 'Resume this run (approve)' : 'Requires the control-plane scope claim (D-066)'}
                    onclick={() => void resolve(snap, 'approve')}
                  >
                    Resume
                  </button>
                  <button
                    type="button"
                    class="mini"
                    data-testid="intervention-reject"
                    disabled={!canControl || actionBusy === snap.token || (snap.identity.run ?? '') === ''}
                    title={canControl ? 'Reject this run' : 'Requires the control-plane scope claim (D-066)'}
                    onclick={() => void resolve(snap, 'reject')}
                  >
                    Reject
                  </button>
                {/if}
              </div>
            </li>
          {/each}
        </ul>
      {/if}
      {#if interventionEvents.length === 0 && pending.length === 0}
        <p class="empty" data-testid="dock-empty-interventions">
          No interventions recorded for this session.
        </p>
      {:else if interventionEvents.length > 0}
        <ul class="event-log" data-testid="interventions-list">
          {#each interventionEvents as ev (ev.sequence)}
            <li class="event-row">
              <span class="event-type">{ev.type}</span>
              <span class="event-age">{formatRelative(ev.occurred_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    {/if}
  </div>
</section>

<style>
  .dock {
    display: flex;
    flex-direction: column;
    flex: 1;
    gap: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    background: var(--color-surface);
    min-height: 0;
  }

  .tab-strip {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
    padding: var(--space-2) var(--space-2) var(--space-0);
  }

  .tab {
    background: none;
    color: var(--color-text-muted);
    border: none;
    border-bottom: var(--border-hairline);
    border-bottom-color: transparent;
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .tab.active {
    color: var(--color-text);
    border-bottom-color: var(--color-accent);
    font-weight: 600;
  }

  .stream-state {
    margin-left: auto;
    padding-right: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .tab-panel {
    flex: 1;
    min-height: 0;
    padding: var(--space-3);
    overflow-y: auto;
  }

  .panel-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    margin-bottom: var(--space-2);
  }

  .panel-count {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .timeline {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .step {
    display: flex;
    gap: var(--space-3);
    align-items: flex-start;
  }

  .marker {
    margin-top: var(--space-1);
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    background: var(--color-text-muted);
    flex-shrink: 0;
  }

  .marker[data-kind='planner'] {
    background: var(--color-accent);
  }

  .marker[data-kind='tool'] {
    background: var(--color-success);
  }

  .marker[data-kind='task'] {
    background: var(--color-warning);
  }

  .step-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }

  .step-head {
    display: flex;
    gap: var(--space-3);
    align-items: baseline;
  }

  .step-label {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--color-text);
  }

  .step-age,
  .event-age {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .step-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .event-log,
  .pending-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .event-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-0);
  }

  .event-type {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .event-age {
    margin-left: auto;
  }

  .cost-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .cost-table th,
  .cost-table td {
    text-align: left;
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .cost-table .num {
    text-align: right;
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .pending-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .pending-meta {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .pending-reason {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .pending-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .action-result {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .mini {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .mini:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
