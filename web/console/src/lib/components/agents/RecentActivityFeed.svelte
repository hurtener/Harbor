<script lang="ts">
  // Harbor Console — Agents detail recent-activity feed (Phase 73e /
  // D-124). The agent's last N `agent.*` lifecycle events (registered /
  // restarted / health / drained / deregistered / paused /
  // restart_requested / force_stopped).
  //
  // # Honest empty state (CLAUDE.md §13)
  //
  // The activity feed is fed by an `events.subscribe` stream filtered to
  // this agent's events — a streaming surface a later wave wires into the
  // detail route. Until that subscription is wired, the feed renders an
  // HONEST "no recent activity" empty state; it is NOT faked with
  // synthetic rows. The `entries` prop is the seam the streaming consumer
  // fills.

  /** One projected `agent.*` lifecycle event row. */
  export interface ActivityEntry {
    /** The event type, e.g. `agent.registered`. */
    type: string;
    /** RFC3339 timestamp of the event. */
    at: string;
    /** A short operator-facing summary. */
    summary: string;
  }

  let { entries }: { entries: ActivityEntry[] } = $props();
</script>

<div class="activity-feed" data-testid="agent-activity-feed">
  {#if entries.length === 0}
    <p class="empty">
      No recent activity. Agent lifecycle events surface here as they
      stream in.
    </p>
  {:else}
    <ul>
      {#each entries as entry, i (i)}
        <li data-testid="agent-activity-row">
          <span class="event-type mono">{entry.type}</span>
          <span class="event-summary">{entry.summary}</span>
          <span class="event-time">{entry.at}</span>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .activity-feed {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  li {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-0);
    border-bottom: var(--border-hairline);
  }

  .event-type {
    font-size: var(--text-xs);
    color: var(--color-accent);
  }

  .event-summary {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .event-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
