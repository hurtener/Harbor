<script lang="ts">
  // Harbor Console — agent-config layered system prompt (92e).
  //
  // Edits the operator BASE layer and views the (read-only here) USER layer.
  // The user layer composes ABOVE the base in the lower-trust position — it
  // can extend but never replace or weaken the operator base, so the operator
  // UI only writes the base. Saving replaces ONLY the prompt-layer section
  // (`agent_config.set_prompt_layers`) and records a revision; the diff
  // against prior revisions is visible in the Revision history card above.
  // Writes are admin-gated (disabled-with-tooltip — the 92b precedent).
  // Page-specific; Tokens only; Svelte 5 runes (D-092).
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();
</script>

<div class="card-body" data-testid="agentcfg-prompt">
  <p class="note">
    The agent&rsquo;s layered system prompt. You edit the operator <strong>base</strong>
    layer; the <strong>user</strong> layer composes above it (set per-session by
    the end user) and is shown read-only. Saving records a revision (applies next run).
  </p>

  <label class="field">
    <span class="field-label">Operator base layer</span>
    <textarea
      rows="6"
      data-testid="agentcfg-prompt-base"
      placeholder="You are a helpful assistant for…"
      bind:value={panel.promptBase}
      oninput={() => panel.markPromptEdited()}
      disabled={!panel.hasAdminScope}
    ></textarea>
  </label>

  <label class="field">
    <span class="field-label">User layer (read-only)</span>
    <textarea
      rows="3"
      data-testid="agentcfg-prompt-user"
      placeholder="(no user layer set)"
      value={panel.promptUser}
      readonly
    ></textarea>
  </label>

  <div class="actions">
    <button
      type="button"
      class="primary"
      data-testid="agentcfg-prompt-save"
      disabled={!panel.hasAdminScope || panel.promptPhase === 'saving'}
      title={panel.hasAdminScope
        ? 'Save the operator base prompt layer (records a revision)'
        : 'Editing the base prompt requires the admin scope claim'}
      onclick={() => void panel.savePrompt()}
    >
      {panel.promptPhase === 'saving' ? 'Saving…' : 'Save base layer'}
    </button>
    {#if !panel.hasAdminScope}
      <span class="hint" data-testid="agentcfg-prompt-disabled-hint">
        Requires the admin scope claim.
      </span>
    {/if}
    {#if panel.promptSaved}
      <span class="saved" data-testid="agentcfg-prompt-saved">Saved — effective next run.</span>
    {/if}
  </div>
  {#if panel.promptError}
    <p class="form-error" data-testid="agentcfg-prompt-error">
      {panel.promptError.code}: {panel.promptError.message}
    </p>
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
  textarea {
    padding: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
    font-family: var(--font-mono);
    resize: vertical;
  }
  textarea:disabled,
  textarea[readonly] {
    opacity: 0.7;
    background: var(--color-surface-raised);
  }
  textarea:disabled {
    cursor: not-allowed;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .primary {
    align-self: flex-start;
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    background: var(--color-accent);
    border-color: var(--color-accent);
    color: var(--color-bg);
  }
  .primary:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .saved {
    font-size: var(--text-xs);
    color: var(--color-success);
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
</style>
