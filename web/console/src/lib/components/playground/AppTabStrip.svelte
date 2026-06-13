<script lang="ts">
  // Harbor Console — Playground `fullscreen` tab strip (DisplayMode layout).
  //
  // The tab strip shown when one or more MCP Apps are in `fullscreen` mode: a
  // non-closable Chat tab plus one closable tab per fullscreen app. Selecting a
  // tab activates its region; closing an app tab removes it and (in the parent)
  // returns focus to Chat. Multiple fullscreen apps yield multiple tabs (D-062).
  //
  // Custom-built (not a Skeleton primitive): the Console uses NO Skeleton
  // components anywhere — Skeleton v3's Tailwind-utility-class Tabs would break
  // the `.stylelintrc.cjs` token-only rule and fragment the hand-built `ui/`
  // inventory (CLAUDE.md §4.5 rule 4, D-092). This is a thin token-styled
  // tablist over the shared layout state.
  //
  // Design tokens only.
  import type { LayoutTab } from './layout.js';

  let {
    tabs,
    activeTabId,
    onactivate,
    onclose
  }: {
    /** The ordered tab set (Chat first, then fullscreen apps). */
    tabs: LayoutTab[];
    /** The currently-active tab id. */
    activeTabId: string;
    onactivate: (id: string) => void;
    /** Emitted when an app tab's close affordance is used. */
    onclose: (id: string) => void;
  } = $props();
</script>

<div class="tabstrip" role="tablist" data-testid="app-tabstrip" aria-label="Open apps">
  {#each tabs as tab (tab.id)}
    <div class="tab" data-active={tab.id === activeTabId} data-tab-id={tab.id}>
      <button
        type="button"
        class="tab-btn"
        role="tab"
        aria-selected={tab.id === activeTabId}
        data-testid="app-tab"
        data-tab-id={tab.id}
        onclick={() => onactivate(tab.id)}
      >
        {tab.label}
      </button>
      {#if tab.closable}
        <button
          type="button"
          class="tab-close"
          data-testid="app-tab-close"
          data-tab-id={tab.id}
          aria-label="Close {tab.label}"
          onclick={() => onclose(tab.id)}
        >
          ×
        </button>
      {/if}
    </div>
  {/each}
</div>

<style>
  .tabstrip {
    display: flex;
    align-items: stretch;
    gap: var(--space-1);
    border-bottom: var(--border-hairline);
  }

  .tab {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-bottom: none;
    border-top-left-radius: var(--radius-sm);
    border-top-right-radius: var(--radius-sm);
    background: var(--color-surface);
  }

  .tab[data-active='true'] {
    background: var(--color-accent-soft);
    border-color: var(--color-accent);
  }

  .tab-btn {
    background: none;
    border: none;
    color: var(--color-text-muted);
    font-size: var(--text-xs);
    cursor: pointer;
    padding: var(--space-0);
  }

  .tab[data-active='true'] .tab-btn {
    color: var(--color-accent);
  }

  .tab-close {
    background: none;
    border: none;
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    line-height: 1;
    cursor: pointer;
    padding: var(--space-0);
  }

  .tab-close:hover {
    color: var(--color-danger);
  }
</style>
