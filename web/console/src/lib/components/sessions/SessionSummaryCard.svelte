<script lang="ts">
  // Harbor Console — Sessions detail-view right-rail Session Summary
  // card (Phase 73c / D-122; Phase 108g / D-179 dropped the always-zero
  // cost row). Renders the additive `sessions.inspect` projection: id /
  // status / started / duration / events / tasks / agent / user / tenant
  // / last activity.
  //
  // No cost / tokens row: the Phase 08 registry does not model per-session
  // cost (D-122) and no shipped aggregate sums `llm.cost.recorded` per
  // session (D-179) — showing a structural "$0.00" next to the dock's
  // real Cost History tab would be misleading. Cost is surfaced in the
  // Cost History tab (live SSE sum) instead of a fabricated zero here.
  //
  // Sourced exclusively from `sessions.inspect` — the card NEVER reads
  // runtime internals (CONVENTIONS.md / CLAUDE.md §13). Sessions-specific
  // component — lives in `components/sessions/`. Svelte 5 runes (D-092);
  // design tokens only.
  import type { SessionRow } from '$lib/sessions/types.js';
  import { formatDurationNS, formatRelative, statusKind } from '$lib/sessions/format.js';
  import { StatusChip } from '$lib/components/ui/index.js';

  let { row }: { row: SessionRow } = $props();
</script>

<dl class="summary" data-testid="session-summary">
  <div class="field">
    <dt>Session</dt>
    <dd class="mono" title={row.session_id}>{row.session_id}</dd>
  </div>
  <div class="field">
    <dt>Status</dt>
    <dd><StatusChip kind={statusKind(row.status)} label={row.status} /></dd>
  </div>
  <div class="field">
    <dt>Started</dt>
    <dd>{formatRelative(row.started_at)}</dd>
  </div>
  <div class="field">
    <dt title="Active processing time — sum of run durations, not wall-clock">Duration</dt>
    <dd>{formatDurationNS(row.duration)}</dd>
  </div>
  <div class="field">
    <dt>Events</dt>
    <dd>{row.events_count}</dd>
  </div>
  <div class="field">
    <dt>Tasks</dt>
    <dd>{row.tasks_count}</dd>
  </div>
  <div class="field">
    <dt>Agent</dt>
    <dd>{row.agent_name || '—'}</dd>
  </div>
  <div class="field">
    <dt>User</dt>
    <dd>{row.user_id}</dd>
  </div>
  <div class="field">
    <dt>Tenant</dt>
    <dd>{row.tenant_id}</dd>
  </div>
  <div class="field">
    <dt>Cost</dt>
    <dd class="muted">see Cost History</dd>
  </div>
  <div class="field">
    <dt>Last activity</dt>
    <dd>{formatRelative(row.last_activity_at)}</dd>
  </div>
</dl>

<style>
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: var(--space-0);
  }

  .field {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }

  dt {
    color: var(--color-text-muted);
  }

  dd {
    margin: var(--space-0);
    color: var(--color-text);
    text-align: right;
  }

  .muted {
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
