<script lang="ts">
  // Harbor Console — Overview counter card (Phase 73a / 108c).
  //
  // One of the four cards in the counter row (Events/min, Tasks Running,
  // Background Jobs, MCP Connections — page-overview.md §4 row 2 + §12). Each
  // card carries a leading status icon, a headline value, an optional trend
  // sparkline, an optional real delta badge, and a "View X →" deep-link
  // (CONVENTIONS.md §1, unprefixed routes). The whole card is the link.
  //
  // No fabrication: the sparkline renders only when a real series is supplied
  // (events-rate fold or sampled gauge history); the delta badge renders only
  // when `delta` is a real computed value — never a placeholder (procedure §1).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import type { Component } from 'svelte';
  import TrendingUp from '@lucide/svelte/icons/trending-up';
  import TrendingDown from '@lucide/svelte/icons/trending-down';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import CounterCardSparkline from './CounterCardSparkline.svelte';

  let {
    label,
    value,
    href,
    testid,
    icon,
    viewLabel,
    values,
    delta,
    tone = 'accent'
  }: {
    label: string;
    value: string;
    href: string;
    testid: string;
    /** Leading lucide icon component. */
    icon: Component;
    /** The "View X →" footer link text. */
    viewLabel: string;
    /** Optional real numeric series for the sparkline (oldest → newest). */
    values?: number[];
    /** Optional real delta vs the window start; null/absent renders nothing. */
    delta?: { pct: number; dir: 'up' | 'down' } | null;
    /** Status tone for the leading icon (mock shows warn on Background Jobs). */
    tone?: 'accent' | 'warn' | 'success';
  } = $props();

  const Icon = $derived(icon);
</script>

<a class="counter-card" {href} data-testid={testid}>
  <div class="head">
    <span class="icon" data-tone={tone}><Icon size={18} aria-hidden="true" /></span>
    {#if delta}
      <span class="delta" data-dir={delta.dir} data-testid={`${testid}-delta`}>
        {#if delta.dir === 'up'}<TrendingUp size={12} />{:else}<TrendingDown size={12} />{/if}
        {delta.pct >= 0 ? '+' : ''}{delta.pct.toFixed(0)}%
      </span>
    {/if}
  </div>

  <span class="label">{label}</span>
  <span class="value" data-testid={`${testid}-value`}>{value}</span>

  {#if values && values.length > 0}
    <CounterCardSparkline {values} label={`${label} trend`} />
  {/if}

  <span class="view">{viewLabel}<ArrowRight size={13} aria-hidden="true" /></span>
</a>

<style>
  .counter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4);
    background: var(--color-surface-raised);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    text-decoration: none;
    color: var(--color-text);
    min-width: var(--size-card-min);
    transition: border-color var(--motion-fast) var(--motion-ease);
  }

  .counter-card:hover {
    border-color: var(--color-accent);
  }

  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }

  .icon {
    display: inline-flex;
    color: var(--color-accent);
  }

  .icon[data-tone='warn'] {
    color: var(--color-warning);
  }

  .icon[data-tone='success'] {
    color: var(--color-success);
  }

  .delta {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-success);
  }

  .delta[data-dir='down'] {
    color: var(--color-danger);
  }

  .label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .value {
    font-size: var(--text-2xl);
    font-weight: 600;
    line-height: 1;
  }

  .view {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-accent);
  }
</style>
