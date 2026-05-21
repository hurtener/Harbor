<script lang="ts">
  // Harbor Console — Playground Controls card (Phase 73n / D-130).
  //
  // The right-rail Controls card: reasoning-effort, temperature,
  // max-tokens, and system-prompt-override inputs, plus an Apply button
  // that records the override via `runs.set_overrides` (Brief 11
  // §PG-5). The override applies to the NEXT message in the session.
  //
  // The drift-mode toggle is rendered visible-but-DISABLED with a
  // "Post-V1" tooltip — Brief 11 §PG-5 defers drift mode. It is NOT a
  // stubbed action presented as done (CONVENTIONS.md §5): the disabled
  // state + tooltip is the honest signal.
  //
  // Design tokens only.

  let {
    pending = false,
    result = null,
    onapply
  }: {
    /** True while a `runs.set_overrides` call is in flight. */
    pending?: boolean;
    /** The last apply result — success/failure feedback. */
    result?: { ok: boolean; message: string } | null;
    /** Invoked with the composed overrides on Apply. */
    onapply: (overrides: {
      reasoningEffort?: string;
      temperature?: number;
      maxTokens?: number;
      systemPromptOverride?: string;
    }) => void;
  } = $props();

  // The four override inputs. Each is OPT-IN — an untouched field is
  // omitted from the override (leaves the runtime default in place).
  let reasoningEffort = $state('');
  let temperature = $state('');
  let maxTokens = $state('');
  let systemPrompt = $state('');

  function apply(): void {
    const overrides: {
      reasoningEffort?: string;
      temperature?: number;
      maxTokens?: number;
      systemPromptOverride?: string;
    } = {};
    if (reasoningEffort !== '') {
      overrides.reasoningEffort = reasoningEffort;
    }
    if (temperature !== '') {
      const t = Number(temperature);
      if (!Number.isNaN(t)) {
        overrides.temperature = t;
      }
    }
    if (maxTokens !== '') {
      const m = Number(maxTokens);
      if (!Number.isNaN(m)) {
        overrides.maxTokens = m;
      }
    }
    if (systemPrompt !== '') {
      overrides.systemPromptOverride = systemPrompt;
    }
    onapply(overrides);
  }
</script>

<div class="controls-card" data-testid="playground-controls-card">
  <label class="control-field">
    <span class="control-label">Reasoning effort</span>
    <select
      class="control-input"
      data-testid="controls-reasoning-effort"
      bind:value={reasoningEffort}
    >
      <option value="">Default</option>
      <option value="low">Low</option>
      <option value="medium">Medium</option>
      <option value="high">High</option>
    </select>
  </label>

  <label class="control-field">
    <span class="control-label">Temperature</span>
    <input
      class="control-input"
      type="number"
      step="0.1"
      min="0"
      max="2"
      placeholder="Default"
      data-testid="controls-temperature"
      bind:value={temperature}
    />
  </label>

  <label class="control-field">
    <span class="control-label">Max tokens</span>
    <input
      class="control-input"
      type="number"
      min="1"
      placeholder="Default"
      data-testid="controls-max-tokens"
      bind:value={maxTokens}
    />
  </label>

  <label class="control-field">
    <span class="control-label">System prompt override</span>
    <textarea
      class="control-input"
      rows="3"
      placeholder="Leave blank to keep the agent's prompt"
      data-testid="controls-system-prompt"
      bind:value={systemPrompt}
    ></textarea>
  </label>

  <label class="control-field drift-field">
    <span class="control-label">Drift mode</span>
    <span class="drift-toggle" title="Drift mode — Post-V1 (Brief 11 §PG-5)">
      <input
        type="checkbox"
        data-testid="controls-drift-mode"
        disabled
        title="Drift mode — Post-V1 (Brief 11 §PG-5)"
      />
      <span class="drift-note">Post-V1</span>
    </span>
  </label>

  <button
    type="button"
    class="apply-button"
    data-testid="controls-apply"
    onclick={apply}
    disabled={pending}
  >
    {pending ? 'Applying…' : 'Apply to next message'}
  </button>

  {#if result !== null}
    <p
      class="apply-result"
      data-testid="controls-apply-result"
      data-ok={result.ok}
      role="status"
    >
      {result.message}
    </p>
  {/if}
</div>

<style>
  .controls-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .control-field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .control-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    color: var(--color-text-muted);
  }

  .control-input {
    padding: var(--space-1) var(--space-2);
    background: var(--color-bg);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    color: var(--color-text);
    font-family: var(--font-sans);
  }

  .drift-field .drift-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .drift-note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .apply-button {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    font-weight: 600;
    cursor: pointer;
  }

  .apply-button:disabled {
    opacity: 0.4;
    cursor: not-allowed;
  }

  .apply-result {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-success);
  }

  .apply-result[data-ok='false'] {
    color: var(--color-danger);
  }
</style>
