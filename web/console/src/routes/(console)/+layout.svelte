<script lang="ts" module>
  // Harbor Console — the app shell (D-121, CONVENTIONS.md §2).
  //
  // Every Console page renders inside this shell. It provides the
  // persistent sidebar (the 14-page IA in four clusters), the top bar
  // (breadcrumb + identity / connection indicator), the shared footer,
  // and the content region. The `(console)` route group exists ONLY to
  // attach this layout — it does not appear in the URL (CONVENTIONS.md §1).
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
  // surface — the operator's sandbox for steering / inject-context / replay
  // against the active session. Closes walkthrough F2 (Phase 83q / D-159);
  // supersedes the original D-121 stance that the Playground was off-nav.
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
  import { onMount } from 'svelte';
  import { page } from '$app/stores';
  import { goto } from '$app/navigation';
  import { resolveConnection } from '$lib/connection.js';
  import ConnectionFooter from '$lib/components/ui/ConnectionFooter.svelte';

  let { children } = $props();

  // The `console-hydrated` marker the Playwright harness waits on
  // (CLAUDE.md §17.4 — a real signal, never a fixed timeout).
  let hydrated = $state(false);
  onMount(() => {
    hydrated = true;
  });

  const connection = $derived(resolveConnection());

  // Phase 105 (V1.2) — first-load redirect: when nothing is attached,
  // send the operator to Settings (the only surface where they can fix
  // it). Idempotent — if we're already on /settings, no-op.
  $effect(() => {
    if (resolveConnection() === null && !$page.url.pathname.startsWith('/settings')) {
      goto('/settings', { replaceState: true });
    }
  });

  // The breadcrumb is derived from the active route — the first path
  // segment maps to a nav label; deeper segments append verbatim.
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
    <div class="brand">Harbor Console</div>
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
          <span class="triple" title="tenant / user / session">
            {connection.identity.tenant} · {connection.identity.user} ·
            {connection.identity.session}
          </span>
          <span class="runtime-url" title={connection.baseURL}>{connection.baseURL}</span>
          <span class="dot connected" aria-label="Connected"></span>
        {:else}
          <span class="triple muted">no identity</span>
          <span class="dot disconnected" aria-label="Disconnected"></span>
        {/if}
      </div>
    </header>

    <main class="content">{@render children?.()}</main>

    <ConnectionFooter />
  </div>
</div>

<style>
  .console-shell {
    display: flex;
    min-height: 100vh;
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
  }

  .brand {
    font-size: var(--text-lg);
    font-weight: 600;
    padding-bottom: var(--space-2);
    border-bottom: var(--border-hairline);
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
  }

  .nav-cluster a:hover {
    color: var(--color-text);
    background: var(--color-surface-raised);
  }

  .nav-cluster a.active {
    color: var(--color-accent);
    background: var(--color-accent-soft);
    font-weight: 600;
  }

  .main-column {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-width: 0;
  }

  .top-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    padding: var(--space-3) var(--space-6);
    border-bottom: var(--border-hairline);
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

  .triple {
    font-family: var(--font-mono);
  }

  .triple.muted {
    font-style: italic;
  }

  .runtime-url {
    font-family: var(--font-mono);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: 50%;
  }

  .dot.connected {
    background: var(--color-success);
  }

  .dot.disconnected {
    background: var(--color-danger);
  }

  .content {
    flex: 1;
    padding: var(--space-6);
    min-width: 0;
  }
</style>
