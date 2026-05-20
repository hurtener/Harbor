<script lang="ts">
  // Harbor Console — Agents detail Skills tab (Phase 73e / D-124). The
  // agent's attached skills (Phase 38 imported + Phase 41 generated),
  // from `agents.skills`. Page-specific component.
  import { StatusChip } from '$lib/components/ui';
  import type { AgentSkillBinding } from '$lib/protocol/agents.js';

  let { skills }: { skills: AgentSkillBinding[] } = $props();
</script>

<div class="skills-tab" data-testid="agent-skills-tab">
  {#if skills.length === 0}
    <p class="empty">No skills attached to this agent.</p>
  {:else}
    <ul>
      {#each skills as skill (skill.skill_id)}
        <li data-testid="agent-skill-row">
          <span class="skill-name">{skill.name || skill.skill_id}</span>
          <StatusChip
            kind={skill.generated ? 'accent' : 'neutral'}
            label={skill.generated ? 'generated' : 'imported'}
          />
        </li>
      {/each}
    </ul>
  {/if}
</div>

<style>
  .skills-tab {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
  }

  .skill-name {
    font-size: var(--text-sm);
    color: var(--color-text);
  }

  .empty {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }
</style>
