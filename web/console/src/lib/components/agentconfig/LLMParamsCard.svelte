<script lang="ts">
  // Harbor Console — agent-config per-agent LLM parameters (92j / 92i).
  //
  // Pins the agent's model & sampling defaults (model / temperature /
  // max-tokens / reasoning-effort) as a versioned config section. An empty
  // field inherits the next resolution layer (the tenant-wide baseline, then
  // config). Saving replaces ONLY the LLM-params section
  // (`agent_config.set_llm_params`) and records a revision; a set model + the
  // sampling ranges are validated server-side at set time (the rejection
  // surfaces inline — never silently passed through). Writes are admin-gated
  // (disabled-with-tooltip — the 92b precedent). Page-specific; tokens only;
  // Svelte 5 runes (D-092).
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();

  const REASONING_OPTIONS = ['', 'off', 'low', 'medium', 'high'] as const;
</script>

<div class="card-body" data-testid="agentcfg-llm">
  <p class="note">
    The agent&rsquo;s per-agent model &amp; sampling defaults. Each field is
    optional — an empty field inherits the tenant default, then the runtime
    config. These override the tenant-wide baseline for this agent; a session
    swap still wins for one run. Saving records a revision (applies next run).
  </p>

  <label class="field">
    <span class="field-label">Model</span>
    <input
      type="text"
      data-testid="agentcfg-llm-model"
      placeholder="(inherit tenant default)"
      bind:value={panel.llmModel}
      oninput={() => panel.markLlmEdited()}
      disabled={!panel.hasAdminScope}
    />
  </label>

  <div class="field-row">
    <label class="field">
      <span class="field-label">Temperature</span>
      <input
        type="number"
        step="0.1"
        min="0"
        max="2"
        data-testid="agentcfg-llm-temperature"
        placeholder="(inherit)"
        bind:value={panel.llmTemperature}
        oninput={() => panel.markLlmEdited()}
        disabled={!panel.hasAdminScope}
      />
    </label>
    <label class="field">
      <span class="field-label">Max tokens</span>
      <input
        type="number"
        step="1"
        min="1"
        data-testid="agentcfg-llm-max-tokens"
        placeholder="(inherit)"
        bind:value={panel.llmMaxTokens}
        oninput={() => panel.markLlmEdited()}
        disabled={!panel.hasAdminScope}
      />
    </label>
    <label class="field">
      <span class="field-label">Reasoning effort</span>
      <select
        data-testid="agentcfg-llm-reasoning"
        bind:value={panel.llmReasoningEffort}
        onchange={() => panel.markLlmEdited()}
        disabled={!panel.hasAdminScope}
      >
        {#each REASONING_OPTIONS as opt (opt)}
          <option value={opt}>{opt === '' ? '(inherit)' : opt}</option>
        {/each}
      </select>
    </label>
  </div>

  <div class="actions">
    <button
      type="button"
      class="primary"
      data-testid="agentcfg-llm-save"
      disabled={!panel.hasAdminScope || panel.llmPhase === 'saving'}
      title={panel.hasAdminScope
        ? 'Save the per-agent model & sampling defaults (records a revision)'
        : 'Editing model & sampling requires the admin scope claim'}
      onclick={() => void panel.saveLlmParams()}
    >
      {panel.llmPhase === 'saving' ? 'Saving…' : 'Save model & sampling'}
    </button>
    {#if !panel.hasAdminScope}
      <span class="hint" data-testid="agentcfg-llm-disabled-hint">
        Requires the admin scope claim.
      </span>
    {/if}
    {#if panel.llmSaved}
      <span class="saved" data-testid="agentcfg-llm-saved">Saved — effective next run.</span>
    {/if}
  </div>
  {#if panel.llmError}
    <p class="form-error" data-testid="agentcfg-llm-error">
      {panel.llmError.code}: {panel.llmError.message}
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
  .field-row {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  input,
  select {
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  input {
    font-family: var(--font-mono);
  }
  input:disabled,
  select:disabled {
    opacity: 0.55;
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
