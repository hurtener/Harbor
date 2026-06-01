<script lang="ts" module>
  // Harbor Console — Live Runtime cockpit "Active sessions" panel
  // (Phase 108e / D-177).
  //
  // A thin drill INDEX of live sessions on the connected runtime — NOT a
  // re-implementation of the Sessions page (D-062 boundary). Its data:
  //   - the active-session COUNT from `runtime.counters().sessions_active`
  //     (a shipped low-cardinality rollup);
  //   - a recent-sessions list folded CLIENT-SIDE from `session.opened` /
  //     `session.closed` events (there is no runtime-scoped `sessions.list`
  //     surface yet — an honest event-derived partial; flagged in the §8
  //     ledger). No fabrication: an empty stream renders the honest empty
  //     state (CLAUDE.md §13).
  // Each row drills to `/sessions` (the catalog) or `/playground` (steer a
  // conversation) — run-level steering lives in Playground (D-062), never a
  // free-floating composer here.
  //
  // The panel also hosts the session-detail card binding for the connected
  // session (the W10 derived-status surface) so the rail's status reads the
  // derived strip label, not the page status.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  /** One event-derived recent-session row. */
  export interface RecentSession {
    /** The session id. */
    session: string;
    /** The lifecycle state derived from the last event for the session. */
    state: 'open' | 'closed';
    /** The RFC-3339 timestamp of the latest observed event for the session. */
    at: string;
  }
</script>

<script lang="ts">
  import SessionDetailCard from '$lib/components/live-runtime/session-detail-card.svelte';
  import type { ConnectionIdentity } from '$lib/connection.js';

  let {
    activeCount,
    sessions,
    identity,
    sessionStatusLabel,
    costUSD,
    lastError
  }: {
    /** The active-session count from `runtime.counters().sessions_active`. */
    activeCount: number;
    /** The event-folded recent sessions (newest-first). */
    sessions: RecentSession[];
    /** The connected session's identity triple, or null when disconnected. */
    identity: ConnectionIdentity | null;
    /** The derived session-level status label (from the strip, W10). */
    sessionStatusLabel: string;
    /** The aggregated session cost in USD (folded from `llm.cost.recorded`). */
    costUSD: number;
    /** The most recent failure summary, or null. */
    lastError: string | null;
  } = $props();
</script>

<div class="active-sessions" data-testid="active-sessions-panel">
  <div class="count-row">
    <span class="count" data-testid="active-sessions-count">{activeCount}</span>
    <span class="count-label">active now</span>
  </div>

  {#if identity !== null}
    <SessionDetailCard
      identity={identity}
      agentName="default agent"
      sessionStatus={sessionStatusLabel}
      costUSD={costUSD}
      lastError={lastError}
      tenant={identity.tenant}
    />
  {/if}

  {#if sessions.length > 0}
    <ul class="session-list">
      {#each sessions as s (s.session)}
        <li class="session-row" data-testid="active-session-row" data-state={s.state}>
          <a class="session-link" href={`/sessions`}>{s.session}</a>
          <span class="session-state">{s.state}</span>
          <a class="steer" href={`/playground`} title="Steer this session in the Playground">
            Steer
          </a>
        </li>
      {/each}
    </ul>
  {:else}
    <p class="sessions-empty" data-testid="active-sessions-empty">
      No recent session activity observed on this runtime yet.
    </p>
  {/if}
</div>

<style>
  .active-sessions {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .count-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .count {
    font-size: var(--text-xl);
    font-weight: 600;
    color: var(--color-text);
  }

  .count-label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .session-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .session-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .session-link {
    font-family: var(--font-mono);
    color: var(--color-accent);
    text-decoration: none;
    overflow-wrap: anywhere;
  }

  .session-state {
    color: var(--color-text-muted);
    margin-left: auto;
  }

  .steer {
    color: var(--color-text-muted);
    text-decoration: none;
  }

  .sessions-empty {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
