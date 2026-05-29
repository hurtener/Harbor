<script lang="ts">
  // Harbor Console — Playground Controls card (Phase 73n / D-130, Phase 108 / D-167).
  //
  // The right-rail Controls card: reasoning-effort (segmented control),
  // temperature + top-p sliders with numeric chips, max-tokens combobox,
  // system-prompt-override textarea, and an Apply button.
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
      topP?: number;
      maxTokens?: number;
      systemPromptOverride?: string;
    }) => void;
  } = $props();

  let reasoningEffort = $state('');
  let temperature = $state('');
  let topP = $state('');
  let maxTokens = $state('');
  let systemPrompt = $state('');

  const effortOptions = [
    { value: '', label: 'Default' },
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' }
  ];

  function apply(): void {
    const overrides: {
      reasoningEffort?: string;
      temperature?: number;
      topP?: number;
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
    if (topP !== '') {
      const p = Number(topP);
      if (!Number.isNaN(p)) {
        overrides.topP = p;
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
  <!-- Reasoning effort — segmented control -->
  <div class="control-field">
    <span class="control-label">Reasoning effort</span>
    <div class="segmented" role="radiogroup" aria-label="Reasoning effort">
      {#each effortOptions as opt (opt.value)}
        <button
          type="button"
          class="segment"
          class:active={reasoningEffort === opt.value}
          onclick={() => (reasoningEffort = opt.value)}
          data-testid="controls-reasoning-effort"
          data-value={opt.value}
        >
          {opt.label}
        </button>
      {/each}
    </div>
  </div>

  <!-- Temperature — slider + numeric chip -->
  <label class="control-field">
    <span class="control-label">Temperature</span>
    <div class="slider-row">
      <input
        class="slider"
        type="range"
        min="0"
        max="2"
        step="0.1"
        data-testid="controls-temperature"
        bind:value={temperature}
      />
      <span class="numeric-chip tabular">
        {temperature !== '' ? Number(temperature).toFixed(1) : '—'}
      </span>
    </div>
  </label>

  <!-- Top P — slider + numeric chip -->
  <label class="control-field">
    <span class="control-label">Top P</span>
    <div class="slider-row">
      <input
        class="slider"
        type="range"
        min="0"
        max="1"
        step="0.05"
        data-testid="controls-top-p"
        bind:value={topP}
      />
      <span class="numeric-chip tabular">
        {topP !== '' ? Number(topP).toFixed(2) : '—'}
      </span>
    </div>
  </label>

  <!-- Max tokens — combobox -->
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

  <!-- System prompt override -->
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

  <!-- Drift mode — visible but disabled -->
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

  <!-- Sticky bottom action -->
  <div class="sticky-action">
    <button
      type="button"
      class="apply-button"
      data-testid="controls-apply"
      onclick={apply}
      disabled={pending}
    >
      {pending ? 'Applying…' : 'Apply to next message'}
    </button>
  </div>

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

  .segmented {
    display: flex;
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    overflow: hidden;
  }

  .segment {
    flex: 1;
    padding: var(--space-1) var(--space-2);
    background: var(--color-bg);
    color: var(--color-text-muted);
    border: none;
    border-right: var(--border-hairline);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .segment:last-child {
    border-right: none;
  }

  .segment.active {
    background: var(--color-accent-soft);
    color: var(--color-accent);
    font-weight: 600;
  }

  .slider-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .slider {
    flex: 1;
    accent-color: var(--color-accent);
  }

  .numeric-chip {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--color-text);
    min-width: var(--size-chip-min-width);
    justify-content: center;
  }

  .tabular {
    font-variant-numeric: var(--font-variant-tabular);
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

  .sticky-action {
    position: sticky;
    bottom: 0;
    background: var(--color-surface);
    padding-top: var(--space-2);
    border-top: var(--border-hairline);
    margin-top: auto;
  }

  .apply-button {
    width: 100%;
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
