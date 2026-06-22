<script lang="ts">
  // Harbor Console — agent-config two-revision diff view (92a).
  //
  // Renders the server-side `AgentConfigDiff`: a TEXT delta for the prompt
  // layers (base / user, before → after) and a structured SET-diff for
  // skills, MCP tool-exposure, and runtime-added connections. Page-specific
  // component; composes the shared StatusChip. Tokens only; runes (D-092).
  import { StatusChip } from '$lib/components/ui';
  import type { AgentConfigDiff } from '$lib/protocol/agentconfig.js';

  let { diff }: { diff: AgentConfigDiff } = $props();

  const hasSkillChanges = $derived(
    (diff.skills.added?.length ?? 0) > 0 || (diff.skills.removed?.length ?? 0) > 0
  );
  const hasExposureChanges = $derived(
    (diff.tool_exposure.paused_added?.length ?? 0) > 0 ||
      (diff.tool_exposure.paused_resumed?.length ?? 0) > 0 ||
      (diff.tool_exposure.disabled_added?.length ?? 0) > 0 ||
      (diff.tool_exposure.disabled_enabled?.length ?? 0) > 0
  );
  const hasConnChanges = $derived(
    (diff.connections.added?.length ?? 0) > 0 || (diff.connections.removed?.length ?? 0) > 0
  );
  const hasPromptChanges = $derived(diff.prompt_layers.base_changed || diff.prompt_layers.user_changed);
  const hasLlmChanges = $derived(
    diff.llm_params.model_changed ||
      diff.llm_params.temperature_changed ||
      diff.llm_params.max_tokens_changed ||
      diff.llm_params.reasoning_effort_changed
  );
  /** The per-field model-&-sampling deltas to render (only the changed ones).
   * An unset value renders as "(inherit)". */
  const llmDeltas = $derived(
    [
      {
        label: 'Model',
        changed: diff.llm_params.model_changed,
        from: diff.llm_params.model_from,
        to: diff.llm_params.model_to
      },
      {
        label: 'Temperature',
        changed: diff.llm_params.temperature_changed,
        from: diff.llm_params.temperature_from,
        to: diff.llm_params.temperature_to
      },
      {
        label: 'Max tokens',
        changed: diff.llm_params.max_tokens_changed,
        from: diff.llm_params.max_tokens_from,
        to: diff.llm_params.max_tokens_to
      },
      {
        label: 'Reasoning effort',
        changed: diff.llm_params.reasoning_effort_changed,
        from: diff.llm_params.reasoning_effort_from,
        to: diff.llm_params.reasoning_effort_to
      }
    ].filter((d) => d.changed)
  );
  const hasAnyChange = $derived(
    hasSkillChanges || hasExposureChanges || hasConnChanges || hasPromptChanges || hasLlmChanges
  );
</script>

<div class="diff" data-testid="agentcfg-diff">
  {#if !hasAnyChange}
    <p class="diff-empty" data-testid="agentcfg-diff-empty">
      The two revisions are identical — no configuration changed.
    </p>
  {/if}

  {#if hasPromptChanges}
    <section class="diff-section" data-testid="agentcfg-diff-prompt">
      <h4 class="diff-title">Prompt layers</h4>
      {#if diff.prompt_layers.base_changed}
        <div class="text-delta">
          <span class="delta-label">Base</span>
          <pre class="delta from">{diff.prompt_layers.base_from ?? '(empty)'}</pre>
          <pre class="delta to">{diff.prompt_layers.base_to ?? '(empty)'}</pre>
        </div>
      {/if}
      {#if diff.prompt_layers.user_changed}
        <div class="text-delta">
          <span class="delta-label">User</span>
          <pre class="delta from">{diff.prompt_layers.user_from ?? '(empty)'}</pre>
          <pre class="delta to">{diff.prompt_layers.user_to ?? '(empty)'}</pre>
        </div>
      {/if}
    </section>
  {/if}

  {#if hasSkillChanges}
    <section class="diff-section" data-testid="agentcfg-diff-skills">
      <h4 class="diff-title">Skills</h4>
      <div class="chip-row">
        {#each diff.skills.added ?? [] as name (name)}
          <StatusChip kind="success" label={`+ ${name}`} />
        {/each}
        {#each diff.skills.removed ?? [] as name (name)}
          <StatusChip kind="danger" label={`− ${name}`} />
        {/each}
      </div>
    </section>
  {/if}

  {#if hasExposureChanges}
    <section class="diff-section" data-testid="agentcfg-diff-exposure">
      <h4 class="diff-title">MCP tool exposure</h4>
      <div class="chip-row">
        {#each diff.tool_exposure.paused_added ?? [] as s (s)}
          <StatusChip kind="warning" label={`paused ${s}`} />
        {/each}
        {#each diff.tool_exposure.paused_resumed ?? [] as s (s)}
          <StatusChip kind="success" label={`resumed ${s}`} />
        {/each}
        {#each diff.tool_exposure.disabled_added ?? [] as t (t)}
          <StatusChip kind="danger" label={`disabled ${t}`} />
        {/each}
        {#each diff.tool_exposure.disabled_enabled ?? [] as t (t)}
          <StatusChip kind="success" label={`enabled ${t}`} />
        {/each}
      </div>
    </section>
  {/if}

  {#if hasConnChanges}
    <section class="diff-section" data-testid="agentcfg-diff-connections">
      <h4 class="diff-title">MCP connections</h4>
      <div class="chip-row">
        {#each diff.connections.added ?? [] as name (name)}
          <StatusChip kind="success" label={`+ ${name}`} />
        {/each}
        {#each diff.connections.removed ?? [] as name (name)}
          <StatusChip kind="danger" label={`− ${name}`} />
        {/each}
      </div>
    </section>
  {/if}

  {#if hasLlmChanges}
    <section class="diff-section" data-testid="agentcfg-diff-llm">
      <h4 class="diff-title">Model &amp; sampling</h4>
      <div class="llm-deltas">
        {#each llmDeltas as d (d.label)}
          <div class="llm-delta" data-testid="agentcfg-diff-llm-row">
            <span class="delta-label">{d.label}</span>
            <code class="delta from">{d.from === undefined || d.from === '' ? '(inherit)' : d.from}</code>
            <span class="arrow" aria-hidden="true">→</span>
            <code class="delta to">{d.to === undefined || d.to === '' ? '(inherit)' : d.to}</code>
          </div>
        {/each}
      </div>
    </section>
  {/if}
</div>

<style>
  .diff {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .diff-empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
  .diff-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .diff-title {
    margin: var(--space-0);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  .chip-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
  }
  .text-delta {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-1) var(--space-2);
    align-items: start;
  }
  .delta-label {
    grid-column: 1 / -1;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }
  .delta {
    margin: var(--space-0);
    padding: var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    white-space: pre-wrap;
    word-break: break-word;
    overflow-x: auto;
  }
  .delta.from {
    background: var(--color-danger-soft);
  }
  .delta.to {
    background: var(--color-success-soft);
  }
  .llm-deltas {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .llm-delta {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .llm-delta .delta-label {
    min-width: var(--size-nav);
  }
  .arrow {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }
</style>
