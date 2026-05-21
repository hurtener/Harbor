<script lang="ts">
  // Settings — the left section-nav rail (Phase 73m / D-129).
  //
  // The persistent left rail listing the 12 Settings sections (page-
  // settings.md §4 "Row 1 — section-nav rail"). Clicking a section
  // anchor-scrolls the section content into view and highlights the
  // active entry. Svelte 5 runes (D-092); design tokens only.
  import { SETTINGS_SECTIONS, type SettingsSectionId } from '$lib/settings/state.svelte.js';

  let {
    active,
    onselect
  }: {
    /** The currently-active section id (drives the highlight). */
    active: SettingsSectionId;
    /** Emitted when the operator clicks a section anchor. */
    onselect: (id: SettingsSectionId) => void;
  } = $props();
</script>

<nav class="sub-nav" aria-label="Settings sections" data-testid="settings-subnav">
  <ul>
    {#each SETTINGS_SECTIONS as section (section.id)}
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
    width: var(--size-rail);
    flex-shrink: 0;
  }
  ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    position: sticky;
    top: var(--space-4);
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
