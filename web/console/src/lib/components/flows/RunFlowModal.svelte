<script lang="ts">
  // Harbor Console — Run-flow input modal (Phase 73i / D-117).
  //
  // The inline runner the `Run flow` / `Run this flow ▶` actions open.
  // It collects a hand-crafted JSON input form and invokes `flows.run`.
  // `flows.run` is the ONLY mutating Flows-page action; the Runtime
  // gates it on the verified `admin` scope claim (D-079) — a claimless
  // submit surfaces an inline error here.
  interface Props {
    flowID: string;
    open: boolean;
    pending: boolean;
    errorMessage: string | null;
    onsubmit: (inputs: Record<string, unknown>) => void;
    oncancel: () => void;
  }

  const {
    flowID,
    open,
    pending,
    errorMessage,
    onsubmit,
    oncancel,
  }: Props = $props();

  let raw = $state('{}');
  let parseError = $state<string | null>(null);

  function submit() {
    parseError = null;
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(raw) as Record<string, unknown>;
    } catch {
      parseError = 'Inputs must be a valid JSON object.';
      return;
    }
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      parseError = 'Inputs must be a JSON object.';
      return;
    }
    onsubmit(parsed);
  }
</script>

{#if open}
  <div class="overlay" data-testid="run-flow-modal">
    <div class="modal" role="dialog" aria-modal="true" aria-label={`Run ${flowID}`}>
      <h3>Run flow — {flowID}</h3>
      <label for="run-inputs">Inputs (JSON)</label>
      <textarea
        id="run-inputs"
        data-testid="run-flow-inputs"
        bind:value={raw}
        rows="6"
      ></textarea>
      {#if parseError}
        <p class="error" data-testid="run-flow-parse-error">{parseError}</p>
      {/if}
      {#if errorMessage}
        <p class="error" data-testid="run-flow-error">{errorMessage}</p>
      {/if}
      <div class="actions">
        <button class="ghost" onclick={oncancel} disabled={pending}>Cancel</button>
        <button
          class="primary"
          data-testid="run-flow-submit"
          onclick={submit}
          disabled={pending}
        >
          {pending ? 'Running…' : 'Run flow ▶'}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .overlay {
    position: fixed;
    inset: 0;
    background: var(--color-overlay);
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .modal {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-6);
    width: var(--size-modal-width);
    max-width: 90vw;
  }

  h3 {
    font-size: var(--text-base);
    margin: var(--space-0) var(--space-0) var(--space-3);
  }

  label {
    display: block;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    margin-bottom: var(--space-1);
  }

  textarea {
    width: 100%;
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: var(--space-2);
    box-sizing: border-box;
  }

  .error {
    color: var(--color-danger);
    font-size: var(--text-xs);
    margin: var(--space-2) var(--space-0) var(--space-0);
  }

  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-4);
  }

  .primary {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  .ghost {
    background: none;
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    cursor: pointer;
  }

  button:disabled {
    opacity: 0.6;
    cursor: not-allowed;
  }
</style>
