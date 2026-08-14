<script lang="ts">
  // Agent Config — the left section-nav rail.
  //
  // Mirrors the Settings page's sub-nav rail vocabulary (CONVENTIONS.md §3,
  // the single-section model): a persistent left rail listing the seven
  // control-plane areas. Clicking an area selects it; the right pane renders
  // ONLY that area. An optional `header` snippet slot carries the agent
  // selector at the top of the rail. Svelte 5 runes (D-092); design tokens
  // only. The visual treatment is identical to `settings/SubNavRail.svelte`
  // — there is no shared generic rail primitive, so each multi-section
  // surface ships its own thin rail over the same token surface.
  import type { Snippet } from 'svelte';
  import { AGENT_CONFIG_AREAS, type AgentConfigAreaId } from '$lib/agentconfig/state.svelte.js';

  let {
    active,
    onselect,
    header
  }: {
    /** The currently-active area id (drives the highlight). */
    active: AgentConfigAreaId;
    /** Emitted when the operator clicks an area anchor. */
    onselect: (id: AgentConfigAreaId) => void;
    /** Optional content rendered above the nav list (the agent selector). */
    header?: Snippet;
  } = $props();
</script>

<nav class="sub-nav" aria-label="Agent config areas" data-testid="agent-config-subnav">
  {#if header}
    <div class="rail-header">{@render header()}</div>
  {/if}

  <ul>
    {#each AGENT_CONFIG_AREAS as area (area.id)}
      <li>
        <button
          type="button"
          class="nav-item"
          class:active={active === area.id}
          data-testid="agent-config-subnav-{area.id}"
          aria-current={active === area.id ? 'true' : 'false'}
          onclick={() => onselect(area.id)}
        >
          {area.label}
        </button>
      </li>
    {/each}
  </ul>
</nav>

<style>
  .sub-nav {
    width: 100%;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .rail-header {
    display: flex;
    flex-direction: column;
  }
  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .nav-item {
    width: 100%;
    text-align: left;
    padding: var(--space-2) var(--space-3);
    background: transparent;
    border: var(--border-hairline);
    border-color: transparent;
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    cursor: pointer;
  }
  .nav-item:hover {
    background: var(--color-surface);
    color: var(--color-text);
  }
  .nav-item.active {
    background: var(--color-surface-raised);
    border-color: var(--color-border);
    color: var(--color-text);
    font-weight: 600;
  }
</style>
