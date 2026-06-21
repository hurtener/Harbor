<script lang="ts">
  // Harbor Console — agent-config skills management (92c).
  //
  // Lists the agent's skills (metadata only), and offers add / remove via
  // `agent_config.skills.upsert` / `.delete`. A pack-overwrite refusal from
  // the runtime surfaces as a clear inline error (never a silent failure —
  // CLAUDE.md §13). Writes are admin-gated (disabled-with-tooltip for
  // non-admin — the 92b precedent). Page-specific; composes the shared
  // StatusChip. Tokens only; Svelte 5 runes (D-092).
  import { StatusChip } from '$lib/components/ui';
  import type { AgentConfigPanelState } from '$lib/agentconfig/state.svelte.js';

  let { panel }: { panel: AgentConfigPanelState } = $props();
</script>

<div class="card-body" data-testid="agentcfg-skills">
  <p class="note">
    The agent&rsquo;s skills. Adding or removing one records a config revision
    and applies on the agent&rsquo;s next run.
  </p>

  {#if panel.skills.length === 0}
    <p class="note" data-testid="agentcfg-skills-empty">No skills configured for this agent.</p>
  {:else}
    <ul class="skill-list">
      {#each panel.skills as skill (skill.name)}
        <li class="skill-row" data-testid="agentcfg-skill-row">
          <div class="skill-main">
            <span class="skill-name">{skill.title || skill.name}</span>
            <StatusChip kind="neutral" label={skill.origin} />
            <StatusChip kind="info" label={skill.scope} />
            {#if skill.trigger}
              <span class="skill-trigger">{skill.trigger}</span>
            {/if}
          </div>
          <button
            type="button"
            class="ghost danger"
            data-testid="agentcfg-skill-delete"
            disabled={!panel.hasAdminScope}
            title={panel.hasAdminScope
              ? 'Remove this skill'
              : 'Removing a skill requires the admin scope claim'}
            onclick={() => void panel.deleteSkill(skill.name)}
          >
            Remove
          </button>
        </li>
      {/each}
    </ul>
  {/if}

  <form
    class="add-skill"
    data-testid="agentcfg-add-skill"
    onsubmit={(e) => {
      e.preventDefault();
      void panel.addSkill();
    }}
  >
    <div class="form-grid">
      <label class="field">
        <span class="field-label">Name</span>
        <input
          type="text"
          data-testid="agentcfg-skill-name"
          placeholder="summarise-thread"
          bind:value={panel.skillName}
          disabled={!panel.hasAdminScope}
        />
      </label>
      <label class="field">
        <span class="field-label">Title</span>
        <input
          type="text"
          data-testid="agentcfg-skill-title"
          placeholder="Summarise a thread"
          bind:value={panel.skillTitle}
          disabled={!panel.hasAdminScope}
        />
      </label>
      <label class="field field-wide">
        <span class="field-label">Trigger</span>
        <input
          type="text"
          data-testid="agentcfg-skill-trigger"
          placeholder="When the user asks for a recap of the conversation"
          bind:value={panel.skillTrigger}
          disabled={!panel.hasAdminScope}
        />
      </label>
      <label class="field field-wide">
        <span class="field-label">Steps (one per line)</span>
        <textarea
          rows="3"
          data-testid="agentcfg-skill-steps"
          placeholder={'Read the recent turns\nGroup by topic\nWrite a 3-bullet recap'}
          bind:value={panel.skillSteps}
          disabled={!panel.hasAdminScope}
        ></textarea>
      </label>
    </div>
    <div class="actions">
      <button
        type="submit"
        class="primary"
        data-testid="agentcfg-skill-add"
        disabled={!panel.hasAdminScope || panel.skillBusy}
        title={panel.hasAdminScope
          ? 'Add this skill (records a revision)'
          : 'Adding a skill requires the admin scope claim'}
      >
        {panel.skillBusy ? 'Adding…' : 'Add skill'}
      </button>
      {#if !panel.hasAdminScope}
        <span class="hint" data-testid="agentcfg-skills-disabled-hint">
          Requires the admin scope claim.
        </span>
      {/if}
    </div>
    {#if panel.skillsError}
      <p class="form-error" data-testid="agentcfg-skills-error">
        {panel.skillsError.code}: {panel.skillsError.message}
      </p>
    {/if}
  </form>
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
  .skill-list {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .skill-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface);
  }
  .skill-main {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    min-width: 0;
  }
  .skill-name {
    font-weight: 600;
    color: var(--color-text);
  }
  .skill-trigger {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .add-skill {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding-top: var(--space-2);
    border-top: var(--border-hairline);
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
  .ghost {
    padding: var(--space-1) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-size: var(--text-xs);
    cursor: pointer;
    background: var(--color-surface);
    color: var(--color-text);
  }
  .ghost.danger {
    color: var(--color-danger);
  }
  .primary:disabled,
  .ghost:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .hint {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
  .form-error {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-danger);
  }
</style>
