<script lang="ts">
  // Harbor Console — Session detail page (`/sessions/<id>`), Phase 73c /
  // D-122, built on the design-system foundation (D-121).
  //
  // The detail-mode view for one session: a detail header card + the
  // bottom-dock tab strip (Trajectory | Events | Cost History | Control
  // History | Interventions) + a `DetailRail` carrying the Session
  // Summary, Recent Interventions, and Recent Artifacts cards. Sourced
  // entirely from `sessions.inspect` (CLAUDE.md §13 — never runtime
  // internals).
  //
  // # Console consistency (CONVENTIONS.md)
  //
  // - Routes under `(console)/sessions/[id]` — the `[id]` segment is the
  //   uniform detail-route name (§1); no `/console/` URL prefix.
  // - Renders inside the app shell (§2).
  // - Composes the `ui/` inventory: `PageHeader`, `DetailRail`/`RailCard`,
  //   `StatusChip`, `PageState` (§3/§4). Sessions-specific components
  //   (`SessionSummaryCard`, `RecentInterventionsCard`,
  //   `RecentArtifactsCard`, `BottomDockTabs`) stay in
  //   `components/sessions/` (§3).
  // - Routes async state through the four-state `<PageState>` (§4).
  // - Talks to the Runtime only through `HarborClient` + `connection.ts`
  //   (§6) — no hand-rolled `fetch`. Design tokens only (§7).
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { HarborClient } from '$lib/protocol/harbor.js';
  import { ProtocolError } from '$lib/protocol/errors.js';
  import { SessionsProtocol } from '$lib/protocol/sessions.js';
  import { resolveConnection } from '$lib/connection.js';
  import {
    PageHeader,
    DetailRail,
    RailCard,
    StatusChip,
    PageState,
    type PageStatus
  } from '$lib/components/ui/index.js';
  import SessionSummaryCard from '$lib/components/sessions/SessionSummaryCard.svelte';
  import RecentInterventionsCard from '$lib/components/sessions/RecentInterventionsCard.svelte';
  import RecentArtifactsCard from '$lib/components/sessions/RecentArtifactsCard.svelte';
  import BottomDockTabs from '$lib/components/sessions/BottomDockTabs.svelte';
  import IdentityCell from '$lib/components/sessions/IdentityCell.svelte';
  import type { SessionsInspectResponse } from '$lib/sessions/types.js';
  import {
    formatCostCents,
    formatDurationNS,
    formatRelative,
    statusKind
  } from '$lib/sessions/format.js';

  // ---- Connection + typed client (CONVENTIONS.md §6) ----
  const connection = resolveConnection();
  const sessionsClient =
    connection !== null ? new SessionsProtocol(new HarborClient({ connection })) : null;

  const sessionID = $derived(page.params.id ?? '');

  // ---- Async-state model (CONVENTIONS.md §4) ----
  let status = $state<PageStatus>(connection === null ? 'disconnected' : 'loading');
  let loadError = $state<ProtocolError | null>(null);
  let snapshot = $state<SessionsInspectResponse | null>(null);

  /** Loads `sessions.inspect` for the routed session id. */
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
      status = 'ready';
    } catch (err) {
      snapshot = null;
      loadError =
        err instanceof ProtocolError
          ? err
          : new ProtocolError('runtime_error', String(err), 0);
      status = 'error';
    }
  }

  /** Copies the full session id to the clipboard. */
  async function copyID(): Promise<void> {
    try {
      await navigator.clipboard.writeText(sessionID);
    } catch {
      // Clipboard denied (no permission / insecure context) — non-fatal;
      // the operator can still select the visible id text.
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
  <PageHeader title="Session detail" subtitle={sessionID}>
    {#snippet actions()}
      <button
        type="button"
        class="ghost"
        data-testid="session-detail-back"
        onclick={() => void goto('/sessions')}
      >
        ← Back to list
      </button>
    {/snippet}
  </PageHeader>

  <PageState {status} error={loadError} onretry={() => void loadSnapshot()}>
    {#snippet empty()}
      <p class="headline">Session not found</p>
      <p class="detail">
        No session with id <code>{sessionID}</code> is visible in your scope.
      </p>
    {/snippet}

    {#if snapshot}
      <div class="detail-grid">
        <div class="main">
          <div class="header-card" data-testid="session-detail-header">
            <div class="header-line">
              <span class="mono id" title={snapshot.row.session_id}>
                {snapshot.row.session_id}
              </span>
              <button
                type="button"
                class="ghost small"
                data-testid="session-copy-id"
                onclick={() => void copyID()}
              >
                Copy id
              </button>
              <StatusChip kind={statusKind(snapshot.row.status)} label={snapshot.row.status} />
            </div>
            <div class="header-meta">
              <span>Started {formatRelative(snapshot.row.started_at)}</span>
              <span>Duration {formatDurationNS(snapshot.row.duration)}</span>
              <span>{snapshot.row.tasks_count} tasks · {snapshot.row.events_count} events</span>
              <span>{formatCostCents(snapshot.row.total_cost_cents)}</span>
            </div>
            <div class="header-identity">
              <IdentityCell identity={snapshot.row.identity} />
            </div>
          </div>

          <BottomDockTabs />
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
  .session-detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .detail-grid {
    display: grid;
    grid-template-columns: 1fr var(--size-rail);
    gap: var(--space-4);
    align-items: start;
  }

  .main {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .header-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
  }

  .header-line {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .header-meta {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .mono,
  .id {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .ghost.small {
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
  }
</style>
