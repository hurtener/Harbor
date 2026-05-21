<script lang="ts" module>
  // Harbor Console — Overview `+ New` quick-create menu (Phase 73a / D-127).
  //
  // The `+ New` button (page-overview.md §12 — "Top bar adds a + New
  // button"). It is a Console-LOCAL navigation surface: every menu item
  // deep-links into a per-page create flow owned by that page's phase
  // plan — the Overview only provides the menu (acceptance criterion).
  //
  // Each item routes through an unprefixed Console route (CONVENTIONS.md
  // §1). The menu mints NO Protocol traffic — opening it is pure
  // client-side navigation; the target page owns the actual create
  // flow + its Protocol calls.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  /** One quick-create menu item. */
  interface NewMenuItem {
    /** The unprefixed Console route the item deep-links to. */
    href: string;
    /** The menu label. */
    label: string;
    /** Stable test id. */
    testid: string;
  }

  /**
   * The quick-create menu items — page-overview.md §12. Each deep-links
   * into the create flow its owning page's phase delivers; the Overview
   * provides the menu only.
   */
  export const NEW_MENU_ITEMS: readonly NewMenuItem[] = [
    { href: '/sessions', label: 'New session', testid: 'new-menu-session' },
    { href: '/playground', label: 'Open Playground', testid: 'new-menu-playground' },
    { href: '/flows', label: 'Run flow', testid: 'new-menu-flow' },
    { href: '/mcp-connections', label: 'Add MCP server', testid: 'new-menu-mcp' },
    { href: '/settings', label: 'Connect runtime', testid: 'new-menu-runtime' }
  ] as const;
</script>

<script lang="ts">
  let open = $state(false);

  function toggle(): void {
    open = !open;
  }

  function close(): void {
    open = false;
  }
</script>

<div class="new-menu" data-testid="new-menu">
  <button
    type="button"
    class="trigger"
    data-testid="new-menu-trigger"
    aria-expanded={open}
    aria-haspopup="menu"
    onclick={toggle}
  >
    + New
  </button>
  {#if open}
    <div
      class="menu"
      role="menu"
      tabindex="-1"
      data-testid="new-menu-list"
      onmouseleave={close}
    >
      {#each NEW_MENU_ITEMS as item (item.href)}
        <a
          class="item"
          role="menuitem"
          href={item.href}
          data-testid={item.testid}
          onclick={close}
        >
          {item.label}
        </a>
      {/each}
    </div>
  {/if}
</div>

<style>
  .new-menu {
    position: relative;
    display: inline-flex;
  }

  .trigger {
    background: var(--color-accent);
    color: var(--color-bg);
    border: none;
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-sm);
    font-weight: 600;
    cursor: pointer;
  }

  .menu {
    position: absolute;
    top: calc(100% + var(--space-1));
    right: var(--space-0);
    z-index: 1;
    display: flex;
    flex-direction: column;
    min-width: var(--size-card-min);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .item {
    padding: var(--space-2) var(--space-3);
    font-size: var(--text-sm);
    color: var(--color-text);
    text-decoration: none;
  }

  .item:hover {
    background: var(--color-surface-raised);
  }
</style>
