<script lang="ts">
  // Harbor Console — Tasks page right-rail Cost Breakdown card body
  // (Phase 73d / D-123). Renders the `tasks.get` per-step cost rollup
  // aggregated from `llm.cost.recorded` events. Page-specific; design
  // tokens only.
  import type { TaskDetail } from '$lib/protocol/tasks.js';

  let { detail }: { detail: TaskDetail | null } = $props();
</script>

{#if detail !== null}
  <div class="cost-card" data-testid="rail-cost-breakdown">
    <dl class="totals">
      <div><dt>Total tokens</dt><dd>{detail.cost.total_tokens}</dd></div>
      <div><dt>Prompt</dt><dd>{detail.cost.prompt_tokens}</dd></div>
      <div><dt>Output</dt><dd>{detail.cost.output_tokens}</dd></div>
      <div><dt>USD</dt><dd>${detail.cost.usd.toFixed(4)}</dd></div>
    </dl>
    {#if detail.cost.per_step.length > 0}
      <ul class="steps">
        {#each detail.cost.per_step as step (step.step_index)}
          <li>
            <span>Step {step.step_index}</span>
            <span>{step.tokens} tok · ${step.usd.toFixed(4)}</span>
          </li>
        {/each}
      </ul>
    {:else}
      <p class="muted">No per-step cost recorded.</p>
    {/if}
  </div>
{:else}
  <p class="muted">Select a task to see its cost rollup.</p>
{/if}

<style>
  .cost-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .totals {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: var(--space-0);
  }

  .totals div {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .totals dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .totals dd {
    margin: var(--space-0);
    font-size: var(--text-xs);
  }

  .steps {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .steps li {
    display: flex;
    justify-content: space-between;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text);
  }

  .muted {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
