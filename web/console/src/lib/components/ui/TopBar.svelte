<script lang="ts">
  // Harbor Console — the app-shell top bar (Phase 108b chrome).
  //
  // Renders on every page (the `(console)/+layout.svelte` shell): the
  // sidebar-collapse hamburger, the route breadcrumb, the ⌘K global search
  // launcher, a help link, and the identity avatar + connection popover.
  //
  // The mock's notification bell and theme toggle are OMITTED (Phase 108b
  // operator decision): there is no notifications Protocol feed and
  // `tokens.css` is dark-only — neither can be wired to real state, and the
  // page-polish procedure forbids faking. They are deferred chrome items.
  //
  // Every datum here is real — the breadcrumb from the route, the identity
  // from `connection.ts`. Nothing is synthesised. Svelte 5 runes (D-092);
  // design tokens only (CLAUDE.md §4.5).
  import { onMount } from 'svelte';
  import Menu from '@lucide/svelte/icons/menu';
  import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
  import type { RuntimeConnection } from '$lib/connection.js';
  import GlobalSearch from './GlobalSearch.svelte';

  let {
    crumbLabel,
    connection,
    collapsed,
    onToggleCollapse
  }: {
    /** The active page's breadcrumb label (derived from the NAV constant). */
    crumbLabel: string;
    /** The resolved Runtime connection, or null when not attached. */
    connection: RuntimeConnection | null;
    /** Whether the sidebar is collapsed to icons-only. */
    collapsed: boolean;
    /** Toggle the sidebar collapse state. */
    onToggleCollapse: () => void;
  } = $props();

  // The Harbor project home — the help affordance's target (the canonical
  // docs/readme home for this module path). Opens in a new tab.
  const HELP_URL = 'https://github.com/hurtener/Harbor#readme';

  let popoverOpen = $state(false);

  // Avatar initials from the operator's user id (real identity, D-160).
  const initials = $derived.by(() => {
    const u = connection?.identity.user ?? '';
    const cleaned = u.replace(/[^a-zA-Z0-9]/g, '');
    return cleaned.slice(0, 2).toUpperCase() || '—';
  });

  function togglePopover() {
    popoverOpen = !popoverOpen;
  }

  onMount(() => {
    const onDocClick = (e: MouseEvent) => {
      const t = e.target as HTMLElement;
      if (!t.closest('[data-testid="identity-menu"]')) popoverOpen = false;
    };
    window.addEventListener('click', onDocClick);
    return () => window.removeEventListener('click', onDocClick);
  });
</script>

<header class="top-bar">
  <div class="left">
    <button
      type="button"
      class="icon-btn"
      data-testid="nav-collapse-toggle"
      onclick={onToggleCollapse}
      aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
      title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
    >
      <Menu size={18} aria-hidden="true" />
    </button>
    <nav class="breadcrumb" aria-label="Breadcrumb">
      <a href="/overview">Console</a>
      <span class="sep">/</span>
      <span class="current">{crumbLabel}</span>
    </nav>
  </div>

  <div class="right">
    <GlobalSearch />

    <a
      class="icon-btn"
      href={HELP_URL}
      target="_blank"
      rel="noreferrer"
      title="Help &amp; documentation"
      aria-label="Help and documentation"
    >
      <CircleQuestionMark size={18} aria-hidden="true" />
    </a>

    <div class="identity" data-testid="identity-menu">
      <button
        type="button"
        class="avatar"
        data-testid="identity-avatar"
        onclick={togglePopover}
        aria-haspopup="true"
        aria-expanded={popoverOpen}
        aria-label="Identity and connection"
      >
        <span class="avatar-initials" class:muted={!connection}>{initials}</span>
        <span
          class="dot"
          class:connected={!!connection}
          class:disconnected={!connection}
          aria-hidden="true"
        ></span>
      </button>

      {#if popoverOpen}
        <div class="popover" role="menu" data-testid="identity-popover">
          {#if connection}
            <dl>
              <dt>Tenant</dt>
              <dd class="mono">{connection.identity.tenant}</dd>
              <dt>User</dt>
              <dd class="mono">{connection.identity.user}</dd>
              <dt>Session</dt>
              <dd class="mono">{connection.identity.session || '— (per-request)'}</dd>
              <dt>Runtime</dt>
              <dd class="mono">{connection.baseURL}</dd>
            </dl>
            <a class="popover-link" href="/settings">Connected runtimes →</a>
          {:else}
            <p class="muted">Not connected to a Harbor Runtime.</p>
            <a class="popover-link" href="/settings">Attach a Runtime →</a>
          {/if}
        </div>
      {/if}
    </div>
  </div>
</header>

<style>
  .top-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-2) var(--space-6);
    border-bottom: var(--border-hairline);
    flex-shrink: 0;
  }

  .left,
  .right {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }

  .icon-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-1);
    background: transparent;
    border: none;
    border-radius: var(--radius-sm);
    color: var(--color-text-muted);
    cursor: pointer;
    text-decoration: none;
  }

  .icon-btn:hover {
    color: var(--color-text);
    background: var(--color-surface-raised);
  }

  .breadcrumb {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-sm);
  }

  .breadcrumb a {
    color: var(--color-text-muted);
    text-decoration: none;
  }

  .breadcrumb .sep {
    color: var(--color-text-muted);
  }

  .breadcrumb .current {
    color: var(--color-text);
    font-weight: 600;
  }

  .identity {
    position: relative;
  }

  .avatar {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    background: transparent;
    border: var(--border-hairline);
    border-radius: var(--radius-pill);
    cursor: pointer;
  }

  .avatar:hover {
    border-color: var(--color-accent);
  }

  .avatar-initials {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-sm);
    height: var(--size-avatar-sm);
    border-radius: 50%;
    background: var(--color-accent);
    color: var(--color-bg);
    font-size: var(--text-xs);
    font-weight: 600;
  }

  .avatar-initials.muted {
    background: var(--color-text-muted);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: 50%;
    flex-shrink: 0;
  }

  .dot.connected {
    background: var(--color-success);
  }

  .dot.disconnected {
    background: var(--color-danger);
  }

  .popover {
    position: absolute;
    top: calc(100% + var(--space-2));
    right: 0;
    min-width: var(--size-popover-min-width);
    padding: var(--space-3) var(--space-4);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    font-size: var(--text-xs);
  }

  .popover dl {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: var(--space-1) var(--space-3);
    margin: var(--space-0);
  }

  .popover dt {
    color: var(--color-text-muted);
  }

  .popover dd {
    margin: var(--space-0);
    color: var(--color-text);
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .popover .muted {
    margin: var(--space-0);
    color: var(--color-text-muted);
  }

  .popover-link {
    display: inline-block;
    margin-top: var(--space-3);
    color: var(--color-accent);
    text-decoration: none;
    font-size: var(--text-sm);
  }

  .mono {
    font-family: var(--font-mono);
  }
</style>
