<script lang="ts">
  // Harbor Console — Live Runtime trace toggle (Phase 73b / D-126).
  //
  // The bottom-dock Trace-tab control (Brief 11 §PG-7): a local UI
  // toggle that narrows the Event Stream subscription to one run id so
  // events correlate to a single topology node. The run-scoped filter
  // rides the already-shipped `events.subscribe` run carrier (the
  // structured counterpart to D-082's `X-Harbor-Run` header) — Phase
  // 73b ships NO new filter type.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  let {
    on,
    runID,
    ontoggle
  }: {
    /** Whether trace mode (run-scoped correlation) is active. */
    on: boolean;
    /** The run id the trace narrows to; empty disables the toggle. */
    runID: string;
    /** Emitted with the requested new on-state. */
    ontoggle: (next: boolean) => void;
  } = $props();

  const disabled = $derived(runID.trim().length === 0);
</script>

<label class="trace-toggle" data-testid="trace-toggle">
  <input
    type="checkbox"
    checked={on}
    {disabled}
    data-testid="trace-toggle-input"
    onchange={(e) => ontoggle((e.currentTarget as HTMLInputElement).checked)}
  />
  <span class="label">
    Trace mode
    {#if disabled}
      <span class="hint" title="Select a topology node to scope the trace">
        — select a node
      </span>
    {:else}
      <span class="hint">— run {runID}</span>
    {/if}
  </span>
</label>

<style>
  .trace-toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
    cursor: pointer;
  }

  .hint {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
