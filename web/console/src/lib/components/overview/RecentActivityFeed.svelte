<script lang="ts">
  // Harbor Console — Overview recent-activity feed (Phase 73a / D-127).
  //
  // The full-width panel in canvas row 4 (page-overview.md §4). It
  // renders the projected `ActivityRow[]` (from `$lib/overview/activity.ts`
  // — pure, folded off the SHIPPED `events.subscribe` cursor; NO new
  // Protocol method). Each row is a typed-event icon glyph + a
  // session-id chip + a free-text description + a relative timestamp
  // (page-overview.md §12 — "Refinements to §3"), and is a deep-link
  // into that entity's detail page.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import { relativeTime, type ActivityRow } from '$lib/overview/activity.js';

  let {
    rows,
    now
  }: {
    /** The projected, newest-first activity rows. */
    rows: ActivityRow[];
    /** The reference clock for relative timestamps, unix millis. */
    now: number;
  } = $props();
</script>

<div class="activity-feed" data-testid="recent-activity-feed">
  {#if rows.length === 0}
    <p class="empty" data-testid="recent-activity-empty">
      Waiting for runtime activity — session opens, task completions, and agent
      restarts will appear here.
    </p>
  {:else}
    <ul class="rows">
      {#each rows as row (row.sequence)}
        <li class="row">
          <a class="row-link" href={row.href} data-testid="activity-row">
            <span class="glyph" data-severity={row.severity}>{row.glyph}</span>
            <span class="desc">{row.description}</span>
            <span class="session-chip">
              <StatusChip kind="neutral" label={row.session} />
            </span>
            <span class="ago">{relativeTime(row.occurredAt, now)}</span>
          </a>
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .activity-feed {
    display: flex;
    flex-direction: column;
  }

  .empty {
    margin: var(--space-0);
    padding: var(--space-8) var(--space-4);
    text-align: center;
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .rows {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
  }

  .row {
    border-bottom: var(--border-hairline);
  }

  .row:last-child {
    border-bottom: none;
  }

  .row-link {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-1);
    text-decoration: none;
    color: var(--color-text);
  }

  .row-link:hover {
    background: var(--color-surface-raised);
  }

  .glyph {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--space-6);
    height: var(--space-6);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-weight: 600;
    background: var(--color-surface-raised);
  }

  .glyph[data-severity='success'] {
    color: var(--color-success);
    background: var(--color-success-soft);
  }

  .glyph[data-severity='danger'] {
    color: var(--color-danger);
    background: var(--color-danger-soft);
  }

  .glyph[data-severity='warning'] {
    color: var(--color-warning);
    background: var(--color-warning-soft);
  }

  .glyph[data-severity='accent'] {
    color: var(--color-accent);
    background: var(--color-accent-soft);
  }

  .desc {
    flex: 1;
    font-size: var(--text-sm);
  }

  .session-chip {
    display: inline-flex;
  }

  .ago {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    font-family: var(--font-mono);
    white-space: nowrap;
  }
</style>
