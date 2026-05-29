<script lang="ts" module>
  // Harbor Console — the app shell (D-121, CONVENTIONS.md §2, Phase 108).
  //
  // Every Console page renders inside this shell. It provides the
  // persistent sidebar (the 14-page IA in four clusters), the top bar
  // (breadcrumb + identity / connection indicator), the shared footer,
  // the content region, and an optional status-bar slot (Phase 108).
  // The `(console)` route group exists ONLY to attach this layout.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  /** One sidebar entry. */
  interface NavItem {
    label: string;
    href: string;
  }

  /** One sidebar cluster — a labelled group of nav entries. */
  interface NavCluster {
    label: string;
    items: NavItem[];
  }

  // The 14-page information architecture in four clusters (CONVENTIONS.md
  // §2). Playground rides in the Execution cluster as an execution-shaped
  // surface.
  const NAV: NavCluster[] = [
    {
      label: 'Runtime',
      items: [
        { label: 'Overview', href: '/overview' },
        { label: 'Live Runtime', href: '/live-runtime' }
      ]
    },
    {
      label: 'Execution',
      items: [
        { label: 'Sessions', href: '/sessions' },
        { label: 'Tasks', href: '/tasks' },
        { label: 'Agents', href: '/agents' },
        { label: 'Tools', href: '/tools' },
        { label: 'Events', href: '/events' },
        { label: 'Background Jobs', href: '/background-jobs' },
        { label: 'Playground', href: '/playground' }
      ]
    },
    {
      label: 'Resources',
      items: [
        { label: 'Flows', href: '/flows' },
        { label: 'Memory', href: '/memory' },
        { label: 'MCP Connections', href: '/mcp-connections' },
        { label: 'Artifacts', href: '/artifacts' }
      ]
    },
    {
      label: 'Settings',
      items: [{ label: 'Settings', href: '/settings' }]
    }
  ];
</script>

<script lang="ts">
  import { onMount, setContext } from 'svelte';
  import type { Snippet } from 'svelte';
  import { page } from '$app/stores';
  import { goto } from '$app/navigation';
  import { resolveConnection } from '$lib/connection.js';
  import ConnectionFooter from '$lib/components/ui/ConnectionFooter.svelte';

  let { children }: { children?: Snippet } = $props();

  // Phase 108 (D-167) — status-bar snippet registry.
  // Pages call setStatusBar(snippet) to render content in the shell's
  // status-bar region; calling with null clears it.
  let statusBarSnippet = $state<Snippet | null>(null);
  setContext('playgroundStatusBar', {
    set: (snippet: Snippet | null) => {
      statusBarSnippet = snippet;
    }
  });

  // The `console-hydrated` marker the Playwright harness waits on
  let hydrated = $state(false);
  onMount(() => {
    hydrated = true;
  });

  const connection = $derived(resolveConnection());

  // Phase 105 (V1.2) — first-load redirect: when nothing is attached,
  // send the operator to Settings.
  $effect(() => {
    if (resolveConnection() === null && !$page.url.pathname.startsWith('/settings')) {
      goto('/settings', { replaceState: true });
    }
  });

  // The breadcrumb is derived from the active route.
  const segments = $derived(
    $page.url.pathname.split('/').filter((s) => s.length > 0)
  );
  const crumbLabel = $derived.by(() => {
    if (segments.length === 0) return 'Overview';
    const first = segments[0];
    for (const cluster of NAV) {
      for (const item of cluster.items) {
        if (item.href === `/${first}`) return item.label;
      }
    }
    return first;
  });

  function isActive(href: string): boolean {
    return $page.url.pathname === href || $page.url.pathname.startsWith(`${href}/`);
  }
</script>

