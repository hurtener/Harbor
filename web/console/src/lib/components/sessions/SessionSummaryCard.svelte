<script lang="ts">
  // Harbor Console — Sessions detail-view right-rail Session Summary
  // card (Phase 73c / D-122). Renders the additive `sessions.inspect`
  // projection: id / status / started / duration / events / tasks /
  // agent / user / tenant / cost / last activity (mockup §12).
  //
  // Sourced exclusively from `sessions.inspect` — the card NEVER reads
  // runtime internals (CONVENTIONS.md / CLAUDE.md §13). Sessions-specific
  // component — lives in `components/sessions/`. Svelte 5 runes (D-092);
  // design tokens only.
  import type { SessionRow } from '$lib/sessions/types.js';
  import {
    formatCostCents,
    formatDurationNS,
    formatRelative,
    formatTokens,
    statusKind
  } from '$lib/sessions/format.js';
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
    <dt>Duration</dt>
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
    <dd>{formatCostCents(row.total_cost_cents)} · {formatTokens(row.total_tokens)} tok</dd>
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

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
