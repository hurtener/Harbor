<script lang="ts">
  // Harbor Console — Live Runtime Current Step panel (Phase 73b /
  // D-126). The right-rail sub-panel showing the session's most recent
  // planner step, derived from the live event stream (`planner.*`
  // events). NO new Protocol surface — the page passes the latest
  // planner-step summary it tracked from the SSE feed.
  //
  // Svelte 5 runes mode (D-092); design tokens only.

  let {
    step,
    detail
  }: {
    /** The current step label (e.g. an event type), or null when none. */
    step: string | null;
    /** A one-line detail for the step. */
    detail: string;
  } = $props();
</script>

<div class="current-step" data-testid="current-step-panel">
  {#if step === null}
    <p class="step-empty">No planner step observed yet.</p>
  {:else}
    <p class="step-label" data-testid="current-step-label">{step}</p>
    <p class="step-detail">{detail}</p>
  {/if}
</div>

<style>
  .current-step {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--text-sm);
  }

  .step-empty,
  .step-detail {
    margin: var(--space-0);
    color: var(--color-text-muted);
  }

  .step-label {
    margin: var(--space-0);
    color: var(--color-accent);
    font-weight: 600;
  }
</style>
