<script lang="ts" module>
  // Harbor Console — Overview intervention queue (Phase 73a / D-127).
  //
  // The left-column panel in canvas row 3 (page-overview.md §4). It
  // composes the SHIPPED `pause.list` snapshot (Phase 72e / D-110) into
  // a `DataTable` of pending pauses, identity-scope-filtered. Each row
  // carries three action affordances (page-overview.md §12):
  //   - Approve / Reject — invoke the SHIPPED Phase 54 `approve` /
  //     `reject` control verbs (NO parallel implementation — §13);
  //   - View — deep-links into the paused session's Live Runtime page.
  //
  // # Control-scope gating (D-066 / D-079, CONVENTIONS.md §5)
  //
  // Approve / Reject are control-plane verbs. Without the admin scope
  // claim the buttons render DISABLED-WITH-TOOLTIP — never hidden into
  // a fake "works" state, never a stubbed action presented as done.
  // The runtime re-checks server-side regardless (`CodeScopeMismatch`).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  /** The result of a row action, surfaced inline next to the row. */
  export interface RowActionResult {
    /** The pause token the action targeted. */
    token: string;
    /** True on a 2xx control response. */
    ok: boolean;
    /** A human-readable status line (`approved` or `code: message`). */
    message: string;
  }
</script>

<script lang="ts">
  import DataTable, { type DataTableColumn } from '$lib/components/ui/DataTable.svelte';
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import type { PauseSnapshot } from '$lib/protocol/pause.js';

  let {
    snapshots,
    canControl,
    pendingToken,
    results,
    onapprove,
    onreject
  }: {
    /** The page of pending-pause snapshots. */
    snapshots: PauseSnapshot[];
    /** Whether the operator carries the admin control scope (D-066). */
    canControl: boolean;
    /** The token of an in-flight action, or null. */
    pendingToken: string | null;
    /** The per-token action results, surfaced inline. */
    results: Map<string, RowActionResult>;
    /** Invoked when Approve is clicked — runs the real Phase 54 `approve`. */
    onapprove: (snapshot: PauseSnapshot) => void;
    /** Invoked when Reject is clicked — runs the real Phase 54 `reject`. */
    onreject: (snapshot: PauseSnapshot) => void;
  } = $props();

  const columns: DataTableColumn[] = [
    { key: 'reason', label: 'Reason' },
    { key: 'session', label: 'Session' },
    { key: 'paused_at', label: 'Paused' },
    { key: 'actions', label: 'Actions' }
  ];

  // The control-scope tooltip the disabled buttons carry — D-066.
  const NO_SCOPE_TOOLTIP =
    'Requires the admin control-scope claim (D-066) — request elevation from an administrator.';
</script>

<div class="intervention-queue" data-testid="intervention-queue">
  <DataTable
    {columns}
    rows={snapshots}
    rowKey={(r) => (r as PauseSnapshot).token}
  >
    {#snippet empty()}
      <div class="queue-empty" data-testid="intervention-queue-empty">
        <p class="headline">No pending interventions</p>
        <p class="detail">No runs are parked awaiting an operator decision.</p>
      </div>
    {/snippet}

    {#snippet row(r)}
      {@const snapshot = r as PauseSnapshot}
      {@const result = results.get(snapshot.token)}
      <td>
        <StatusChip kind="warning" label={snapshot.reason} />
      </td>
      <td class="mono">{snapshot.identity.session}</td>
      <td class="mono">{snapshot.paused_at}</td>
      <td class="actions-cell">
        <button
          type="button"
          class="action approve"
          data-testid="intervention-approve"
          disabled={!canControl || pendingToken === snapshot.token}
          title={canControl ? 'Approve this run (Phase 54 approve)' : NO_SCOPE_TOOLTIP}
          onclick={() => onapprove(snapshot)}
        >
          Approve
        </button>
        <button
          type="button"
          class="action reject"
          data-testid="intervention-reject"
          disabled={!canControl || pendingToken === snapshot.token}
          title={canControl ? 'Reject this run (Phase 54 reject)' : NO_SCOPE_TOOLTIP}
          onclick={() => onreject(snapshot)}
        >
          Reject
        </button>
        <a
          class="action view"
          data-testid="intervention-view"
          href={`/live-runtime/${encodeURIComponent(snapshot.identity.session)}`}
        >
          View
        </a>
        {#if result}
          <span
            class="row-result"
            data-testid="intervention-result"
            data-ok={result.ok}
          >
            {result.message}
          </span>
        {/if}
      </td>
    {/snippet}
  </DataTable>
</div>

<style>
  .intervention-queue {
    display: flex;
    flex-direction: column;
  }

  .queue-empty {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-8) var(--space-4);
    text-align: center;
  }

  .queue-empty .headline {
    margin: var(--space-0);
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--color-text);
  }

  .queue-empty .detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .actions-cell {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .action {
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    cursor: pointer;
    background: var(--color-surface-raised);
    color: var(--color-text);
    text-decoration: none;
  }

  .action.approve:not(:disabled) {
    color: var(--color-success);
  }

  .action.reject:not(:disabled) {
    color: var(--color-danger);
  }

  .action:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .row-result {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .row-result[data-ok='true'] {
    color: var(--color-success);
  }

  .row-result[data-ok='false'] {
    color: var(--color-danger);
  }
</style>