<div class="console-shell" data-testid={hydrated ? 'console-hydrated' : 'console-hydrating'}>
  <nav class="sidebar" aria-label="Console navigation">
    <div class="brand">
      <!-- Phase 108 (D-167) — Harbor brand logo + wordmark. -->
      <img
        class="brand-logo"
        src="/harbor_logo.svg"
        alt="Harbor"
        width="24"
        height="24"
      />
      <span class="brand-wordmark">Harbor Console</span>
    </div>
    {#each NAV as cluster (cluster.label)}
      <div class="nav-cluster">
        <p class="cluster-label">{cluster.label}</p>
        <ul>
          {#each cluster.items as item (item.href)}
            <li>
              <a href={item.href} class:active={isActive(item.href)}>{item.label}</a>
            </li>
          {/each}
        </ul>
      </div>
    {/each}
  </nav>

  <div class="main-column">
    <header class="top-bar">
      <nav class="breadcrumb" aria-label="Breadcrumb">
        <a href="/overview">Console</a>
        <span class="sep">/</span>
        <span class="current">{crumbLabel}</span>
      </nav>
      <div class="identity-indicator" data-testid="identity-indicator">
        {#if connection}
          <span
            class="scope-chip"
            title="tenant / user / session"
          >
            {connection.identity.tenant} · {connection.identity.user}
          </span>
          <span class="session-id mono" title={connection.identity.session}>
            {connection.identity.session}
          </span>
          <span
            class="dot connected"
            title={connection.baseURL}
            aria-label="Connected"
          ></span>
        {:else}
          <span class="scope-chip muted">no identity</span>
          <span class="dot disconnected" aria-label="Disconnected"></span>
        {/if}
      </div>
    </header>

    {#if statusBarSnippet}
      <div class="status-bar-region" data-testid="status-bar-region">
        {@render statusBarSnippet()}
      </div>
    {/if}

    <main class="content">{@render children?.()}</main>

    <ConnectionFooter />
  </div>
</div>

<style>
  .console-shell {
    display: flex;
    height: 100vh;
    overflow: hidden;
    background: var(--color-bg);
    color: var(--color-text);
    font-family: var(--font-sans);
    font-size: var(--text-base);
  }

  .sidebar {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    width: var(--size-rail);
    flex-shrink: 0;
    padding: var(--space-4);
    background: var(--color-surface);
    border-right: var(--border-hairline);
    overflow-y: auto;
  }

  .brand {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding-bottom: var(--space-2);
    border-bottom: var(--border-hairline);
  }

  .brand-logo {
    width: var(--size-avatar-sm);
    height: var(--size-avatar-sm);
    flex-shrink: 0;
  }

  .brand-wordmark {
    font-size: var(--text-lg);
    font-weight: 600;
  }

  .cluster-label {
    margin: var(--space-0) var(--space-0) var(--space-2);
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wider);
    color: var(--color-text-muted);
  }

  .nav-cluster ul {
    list-style: none;
    margin: var(--space-0);
    padding: var(--space-0);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .nav-cluster a {
    display: block;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-decoration: none;
    border-left: var(--border-emphasis-width) solid transparent;
  }

  .nav-cluster a:hover {
    color: var(--color-text);
    background: var(--color-surface-raised);
  }

  .nav-cluster a.active {
    color: var(--color-accent);
    background: var(--color-accent-soft);
    font-weight: 600;
    border-left-color: var(--color-accent);
  }

  .main-column {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
  }

  .top-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-6);
    border-bottom: var(--border-hairline);
    flex-shrink: 0;
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

  .identity-indicator {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .scope-chip {
    display: inline-flex;
    align-items: center;
    padding: var(--space-1) var(--space-2);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    font-family: var(--font-mono);
  }

  .scope-chip.muted {
    font-style: italic;
  }

  .session-id {
    max-width: var(--size-session-max-width);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .mono {
    font-family: var(--font-mono);
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

  .status-bar-region {
    flex-shrink: 0;
  }

  .content {
    flex: 1;
    padding: var(--space-6);
    min-width: 0;
    min-height: 0;
    overflow: hidden;
  }
</style>
