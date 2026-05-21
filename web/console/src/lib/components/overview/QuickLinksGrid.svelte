<script lang="ts" module>
  // Harbor Console — Overview Quick Links grid (Phase 73a / D-127).
  //
  // The 2×3 grid of navigation tiles in canvas row 5 (page-overview.md
  // §4 + §12). EXACTLY six tiles: Sessions / Tasks / Background Jobs /
  // Agents / Tools / Settings. There is NO Evaluations tile — D-064
  // pins Evaluations as post-V1.
  //
  // Each tile carries an icon glyph + the page name + a one-liner, and
  // links to the unprefixed Console route (CONVENTIONS.md §1 — a link
  // to `/console/<anything>` is a bug).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).

  /** One Quick Links tile definition. */
  interface QuickLink {
    /** The unprefixed Console route. */
    href: string;
    /** A short glyph for the tile icon. */
    glyph: string;
    /** The page name. */
    name: string;
    /** A one-line description. */
    blurb: string;
    /** Stable test id. */
    testid: string;
  }

  /**
   * The six V1 Quick Links — page-overview.md §12. Evaluations is
   * deliberately absent (D-064 — post-V1).
   */
  export const QUICK_LINKS: readonly QuickLink[] = [
    {
      href: '/sessions',
      glyph: 'Se',
      name: 'Sessions',
      blurb: 'Browse and inspect active and historical sessions.',
      testid: 'quick-link-sessions'
    },
    {
      href: '/tasks',
      glyph: 'Ta',
      name: 'Tasks',
      blurb: 'The kanban board of foreground and background tasks.',
      testid: 'quick-link-tasks'
    },
    {
      href: '/background-jobs',
      glyph: 'Bg',
      name: 'Background Jobs',
      blurb: 'Long-running detached jobs and their progress.',
      testid: 'quick-link-background-jobs'
    },
    {
      href: '/agents',
      glyph: 'Ag',
      name: 'Agents',
      blurb: 'The Agent Registry — registration identity and health.',
      testid: 'quick-link-agents'
    },
    {
      href: '/tools',
      glyph: 'To',
      name: 'Tools',
      blurb: 'The transport-agnostic tool catalog and policies.',
      testid: 'quick-link-tools'
    },
    {
      href: '/settings',
      glyph: 'Cfg',
      name: 'Settings',
      blurb: 'Runtime connections, governance, and Console preferences.',
      testid: 'quick-link-settings'
    }
  ] as const;
</script>

<div class="quick-links" data-testid="quick-links-grid">
  {#each QUICK_LINKS as link (link.href)}
    <a class="tile" href={link.href} data-testid={link.testid}>
      <span class="glyph">{link.glyph}</span>
      <span class="name">{link.name}</span>
      <span class="blurb">{link.blurb}</span>
    </a>
  {/each}
</div>

<style>
  .quick-links {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: var(--space-3);
  }

  .tile {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    text-decoration: none;
    color: var(--color-text);
    transition: border-color var(--motion-fast) var(--motion-ease);
  }

  .tile:hover {
    border-color: var(--color-accent);
  }

  .glyph {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--space-8);
    height: var(--space-8);
    border-radius: var(--radius-sm);
    background: var(--color-accent-soft);
    color: var(--color-accent);
    font-size: var(--text-sm);
    font-weight: 600;
  }

  .name {
    font-size: var(--text-base);
    font-weight: 600;
  }

  .blurb {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }
</style>
