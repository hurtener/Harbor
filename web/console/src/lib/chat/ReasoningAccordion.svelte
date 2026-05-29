<script lang="ts">
  // Chat module — ReasoningAccordion (Phase 107a).
  //
  // Renders a collapsible list of the model's reasoning-trace steps
  // above an agent bubble. Collapsed by default — the operator opts in
  // per bubble. Design-token-only per CONVENTIONS.md §1.
  //
  // Renders nothing when `steps` is empty or undefined.
  import type { ReasoningStep } from './types.js';

  let {
    steps
  }: {
    steps?: ReasoningStep[];
  } = $props();

  let open = $state(false);
</script>

{#if steps && steps.length > 0}
  <div class="reasoning-accordion" data-testid="reasoning-accordion">
    <button
      class="reasoning-toggle"
      type="button"
      aria-expanded={open}
      onclick={() => (open = !open)}
    >
      Reasoning ({steps.length} step{steps.length === 1 ? '' : 's'})
      <span class="toggle-chevron" aria-hidden="true">{open ? '▾' : '▸'}</span>
    </button>

    {#if open}
      <ol class="reasoning-list">
        {#each steps as step (step.index)}
          <li class="reasoning-step">
            <span class="step-label">Step {step.index}</span>
            <pre class="step-trace">{step.reasoning_trace}</pre>
          </li>
        {/each}
      </ol>
    {/if}
  </div>
{/if}

<style>
  .reasoning-accordion {
    margin-bottom: var(--space-2);
  }

  .reasoning-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text-muted);
    cursor: pointer;
  }

  .reasoning-toggle:hover {
    background: var(--color-surface-raised);
  }

  .toggle-chevron {
    font-size: var(--text-sm);
  }

  .reasoning-list {
    margin: var(--space-1) 0 0;
    padding-left: var(--space-5);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .reasoning-step {
    display: flex;
    flex-direction: column;
    gap: var(--space-05);
  }

  .step-label {
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-text-muted);
  }

  .step-trace {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--color-text);
    white-space: pre-wrap;
    word-break: break-word;
    background: var(--color-surface);
    padding: var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
  }
</style>
