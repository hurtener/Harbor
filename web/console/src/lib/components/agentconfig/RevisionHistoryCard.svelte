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
</script>

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
            <code class="rev-id">{shortId(rev.revision_id)}</code>
            {#if rev.revision_id === panel.activeRevisionId}
              <StatusChip kind="accent" label="active" />
            {/if}
            <span class="rev-time">{fmtTime(rev.created_at)}</span>
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
                : 'Roll the active config back to this revision'
              : 'Rolling back requires the admin scope claim'}
            onclick={() => void panel.rollback(rev.revision_id)}
          >
            {panel.rollbackBusy === rev.revision_id ? 'Rolling back…' : 'Rollback'}
          </button>
        </li>
      {/each}
    </ul>

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
