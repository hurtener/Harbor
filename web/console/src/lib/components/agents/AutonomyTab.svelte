<script lang="ts">
  // Harbor Console — Agents detail Autonomy & Planner tab (Phase 73e /
  // D-124). The configured planner choice + planner config (MaxSteps,
  // repair policy, etc.) + model policy, from the `agents.get`
  // AgentConfig projection. Read-only inspector. Page-specific component.
  import type { AgentConfig } from '$lib/protocol/agents.js';

  let { config }: { config: AgentConfig } = $props();

  const plannerEntries = $derived(
    Object.entries(config.planner_config ?? {})
  );
  const modelEntries = $derived(Object.entries(config.model_policy ?? {}));
</script>

<div class="autonomy" data-testid="agent-autonomy-tab">
  <dl class="kv">
    <dt>Planner</dt>
    <dd data-testid="autonomy-planner">{config.planner_type || '—'}</dd>
    <dt>Model</dt>
    <dd>{config.model || '—'}</dd>
    <dt>Max steps</dt>
    <dd>{config.max_steps > 0 ? config.max_steps : '—'}</dd>
  </dl>

  {#if plannerEntries.length > 0}
    <section>
      <h4>Planner config</h4>
      <dl class="kv">
        {#each plannerEntries as [k, v] (k)}
          <dt>{k}</dt>
          <dd>{v}</dd>
        {/each}
      </dl>
    </section>
  {/if}

  {#if modelEntries.length > 0}
    <section>
      <h4>Model policy</h4>
      <dl class="kv">
        {#each modelEntries as [k, v] (k)}
          <dt>{k}</dt>
          <dd>{v}</dd>
        {/each}
      </dl>
    </section>
  {/if}
</div>

<style>
  .autonomy {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .kv {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-2) var(--space-4);
    margin: var(--space-0);
  }

  h4 {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  dt {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  dd {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text);
  }
</style>
