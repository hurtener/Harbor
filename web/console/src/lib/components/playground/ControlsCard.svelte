<script lang="ts">
  // Harbor Console — Playground Controls card (Phase 73n / D-130, 108 / D-167,
  // 108a fidelity pass).
  //
  // The right-rail Controls card: reasoning-effort (segmented), temperature +
  // top-p sliders with live numeric values, max-tokens, and a collapsible
  // system-prompt override. Overrides apply LIVE (debounced) — they re-wire the
  // NEXT message, so there is no "save" button (108a D-Q3 / operator feedback:
  // a save button is wrong when the controls implicitly affect the next turn).
  // The Drift-mode "Post-V1" toggle is removed (no Post-V1 labels above V1).
  //
  // Design tokens only.

  let {
    pending = false,
    result = null,
    onapply
  }: {
    /** True while a `runs.set_overrides` call is in flight. */
    pending?: boolean;
    /** The last apply result — surfaced as a subtle saved/failed hint. */
    result?: { ok: boolean; message: string } | null;
    /** Invoked (debounced) with the composed overrides whenever a control changes. */
    onapply: (overrides: {
      reasoningEffort?: string;
      temperature?: number;
      topP?: number;
      maxTokens?: number;
      systemPromptOverride?: string;
    }) => void;
  } = $props();

  // Defaults: reasoning unset ('' = the agent's default); temperature/top-p sit
  // at neutral resting values so the numeric readout shows a real number rather
  // than the prior "—". Nothing is sent until the operator changes a control.
  const DEFAULTS = { reasoningEffort: '', temperature: 1, topP: 1, maxTokens: '', systemPrompt: '' };

  let reasoningEffort = $state(DEFAULTS.reasoningEffort);
  let temperature = $state<number>(DEFAULTS.temperature);
  let topP = $state<number>(DEFAULTS.topP);
  let maxTokens = $state('');
  let systemPrompt = $state('');
  let systemPromptOpen = $state(false);

  const effortOptions = [
    { value: '', label: 'Default' },
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' }
  ];

  function composeOverrides(): {
    reasoningEffort?: string;
    temperature?: number;
    topP?: number;
    maxTokens?: number;
    systemPromptOverride?: string;
  } {
    const o: {
      reasoningEffort?: string;
      temperature?: number;
      topP?: number;
      maxTokens?: number;
      systemPromptOverride?: string;
    } = {};
    if (reasoningEffort !== '') o.reasoningEffort = reasoningEffort;
    o.temperature = temperature;
    o.topP = topP;
    if (maxTokens !== '') {
      const m = Number(maxTokens);
      if (!Number.isNaN(m)) o.maxTokens = m;
    }
    if (systemPrompt !== '') o.systemPromptOverride = systemPrompt;
    return o;
  }

  // Live apply, debounced — every control change re-sends the override set so
  // the next message reflects it without an explicit save.
  let debounceHandle: ReturnType<typeof setTimeout> | null = null;
  function applyLive(): void {
    if (debounceHandle !== null) clearTimeout(debounceHandle);
    debounceHandle = setTimeout(() => {
      onapply(composeOverrides());
      debounceHandle = null;
    }, 400);
  }

  function setEffort(value: string): void {
    reasoningEffort = value;
    applyLive();
  }

  function resetDefaults(): void {
    reasoningEffort = DEFAULTS.reasoningEffort;
    temperature = DEFAULTS.temperature;
    topP = DEFAULTS.topP;
    maxTokens = DEFAULTS.maxTokens;
    systemPrompt = DEFAULTS.systemPrompt;
    systemPromptOpen = false;
    applyLive();
  }
</script>

<div class="controls-card" data-testid="playground-controls-card">
  <div class="controls-head">
    <button
      type="button"
      class="reset-link"
      data-testid="controls-reset"
      onclick={resetDefaults}
    >
      Reset to defaults
    </button>
  </div>

  <!-- Reasoning effort — segmented control -->
  <div class="control-field">
    <span class="control-label">Reasoning effort</span>
    <div class="segmented" role="radiogroup" aria-label="Reasoning effort">
      {#each effortOptions as opt (opt.value)}
        <button
          type="button"
          class="segment"
          class:active={reasoningEffort === opt.value}
          onclick={() => setEffort(opt.value)}
          data-testid="controls-reasoning-effort"
          data-value={opt.value}
        >
          {opt.label}
        </button>
      {/each}
    </div>
  </div>

  <!-- Temperature — slider + live numeric value -->
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
        oninput={applyLive}
      />
      <span class="numeric-chip tabular">{temperature.toFixed(1)}</span>
    </div>
  </label>

  <!-- Top P — slider + live numeric value -->
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
        oninput={applyLive}
      />
      <span class="numeric-chip tabular">{topP.toFixed(2)}</span>
    </div>
  </label>

  <!-- Max tokens -->
  <label class="control-field">
    <span class="control-label">Max tokens</span>
    <input
      class="control-input"
      type="number"
      min="1"
      placeholder="Default"
      data-testid="controls-max-tokens"
      bind:value={maxTokens}
      oninput={applyLive}
    />
  </label>

  <!-- System prompt override — collapsible -->
  <div class="control-field">
    <div class="control-label-row">
      <span class="control-label">System prompt override</span>
      <button
        type="button"
        class="toggle-link"
        data-testid="controls-system-prompt-toggle"
        aria-expanded={systemPromptOpen}
        onclick={() => (systemPromptOpen = !systemPromptOpen)}
      >
        {systemPromptOpen ? 'On' : 'Off'}
      </button>
    </div>
    {#if systemPromptOpen}
      <textarea
        class="control-input"
        rows="3"
        placeholder="Leave blank to keep the agent's prompt"
        data-testid="controls-system-prompt"
        bind:value={systemPrompt}
        oninput={applyLive}
      ></textarea>
    {/if}
  </div>

  <p class="apply-hint" data-testid="controls-apply-hint">
    {#if pending}
      Applying…
    {:else if result !== null}
      <span data-ok={result.ok} data-testid="controls-apply-result">{result.message}</span>
    {:else}
      Changes apply to your next message.
    {/if}
  </p>
</div>

<style>
  .controls-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .controls-head {
    display: flex;
    justify-content: flex-end;
  }

  .reset-link,
  .toggle-link {
    background: none;
    border: none;
    color: var(--color-accent);
    font-size: var(--text-xs);
    cursor: pointer;
    padding: var(--space-0);
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

  .control-label-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
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

  .apply-hint {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .apply-hint [data-ok='true'] {
    color: var(--color-success);
  }

  .apply-hint [data-ok='false'] {
    color: var(--color-danger);
  }
</style>
