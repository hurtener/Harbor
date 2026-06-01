<script lang="ts">
  // Settings — the left section-nav rail (Phase 73m / D-129; tidied for
  // the Phase 108f single-section model / D-178).
  //
  // The persistent left rail listing the 12 Settings sections (page-
  // settings.md §4 "section-nav rail"). Clicking a section selects it; the
  // right pane renders ONLY that section. The rail is lightly grouped: the
  // Console-local sections first, a hairline divider + a "Runtime"
  // sub-heading, then the read-only runtime-posture sections. Svelte 5
  // runes (D-092); design tokens only.
  import {
    consoleLocalSections,
    runtimePostureSections,
    type SettingsSectionId
  } from '$lib/settings/state.svelte.js';

  let {
    active,
    onselect
  }: {
    /** The currently-active section id (drives the highlight). */
    active: SettingsSectionId;
    /** Emitted when the operator clicks a section anchor. */
    onselect: (id: SettingsSectionId) => void;
  } = $props();

  const localSections = consoleLocalSections();
  const postureSections = runtimePostureSections();
</script>

<nav class="sub-nav" aria-label="Settings sections" data-testid="settings-subnav">
  <ul>
    {#each localSections as section (section.id)}
      <li>
        <button
          type="button"
          class="nav-item"
          class:active={active === section.id}
          data-testid="settings-subnav-{section.id}"
          aria-current={active === section.id ? 'true' : 'false'}
          onclick={() => onselect(section.id)}
        >
          {section.label}
        </button>
      </li>
    {/each}
  </ul>

  <hr class="divider" />
  <p class="group-heading">Runtime</p>

  <ul>
    {#each postureSections as section (section.id)}
      <li>
        <button
          type="button"
          class="nav-item"
          class:active={active === section.id}
          data-testid="settings-subnav-{section.id}"
          aria-current={active === section.id ? 'true' : 'false'}
          onclick={() => onselect(section.id)}
        >
          {section.label}
        </button>
      </li>
    {/each}
  </ul>
</nav>

<style>
  .sub-nav {
    width: var(--size-nav);
    flex-shrink: 0;
    position: sticky;
    top: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .divider {
    border: none;
    border-top: var(--border-hairline);
    margin: var(--space-2) var(--space-0);
    width: 100%;
  }
  .group-heading {
    margin: var(--space-0) var(--space-0) var(--space-1);
    padding: var(--space-0) var(--space-3);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
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
