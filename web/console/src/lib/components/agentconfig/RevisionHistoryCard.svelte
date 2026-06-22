<script lang="ts">
  // Harbor Console — agent-config revision history + diff + rollback (92a).
  //
  // Lists the agent's config revisions newest-first, lets the operator pick
  // two to compare (server-side diff via `agent_config.diff`), and offers a
  // one-click rollback (`agent_config.rollback`) behind a confirm. The
  // active revision is badged. Rollback is admin-gated (disabled-with-tooltip
  // for non-admin — the 92b precedent). Page-specific; composes the shared
  // StatusChip + PageState (the nested diff boundary) + the DiffView. Tokens
  // only; Svelte 5 runes (D-092).
  import { StatusChip, PageState } from '$lib/components/ui';
  import DiffView from './DiffView.svelte';
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();

  /** A short, copy-friendly revision label (first 12 chars). */
  function shortId(id: string): string {
    return id.length > 12 ? `${id.slice(0, 12)}…` : id;
  }
  function fmtTime(iso: string): string {
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
  }

  /** Escape closes the rollback-preview modal (no write) — the dismissal
   * affordance alongside Cancel + scrim-click. */
  function onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape' && panel.rollbackTarget !== null) {
      panel.cancelRollbackPreview();
    }
  }
</script>

<svelte:window onkeydown={onKeydown} />

