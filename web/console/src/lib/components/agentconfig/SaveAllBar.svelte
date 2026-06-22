<script lang="ts">
  // Harbor Console — agent-config atomic multi-section "Save all" bar (92i).
  //
  // Surfaces the staged-edit count across the form areas (prompt / model &
  // sampling / MCP exposure) and the primary atomic save: ONE `set_revision`
  // committing every staged edit as a single revision (the operator's "change
  // prompt + temperature + model in one revision") — instead of N
  // single-section convenience-verb revisions. Discard drops the staged edits
  // without a write. Admin-gated (disabled-with-tooltip — the 92b precedent).
  // Renders OUTSIDE the `<PageState>` async boundary so it holds even when the
  // runtime carries no agent_config data. Page-specific; tokens only; runes.
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();
</script>

{#if panel.hasStagedChanges || panel.saveAllSaved || panel.saveAllError}
  <div class="save-all-bar" data-testid="agentcfg-save-all-bar">
    <div class="status">
      {#if panel.hasStagedChanges}
        <span class="staged" data-testid="agentcfg-staged-count">
          Unsaved changes in {panel.stagedAreaCount}
          {panel.stagedAreaCount === 1 ? 'area' : 'areas'}
        </span>
      {:else if panel.saveAllSaved}
        <span class="saved" data-testid="agentcfg-save-all-saved">
          Saved as one revision — effective next run.
        </span>
      {/if}
      {#if panel.saveAllError}
        <span class="form-error" data-testid="agentcfg-save-all-error">
          {panel.saveAllError.code}: {panel.saveAllError.message}
        </span>
      {/if}
    </div>

    <div class="actions">
      <button
        type="button"
        class="ghost"
        data-testid="agentcfg-discard-all"
        disabled={!panel.hasStagedChanges || panel.saveAllPhase === 'saving'}
        onclick={() => panel.discardStaged()}
      >
        Discard
      </button>
      <button
        type="button"
        class="primary"
        data-testid="agentcfg-save-all"
        disabled={!panel.hasAdminScope || !panel.hasStagedChanges || panel.saveAllPhase === 'saving'}
        title={panel.hasAdminScope
          ? 'Commit every staged edit as ONE revision'
          : 'Saving requires the admin scope claim'}
        onclick={() => void panel.saveAll()}
      >
        {panel.saveAllPhase === 'saving' ? 'Saving…' : 'Save all'}
      </button>
    </div>
  </div>
{/if}

<style>
  .save-all-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
  }
  .status {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    min-width: 0;
    font-size: var(--text-xs);
  }
  .staged {
    color: var(--color-text);
    font-weight: 600;
  }
  .saved {
    color: var(--color-success);
  }
  .form-error {
    color: var(--color-danger);
  }
  .actions {
    display: flex;
    gap: var(--space-2);
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
  }
  .primary:disabled,
  .ghost:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
