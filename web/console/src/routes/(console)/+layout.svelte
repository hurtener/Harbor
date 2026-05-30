<script lang="ts" module>
  // Harbor Console — the app shell (D-121, CONVENTIONS.md §2, Phase 108 / 108b).
  //
  // Every Console page renders inside this shell. It provides the
  // persistent sidebar (the 14-page IA in four clusters, each item with a
  // lucide icon — Phase 108b), the top bar (hamburger collapse, breadcrumb,
  // ⌘K search, help, identity avatar — `TopBar.svelte`), the single global
  // status bar (`AppStatusBar`), and the content region. The `(console)`
  // route group exists ONLY to attach this layout.
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import type { Component } from 'svelte';
  import LayoutDashboard from '@lucide/svelte/icons/layout-dashboard';
  import Activity from '@lucide/svelte/icons/activity';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import ListChecks from '@lucide/svelte/icons/list-checks';
  import Bot from '@lucide/svelte/icons/bot';
  import Wrench from '@lucide/svelte/icons/wrench';
  import Radio from '@lucide/svelte/icons/radio';
  import Layers from '@lucide/svelte/icons/layers';
  import FlaskConical from '@lucide/svelte/icons/flask-conical';
  import Workflow from '@lucide/svelte/icons/workflow';
  import Brain from '@lucide/svelte/icons/brain';
  import Plug from '@lucide/svelte/icons/plug';
  import Package from '@lucide/svelte/icons/package';
  import Settings from '@lucide/svelte/icons/settings';

  /** One sidebar entry. */
  interface NavItem {
    label: string;
    href: string;
    icon: Component;
  }

  /** One sidebar cluster — a labelled group of nav entries. */
  interface NavCluster {
    label: string;
    items: NavItem[];
  }

  // The 14-page information architecture in four clusters (CONVENTIONS.md
  // §2). Playground rides in the Execution cluster (D-159, supersedes the
  // original D-121 stance). NO Evaluations entry — Evaluations is post-V1
  // (D-064); the canonical mock predates that decision.
  const NAV: NavCluster[] = [
    {
      label: 'Runtime',
      items: [
        { label: 'Overview', href: '/overview', icon: LayoutDashboard },
        { label: 'Live Runtime', href: '/live-runtime', icon: Activity }
      ]
    },
    {
      label: 'Execution',
      items: [
        { label: 'Sessions', href: '/sessions', icon: MessagesSquare },
        { label: 'Tasks', href: '/tasks', icon: ListChecks },
        { label: 'Agents', href: '/agents', icon: Bot },
        { label: 'Tools', href: '/tools', icon: Wrench },
        { label: 'Events', href: '/events', icon: Radio },
        { label: 'Background Jobs', href: '/background-jobs', icon: Layers },
        { label: 'Playground', href: '/playground', icon: FlaskConical }
      ]
    },
    {
      label: 'Resources',
      items: [
        { label: 'Flows', href: '/flows', icon: Workflow },
        { label: 'Memory', href: '/memory', icon: Brain },
        { label: 'MCP Connections', href: '/mcp-connections', icon: Plug },
        { label: 'Artifacts', href: '/artifacts', icon: Package }
      ]
    },
    {
      label: 'Settings',
      items: [{ label: 'Settings', href: '/settings', icon: Settings }]
    }
  ];

  // The localStorage key the sidebar-collapse preference persists under.
  const COLLAPSE_KEY = 'harbor.console.nav_collapsed';
</script>

