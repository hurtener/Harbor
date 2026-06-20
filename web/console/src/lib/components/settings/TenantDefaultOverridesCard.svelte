<script lang="ts">
  // Settings — Tenant Default LLM Overrides card (Phase 92b / D-232).
  //
  // The admin tenant-default control: read + write a tenant's default LLM
  // parameters (model / additive extra-instructions / temperature /
  // max-tokens / reasoning-effort) live, no redeploy. The change lands on
  // every session in the tenant on its NEXT run (the RFC §6.15
  // ModelOverride seam). Both calls are ADMIN-gated (CONVENTIONS.md §5):
  // the form either invokes the REAL governance.set/get_tenant_overrides
  // methods, or renders disabled-with-tooltip when the connection lacks
  // the admin scope claim — the runtime ALSO gates (a forged call fails
  // closed with a 403 the error surfaces). An empty field clears that
  // dimension (desired-state replace).
  import type { TenantDefaultOverridesState } from '$lib/settings/state.svelte.js';

  let { overrides }: { overrides: TenantDefaultOverridesState } = $props();

  const onEdit = () => overrides.markEdited();
</script>

<div class="card-body" data-testid="settings-tenant-default-overrides">
  <p class="note">
    Default LLM parameters applied to every session in this tenant on its next
    run. Leave a field blank to inherit the runtime&rsquo;s configured default.
  </p>

  {#if overrides.phase === 'loading'}
    <p class="note" data-testid="tenant-overrides-loading">Loading current defaults…</p>
  {:else}
    <div class="form-grid">
      <label class="field">
        <span class="field-label">Model</span>
        <input
          type="text"
          data-testid="tenant-overrides-model"
          placeholder="(inherit configured model)"
          bind:value={overrides.model}
          oninput={onEdit}
          disabled={!overrides.hasAdminScope}
        />
      </label>
      <label class="field">
        <span class="field-label">Temperature</span>
        <input
          type="number"
          step="0.1"
          min="0"
          max="2"
          data-testid="tenant-overrides-temperature"
          placeholder="(inherit)"
          bind:value={overrides.temperature}
          oninput={onEdit}
          disabled={!overrides.hasAdminScope}
        />
      </label>
      <label class="field">
        <span class="field-label">Max tokens</span>
        <input
          type="number"
          min="1"
          data-testid="tenant-overrides-max-tokens"
          placeholder="(inherit)"
          bind:value={overrides.maxTokens}
          oninput={onEdit}
          disabled={!overrides.hasAdminScope}
        />
      </label>
      <label class="field">
        <span class="field-label">Reasoning effort</span>
        <select
          data-testid="tenant-overrides-reasoning"
          bind:value={overrides.reasoningEffort}
          onchange={onEdit}
          disabled={!overrides.hasAdminScope}
        >
          <option value="">(inherit)</option>
          <option value="low">low</option>
          <option value="medium">medium</option>
          <option value="high">high</option>
        </select>
      </label>
      <label class="field field-wide">
        <span class="field-label">Extra instructions (additive)</span>
        <textarea
          rows="3"
          data-testid="tenant-overrides-extra-instructions"
          placeholder="(none) — appended to the agent's system prompt"
          bind:value={overrides.extraInstructions}
          oninput={onEdit}
          disabled={!overrides.hasAdminScope}
        ></textarea>
      </label>
    </div>

    <div class="actions">
      <button
        type="button"
        class="primary"
        data-testid="tenant-overrides-save"
        disabled={!overrides.hasAdminScope || overrides.phase === 'saving'}
        title={overrides.hasAdminScope
          ? 'Save the tenant default overrides (applies on each session’s next run)'
          : 'Setting tenant defaults requires the admin scope claim'}
        onclick={() => void overrides.save()}
      >
        {overrides.phase === 'saving' ? 'Saving…' : 'Save defaults'}
      </button>
      {#if !overrides.hasAdminScope}
        <span class="hint" data-testid="tenant-overrides-disabled-hint">
          Requires the admin scope claim.
        </span>
      {/if}
      {#if overrides.saved}
        <span class="saved" data-testid="tenant-overrides-saved">Saved — effective next run.</span>
      {/if}
    </div>

    {#if overrides.phase === 'error' && overrides.error}
      <p class="form-error" data-testid="tenant-overrides-error">
        {overrides.error.code}: {overrides.error.message}
      </p>
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
  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2) var(--space-3);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field-wide {
    grid-column: 1 / -1;
  }
  .field-label {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  input,
  select,
  textarea {
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    color: var(--color-text);
    font-size: var(--text-sm);
  }
  textarea {
    resize: vertical;
    font-family: inherit;
  }
  input:disabled,
  select:disabled,
  textarea:disabled {
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
  .hint,
  .form-error,
  .saved {
    font-size: var(--text-xs);
  }
  .hint {
    color: var(--color-text-muted);
  }
  .saved {
    color: var(--color-success);
  }
  .form-error {
    color: var(--color-danger);
    margin: var(--space-0);
  }
</style>
