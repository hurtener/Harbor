<script lang="ts">
  // Harbor Console — Session detail page (`/sessions/<id>`) — Phase 108g
  // rebuild (D-179; supersedes the Phase 73c / D-122 placeholder dock).
  //
  // The detail-mode view for one session: a carded header (id + copy +
  // status + counts + the real action set) + a right rail (Session
  // Summary, Recent Interventions, Recent Artifacts from `sessions.inspect`)
  // + the five-tab bottom dock, which renders REAL session-filtered event
  // data (BottomDockTabs — D-179). Rebuilt to the carded `.panel.card`
  // vocabulary the four done pages set.
  //
  // Action set — all real-wired (PAGE-POLISH §4):
  //   - Continue in Live Runtime ← local nav (re-attach the same session)
  //   - Cancel session           ← `cancel` per active run (tasks.list resolve)
  //   - Clone                    ← `start` seeded from the session's root-task
  //                                description (best-effort; disabled when the
  //                                input is not recoverable on this runtime)
  //   - Copy id                  ← clipboard
  //   - Convert to Evaluation    ← disabled w/ tooltip (D-064, post-V1)
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only (CONVENTIONS.md §6).
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { SessionsProtocol } from '$lib/protocol/sessions.js';
  import { resolveConnection, hasScope } from '$lib/connection.js';
  import { DetailRail, RailCard, StatusChip, PageState, type PageStatus } from '$lib/components/ui/index.js';
  import SessionSummaryCard from '$lib/components/sessions/SessionSummaryCard.svelte';
  import RecentInterventionsCard from '$lib/components/sessions/RecentInterventionsCard.svelte';
  import RecentArtifactsCard from '$lib/components/sessions/RecentArtifactsCard.svelte';
  import BottomDockTabs from '$lib/components/sessions/BottomDockTabs.svelte';
  import IdentityCell from '$lib/components/sessions/IdentityCell.svelte';
  import type { SessionsInspectResponse } from '$lib/sessions/types.js';
  import type { TaskRow } from '$lib/protocol/tasks.js';
  import { formatDurationNS, formatRelative, statusKind } from '$lib/sessions/format.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const harborClient = connection !== null ? new HarborClient({ connection }) : null;
  const sessionsClient = harborClient !== null ? new SessionsProtocol(harborClient) : null;
  const canControl = hasScope(connection, 'admin');

  const sessionID = $derived(page.params.id ?? '');

  // ---- Async-state model (CONVENTIONS.md §4) ----
  let status = $state<PageStatus>(connection === null ? 'disconnected' : 'loading');
  let loadError = $state<ProtocolError | null>(null);
  let snapshot = $state<SessionsInspectResponse | null>(null);

  // ---- Header action feedback ----
  let cloneSeed = $state<string | null>(null);
  let actionBusy = $state(false);
  let actionResult = $state<string | null>(null);

  /** Loads `sessions.inspect` for the routed session id, then enriches counts. */
  async function loadSnapshot(): Promise<void> {
    if (!sessionsClient) {
      status = 'disconnected';
      return;
    }
    if (sessionID === '') {
      status = 'error';
      loadError = new ProtocolError('invalid_request', 'no session id in the URL', 0);
      return;
    }
    status = 'loading';
    loadError = null;
    try {
      snapshot = await sessionsClient.inspect(sessionID);
      // D-122: `sessions.inspect` returns zero aggregates by design (the
      // registry doesn't model counts). Enrich tasks_count + events_count
      // from the shipped `tasks.list` + `events.aggregate` wires. A
      // failure on the enrichment leg leaves the wire zeros in place.
      await enrichSessionCounters();
      status = 'ready';
    } catch (err) {
      snapshot = null;
      loadError = err instanceof ProtocolError ? err : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
  }

  /**
   * Console-side counter enrichment (D-122). Folds:
   *   - tasks_count + ACTIVE duration from a session-scoped `tasks.list`
   *     (Σ of the session's run `duration_ms`) — active processing time,
   *     NOT wall-clock from open to now (mirrors the Playground's
   *     `activeWorkMs`). `tasks.list` is session-scoped via the
   *     X-Harbor-Session header (D-171), so a session-scoped client is
   *     required — the connection-default client returns the WRONG
   *     session's runs (the prior "0 tasks" bug).
   *   - events_count from `events.aggregate` (sum over a 30d window).
   * Also records the session's root-task description as the Clone seed.
   * total_tokens / total_cost_cents stay at the wire zero — no shipped
   * aggregate sums `llm.cost.recorded` per session (D-179); the Cost
   * History dock tab computes cost from the live stream instead.
   */
  async function enrichSessionCounters(): Promise<void> {
    if (harborClient === null || snapshot === null || connection === null) return;
    try {
      const scoped = new HarborClient({
        connection: { ...connection, identity: { ...connection.identity, session: sessionID } }
      });
      const taskResp = await scoped.tasks.list<{ rows: TaskRow[] }>({});
      const tasks = taskResp.rows ?? [];
      const root = tasks.find((t) => t.kind === 'foreground') ?? tasks[0];
      cloneSeed = root?.description?.trim() || null;
      // Active duration = Σ per-run duration_ms (ms → ns for formatDurationNS).
      const activeNs = tasks.reduce((sum, t) => sum + (t.duration_ms ?? 0), 0) * 1_000_000;
      snapshot = { ...snapshot, row: { ...snapshot.row, tasks_count: tasks.length, duration: activeNs } };
    } catch {
      // best-effort; leave the wire zero + no clone seed
    }
    try {
      const thirtyDaysNS = 30 * 24 * 60 * 60 * 1_000_000_000;
      const evtResp = await harborClient.events.aggregate({
        filter: { session_ids: [sessionID] },
        window: thirtyDaysNS,
        bucket: thirtyDaysNS
      });
      let totalEvents = 0;
      for (const b of evtResp.buckets ?? []) {
        for (const v of Object.values(b.counts ?? {})) {
          totalEvents += v ?? 0;
        }
      }
      if (snapshot !== null) {
        snapshot = { ...snapshot, row: { ...snapshot.row, events_count: totalEvents } };
      }
    } catch {
      // same best-effort posture as the tasks leg
    }
  }

  /** Copies the full session id to the clipboard. */
  async function copyID(): Promise<void> {
    try {
      await navigator.clipboard.writeText(sessionID);
    } catch {
      // clipboard denied (no permission / insecure context) — non-fatal
    }
  }

  /** Re-attaches the session in the Live Runtime workbench (local nav). */
  function continueInLiveRuntime(): void {
    void goto(`/live-runtime?session=${encodeURIComponent(sessionID)}`);
  }

  /**
   * Cancels the session by cancelling each of its active runs — iterates
   * the shipped `cancel` control verb (D-047) over the session's running
   * / paused tasks resolved via `tasks.list` (a control verb targets a
   * run, not a session). Control-scope gated (D-066).
   */
  async function cancelSession(): Promise<void> {
    if (harborClient === null || actionBusy) return;
    actionBusy = true;
    actionResult = null;
    try {
      const resp = await harborClient.tasks.list<{ rows: TaskRow[] }>({});
      const live = (resp.rows ?? []).filter(
        (t) => (t.identity?.session ?? t.parent_session_id) === sessionID && (t.status === 'running' || t.status === 'paused')
      );
      let ok = 0;
      for (const t of live) {
        try {
          await harborClient.control.cancel(t.id);
          ok += 1;
        } catch {
          // continue cancelling the rest
        }
      }
      actionResult = live.length === 0 ? 'No active runs to cancel.' : `Cancelled ${ok} of ${live.length} run${live.length === 1 ? '' : 's'}.`;
      void loadSnapshot();
    } catch (err) {
      actionResult = err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err);
    } finally {
      actionBusy = false;
    }
  }

  /**
   * Clones the session — spawns a fresh `start` (D-047) seeded from the
   * session's root-task description, then navigates to the new run in
   * Live Runtime. Disabled when the original input is not recoverable on
   * this runtime (the faithful clone-from-history source is the Phase 73
   * `state.history` surface, still Pending) — never a fabricated input.
   */
  async function cloneSession(): Promise<void> {
    if (harborClient === null || cloneSeed === null || actionBusy) return;
    actionBusy = true;
    actionResult = null;
    try {
      const resp = await harborClient.control.start<{ task_id: string }>(cloneSeed, {
        description: `Clone of ${sessionID}`
      });
      actionResult = `Cloned → task ${resp.task_id}`;
    } catch (err) {
      actionResult = err instanceof ProtocolError ? `${err.code}: ${err.message}` : String(err);
    } finally {
      actionBusy = false;
    }
  }

  $effect(() => {
    void loadSnapshot();
  });