<script lang="ts">
  import { onMount } from 'svelte';
  import type { Snippet } from 'svelte';
  import { page } from '$app/stores';
  import { goto } from '$app/navigation';
  import { resolveConnection } from '$lib/connection.js';
  import AppStatusBar from '$lib/components/ui/AppStatusBar.svelte';
  import TopBar from '$lib/components/ui/TopBar.svelte';

  let { children }: { children?: Snippet } = $props();

  // The `console-hydrated` marker the Playwright harness waits on.
  let hydrated = $state(false);
  // Sidebar collapse (icons-only) — persisted, toggled by the top-bar
  // hamburger. Read from storage on mount so reload restores the choice.
  let collapsed = $state(false);
  onMount(() => {
    hydrated = true;
    try {
      collapsed = localStorage.getItem(COLLAPSE_KEY) === '1';
    } catch {
      /* localStorage unavailable (SSR/test) — keep the default */
    }
  });

  function toggleCollapse() {
    collapsed = !collapsed;
    try {
      localStorage.setItem(COLLAPSE_KEY, collapsed ? '1' : '0');
    } catch {
      /* non-fatal — the toggle still works for this session */
    }
  }

  const connection = $derived(resolveConnection());

  // Phase 105 (V1.2) — first-load redirect: when nothing is attached,
  // send the operator to Settings.
  $effect(() => {
    if (resolveConnection() === null && !$page.url.pathname.startsWith('/settings')) {
      goto('/settings', { replaceState: true });
    }
  });

  // The breadcrumb label is derived from the active route via the NAV
  // constant (D-159 — single source for sidebar + breadcrumb).
  const segments = $derived($page.url.pathname.split('/').filter((s) => s.length > 0));
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

<div
  class="console-shell"
  class:collapsed
  data-testid={hydrated ? 'console-hydrated' : 'console-hydrating'}
>
  <nav class="sidebar" aria-label="Console navigation">
    <div class="brand" title="Harbor Console">
      <!-- Phase 108 (D-167) — Harbor lighthouse mark + wordmark. -->
      <img class="brand-logo" src="/harbor_logo.svg" alt="Harbor" width="24" height="24" />
      {#if !collapsed}
        <span class="brand-wordmark">
          <span class="brand-name">Harbor</span>
          <span class="brand-sub">CONSOLE</span>
        </span>
      {/if}
    </div>
    {#each NAV as cluster (cluster.label)}
      <div class="nav-cluster">
        {#if !collapsed}
          <p class="cluster-label">{cluster.label}</p>
        {/if}
        <ul>
          {#each cluster.items as item (item.href)}
            {@const Icon = item.icon}
            <li>
              <a
                href={item.href}
                class:active={isActive(item.href)}
                aria-current={isActive(item.href) ? 'page' : undefined}
                title={collapsed ? item.label : undefined}
              >
                <span class="nav-icon"><Icon size={18} aria-hidden="true" /></span>
                {#if !collapsed}<span class="nav-label">{item.label}</span>{/if}
              </a>
            </li>
          {/each}
        </ul>
      </div>
    {/each}
  </nav>

  <div class="main-column">
    <TopBar {crumbLabel} {connection} {collapsed} onToggleCollapse={toggleCollapse} />

    <main class="content">{@render children?.()}</main>

    <!-- One global app status bar on every page (connection · protocol ·
         events · console version) — the single bottom bar (108a/108b). -->
    <AppStatusBar />
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
    width: var(--size-nav);
    flex-shrink: 0;
    padding: var(--space-3);
    background: var(--color-surface);
    border-right: var(--border-hairline);
    overflow-y: auto;
    overflow-x: hidden;
  }

  .console-shell.collapsed .sidebar {
    width: var(--size-nav-collapsed);
    align-items: center;
  }

  .brand {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-1) var(--space-3);
    border-bottom: var(--border-hairline);
    width: 100%;
  }

  .console-shell.collapsed .brand {
    justify-content: center;
  }

  .brand-logo {
    width: var(--size-avatar-sm);
    height: var(--size-avatar-sm);
    flex-shrink: 0;
  }

  .brand-wordmark {
    display: flex;
    flex-direction: column;
  }

  .brand-name {
    font-size: var(--text-lg);
    font-weight: 600;
  }

  .brand-sub {
    font-size: var(--text-2xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-widest);
    color: var(--color-accent);
  }

  .nav-cluster {
    width: 100%;
  }

  .cluster-label {
    margin: var(--space-0) var(--space-0) var(--space-1);
    padding: var(--space-0) var(--space-2);
    font-size: var(--text-2xs);
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
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    text-decoration: none;
    border-left: var(--border-emphasis-width) solid transparent;
  }

  .console-shell.collapsed .nav-cluster a {
    justify-content: center;
    gap: var(--space-0);
    border-left: none;
  }

  .nav-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
  }

  .nav-label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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

  .content {
    flex: 1;
    padding: var(--space-6);
    min-width: 0;
    min-height: 0;
    overflow: hidden;
  }
</style>
