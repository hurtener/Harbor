<script lang="ts">
  // Harbor Console — Live Runtime cockpit "Needs attention" panel
  // (Phase 108e / D-177).
  //
  // The runtime-wide intervention queue: pending pauses / approvals /
  // auth-required gates across the selected runtime, fed by the SHIPPED
  // `pause.list` snapshot (Phase 72e / D-110). Each row carries Approve /
  // Reject / Resume row actions that invoke the SHIPPED Phase 54 control
  // verbs (`approve` / `reject` / `resume`) — no parallel implementation
  // (CLAUDE.md §13).
  //
  // # Control-scope gating (D-066 / D-079, CONVENTIONS.md §5)
  //
  // The verbs are control-plane actions. Without the admin scope claim the
  // buttons render DISABLED-WITH-TOOLTIP — never hidden into a fake "works"
  // state. The runtime re-checks server-side regardless.
  //
  // Modelled on the Overview InterventionQueue, wrapped in a nested
  // <PageState> by the page. Svelte 5 runes mode (D-092); design tokens only.
  import StatusChip from '$lib/components/ui/StatusChip.svelte';
  import type { PauseSnapshot } from '$lib/protocol/pause.js';

  /** The result of a row action, surfaced inline next to the row. */
  export interface AttentionActionResult {
    /** The pause token the action targeted. */
    token: string;
    /** True on a 2xx control response. */
    ok: boolean;
    /** A human-readable status line. */
    message: string;
  }

  let {
    snapshots,
    canControl,
    pendingToken,
    results,
    onapprove,
    onreject,
    onresume
  }: {
    /** The page of pending-pause snapshots. */
    snapshots: PauseSnapshot[];
    /** Whether the operator carries the admin control scope (D-066). */
    canControl: boolean;
    /** The token of an in-flight action, or null. */
    pendingToken: string | null;
    /** The per-token action results, surfaced inline. */
    results: Map<string, AttentionActionResult>;
    /** Approve the run — runs the real Phase 54 `approve`. */
    onapprove: (snapshot: PauseSnapshot) => void;
    /** Reject the run — runs the real Phase 54 `reject`. */
    onreject: (snapshot: PauseSnapshot) => void;
    /** Resume the run — runs the real Phase 54 `resume`. */
    onresume: (snapshot: PauseSnapshot) => void;
  } = $props();

  const NO_SCOPE_TOOLTIP =
    'Requires the admin control-scope claim (D-066) — request elevation from an administrator.';
</script>

<ul class="attention-list" data-testid="needs-attention-list">
  {#each snapshots as snapshot (snapshot.token)}
    {@const result = results.get(snapshot.token)}
    <li class="attention-row" data-testid="needs-attention-row">
      <div class="row-head">
        <StatusChip kind="warning" label={snapshot.reason} />
        <span class="row-session mono">{snapshot.identity.session}</span>
      </div>
      <div class="row-meta">
        <span class="row-time">{snapshot.paused_at}</span>
      </div>
      <div class="row-actions">
        <button
          type="button"
          class="action approve"
          data-testid="needs-attention-approve"
          disabled={!canControl || pendingToken === snapshot.token}
          title={canControl ? 'Approve this run (Phase 54 approve)' : NO_SCOPE_TOOLTIP}
          onclick={() => onapprove(snapshot)}
        >
          Approve
        </button>
        <button
          type="button"
          class="action reject"
          data-testid="needs-attention-reject"
          disabled={!canControl || pendingToken === snapshot.token}
          title={canControl ? 'Reject this run (Phase 54 reject)' : NO_SCOPE_TOOLTIP}
          onclick={() => onreject(snapshot)}
        >
          Reject
        </button>
        <button
          type="button"
          class="action resume"
          data-testid="needs-attention-resume"
          disabled={!canControl || pendingToken === snapshot.token}
          title={canControl ? 'Resume this run (Phase 54 resume)' : NO_SCOPE_TOOLTIP}
          onclick={() => onresume(snapshot)}
        >
          Resume
        </button>
      </div>
      {#if result}
        <span class="row-result" data-testid="needs-attention-result" data-ok={result.ok}>
          {result.message}
        </span>
      {/if}
    </li>
  {/each}
</ul>

<style>
  .attention-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .attention-row {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .row-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .row-session {
    color: var(--color-text);
  }

  .row-meta {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .row-actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .action {
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    cursor: pointer;
    background: var(--color-surface);
    color: var(--color-text);
  }

  .action.approve:not(:disabled) {
    color: var(--color-success);
  }

  .action.reject:not(:disabled) {
    color: var(--color-danger);
  }

  .action.resume:not(:disabled) {
    color: var(--color-accent);
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

  .mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
</style>