</script>

<svelte:head>
  <title>Session {sessionID} · Harbor Console</title>
</svelte:head>

<section class="session-detail" data-testid="session-detail-page">
  <PageState {status} error={loadError} onretry={() => void loadSnapshot()}>
    {#snippet empty()}
      <p class="empty-headline">Session not found</p>
      <p class="empty-detail">No session with id <code>{sessionID}</code> is visible in your scope.</p>
      <button type="button" class="control" data-testid="session-detail-back" onclick={() => void goto('/sessions')}>
        ← Back to list
      </button>
    {/snippet}

    {#if snapshot}
      <div class="detail-grid">
        <div class="main">
          <section class="panel card" data-testid="session-detail-header">
            <header class="header-line">
              <span class="mono id" title={snapshot.row.session_id}>{snapshot.row.session_id}</span>
              <button type="button" class="control small" data-testid="session-copy-id" onclick={() => void copyID()}>
                Copy id
              </button>
              <StatusChip kind={statusKind(snapshot.row.status)} label={snapshot.row.status} />
            </header>
            <div class="header-meta">
              <span>Started {formatRelative(snapshot.row.started_at)}</span>
              <span title="Active processing time — sum of run durations, not wall-clock">
                Duration {formatDurationNS(snapshot.row.duration)}
              </span>
              <span>{snapshot.row.tasks_count} tasks · {snapshot.row.events_count} events</span>
              <IdentityCell identity={snapshot.row.identity} />
            </div>
            <div class="header-actions">
              <button type="button" class="control" data-testid="session-continue" onclick={continueInLiveRuntime}>
                Continue in Live Runtime
              </button>
              <button
                type="button"
                class="control"
                data-testid="session-clone"
                disabled={cloneSeed === null || actionBusy}
                title={cloneSeed === null
                  ? 'Original input not recoverable on this runtime (needs the Phase 73 state.history surface)'
                  : 'Spawn a fresh run seeded from this session’s input'}
                onclick={() => void cloneSession()}
              >
                Clone
              </button>
              <button
                type="button"
                class="control"
                data-testid="session-cancel"
                disabled={!canControl || actionBusy || snapshot.row.status !== 'running'}
                title={!canControl
                  ? 'Requires the control-plane scope claim (D-066)'
                  : snapshot.row.status !== 'running'
                    ? 'Only a running session can be cancelled'
                    : 'Cancel every active run in this session'}
                onclick={() => void cancelSession()}
              >
                Cancel session
              </button>
              <button type="button" class="control" data-testid="session-convert-eval" disabled title="Evaluations is post-V1 (D-064)">
                Convert to Evaluation
              </button>
              {#if actionResult}
                <span class="action-result" data-testid="session-action-result">{actionResult}</span>
              {/if}
            </div>
          </section>

          {#if connection}
            <BottomDockTabs {connection} {sessionID} {canControl} />
          {/if}
        </div>

        <DetailRail>
          <RailCard title="Session Summary">
            <SessionSummaryCard row={snapshot.row} />
          </RailCard>
          <RailCard title="Recent Interventions">
            <RecentInterventionsCard interventions={snapshot.recent_interventions} />
          </RailCard>
          <RailCard title="Recent Artifacts">
            <RecentArtifactsCard artifacts={snapshot.recent_artifacts} />
          </RailCard>
        </DetailRail>
      </div>
    {/if}
  </PageState>
</section>

<style>
  /* Viewport-locked: the page fills the shell content region and never
     full-page-scrolls. The dock's tab-panel and the rail scroll
     internally instead (PAGE-POLISH §6 — the Playground pattern). */
  .session-detail {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    padding: var(--space-4);
    overflow: hidden;
  }

  .detail-grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    flex: 1;
    min-height: 0;
    align-items: stretch;
  }

  .main {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
    min-height: 0;
  }

  .card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }

  .header-line {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .header-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .header-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .action-result {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .mono,
  .id {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
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
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .control {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .control.small {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }

  .control:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
</style>