<div class="card-body" data-testid="agentcfg-revisions">
  <p class="note">
    The agent&rsquo;s immutable config revision chain, newest first. Pick two to
    compare, or roll the active pointer back to any revision (applies on the
    agent&rsquo;s next run).
  </p>

  {#if panel.revisions.length === 0}
    <p class="note" data-testid="agentcfg-revisions-empty">
      No revisions yet — this agent runs the runtime&rsquo;s configured defaults.
      Saving any section below records the first revision.
    </p>
  {:else}
    <ul class="rev-list">
      {#each panel.revisions as rev (rev.revision_id)}
        <li class="rev-row" data-testid="agentcfg-revision-row">
          <div class="rev-main">
            <div class="rev-head">
              <code class="rev-id">{shortId(rev.revision_id)}</code>
              {#if rev.revision_id === panel.activeRevisionId}
                <StatusChip kind="accent" label="active" />
              {/if}
              <span class="rev-time">{fmtTime(rev.created_at)}</span>
            </div>
            <!-- Derived, client-side change summary (no extra round-trip) — the
                 readability answer: what this revision changed vs its parent. -->
            <span class="rev-summary" data-testid="agentcfg-revision-summary">
              {panel.revisionSummary(rev)}
            </span>
          </div>
          <button
            type="button"
            class="ghost"
            data-testid="agentcfg-rollback"
            disabled={!panel.hasAdminScope ||
              panel.rollbackBusy !== null ||
              rev.revision_id === panel.activeRevisionId}
            title={panel.hasAdminScope
              ? rev.revision_id === panel.activeRevisionId
                ? 'Already the active revision'
                : 'Preview the diff, then roll the active config back to this revision'
              : 'Rolling back requires the admin scope claim'}
            onclick={() => void panel.requestRollback(rev.revision_id)}
          >
            {panel.rollbackBusy === rev.revision_id ? 'Rolling back…' : 'Rollback…'}
          </button>
        </li>
      {/each}
    </ul>

    <!-- Diff-before-rollback preview (92i): a rollback NEVER repoints blindly;
         the operator confirms against the structured active→target diff. -->
    {#if panel.rollbackTarget !== null}
      <!-- svelte-ignore a11y_click_events_have_key_events -->
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <div class="modal-scrim" onclick={() => panel.cancelRollbackPreview()}>
        <div
          class="modal"
          role="dialog"
          aria-modal="true"
          aria-label="Confirm rollback"
          tabindex="-1"
          data-testid="agentcfg-rollback-preview"
          onclick={(e) => e.stopPropagation()}
        >
          <h3 class="modal-title">
            Roll back to {shortId(panel.rollbackTarget)}?
          </h3>
          <p class="note">
            This repoints the active config to {shortId(panel.rollbackTarget)}.
            The change below applies on the agent&rsquo;s next run. Review it,
            then confirm.
          </p>

          <PageState
            status={panel.rollbackPreviewPhase === 'loading'
              ? 'loading'
              : panel.rollbackPreviewPhase === 'error'
                ? 'error'
                : 'ready'}
            error={panel.rollbackPreviewError}
            onretry={() => void panel.requestRollback(panel.rollbackTarget ?? '')}
            nested
          >
            {#if panel.rollbackPreviewDiff}
              <DiffView diff={panel.rollbackPreviewDiff} />
            {/if}
          </PageState>

          <div class="modal-actions">
            <button
              type="button"
              class="ghost"
              data-testid="agentcfg-rollback-cancel"
              onclick={() => panel.cancelRollbackPreview()}
            >
              Cancel
            </button>
            <button
              type="button"
              class="primary"
              data-testid="agentcfg-rollback-confirm"
              disabled={!panel.hasAdminScope ||
                panel.rollbackBusy !== null ||
                panel.rollbackPreviewPhase !== 'ready'}
              onclick={() => void panel.confirmRollbackPreview()}
            >
              {panel.rollbackBusy !== null ? 'Rolling back…' : 'Confirm rollback'}
            </button>
          </div>
        </div>
      </div>
    {/if}

    {#if panel.rolledBackTo}
      <p class="saved" data-testid="agentcfg-rollback-saved">
        Rolled back to {shortId(panel.rolledBackTo)} — effective next run.
      </p>
    {/if}
    {#if panel.rollbackError}
      <p class="form-error" data-testid="agentcfg-rollback-error">
        {panel.rollbackError.code}: {panel.rollbackError.message}
      </p>
    {/if}

    <div class="diff-controls">
      <label class="field">
        <span class="field-label">From</span>
        <select bind:value={panel.fromRevision} data-testid="agentcfg-diff-from">
          {#each panel.revisions as rev (rev.revision_id)}
            <option value={rev.revision_id}>{shortId(rev.revision_id)}</option>
          {/each}
        </select>
      </label>
      <label class="field">
        <span class="field-label">To</span>
        <select bind:value={panel.toRevision} data-testid="agentcfg-diff-to">
          {#each panel.revisions as rev (rev.revision_id)}
            <option value={rev.revision_id}>{shortId(rev.revision_id)}</option>
          {/each}
        </select>
      </label>
      <button
        type="button"
        class="primary"
        data-testid="agentcfg-diff-compute"
        disabled={panel.fromRevision === '' || panel.toRevision === '' || panel.diffPhase === 'loading'}
        onclick={() => void panel.computeDiff()}
      >
        {panel.diffPhase === 'loading' ? 'Comparing…' : 'Compare'}
      </button>
    </div>

    {#if panel.diffPhase !== 'idle'}
      <PageState
        status={panel.diffPhase === 'loading'
          ? 'loading'
          : panel.diffPhase === 'error'
            ? 'error'
            : 'ready'}
        error={panel.diffError}
        onretry={() => void panel.computeDiff()}
        nested
      >
        {#if panel.diff}
          <DiffView diff={panel.diff} />
        {/if}
      </PageState>
    {/if}
  {/if}
</div>

<style>
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    font-size: var(--text-sm);
  }
  .note {
    margin: var(--space-0);
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
  .rev-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .rev-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
  }
  .rev-main {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .rev-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .rev-id {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
  }
  .rev-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .rev-summary {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .modal-scrim {
    position: fixed;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-4);
    background: var(--color-overlay);
  }
  .modal {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    width: 100%;
    max-width: var(--size-prose-max);
    max-height: 80vh;
    overflow-y: auto;
    padding: var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
  }
  .modal-title {
    margin: var(--space-0);
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--color-text);
  }
  .modal-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  .diff-controls {
    display: flex;
    align-items: flex-end;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  select {
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  .primary,
  .ghost {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .primary {
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .ghost {
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-xs);
  }
  .primary:disabled,
  .ghost:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .saved {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-success);
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
</style>
