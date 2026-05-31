<script lang="ts">
  // Harbor Console — Overview counter card (Phase 73a / 108c).
  //
  // One of the four cards in the counter row (page-overview.md §4 row 2 + §12).
  // Mock-faithful shape: a tinted rounded ICON box + title (header row), the
  // headline value with an inline delta PILL + "vs window" note, a LINE
  // sparkline coloured per metric tone, and a "View X →" deep-link. The whole
  // card is the link (CONVENTIONS.md §1 unprefixed routes).
  //
  // No fabrication: the sparkline renders only from a real series; the delta
  // pill only from a real computed delta (procedure §1).
  //
  // Svelte 5 runes (D-092); design tokens only (CLAUDE.md §4.5).
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
    icon: Component;
    viewLabel: string;
    values?: number[];
    delta?: { pct: number; dir: 'up' | 'down' } | null;
    tone?: 'accent' | 'warn' | 'success';
  } = $props();

  const Icon = $derived(icon);
</script>

<a class="counter-card" {href} data-testid={testid} data-tone={tone}>
  <div class="head">
    <span class="icon-box"><Icon size={18} aria-hidden="true" /></span>
    <span class="label">{label}</span>
  </div>

  <div class="value-row">
    <span class="value" data-testid={`${testid}-value`}>{value}</span>
    {#if delta}
      <span class="delta" data-dir={delta.dir} data-testid={`${testid}-delta`}>
        {#if delta.dir === 'up'}<TrendingUp size={11} />{:else}<TrendingDown size={11} />{/if}
        {delta.pct >= 0 ? '+' : ''}{delta.pct.toFixed(0)}%
      </span>
      <span class="delta-note">vs window</span>
    {/if}
  </div>

  {#if values && values.length > 0}
    <span class="spark-wrap"><CounterCardSparkline {values} label={`${label} trend`} /></span>
  {/if}

  <span class="view">{viewLabel}<ArrowRight size={13} aria-hidden="true" /></span>
</a>

<style>
  .counter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    background: var(--color-surface);
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
    gap: var(--space-2);
  }

  /* The icon sits in a tone-tinted rounded box (mock). data-tone drives the
     tint + glyph colour; the rest of the card stays neutral. */
  .icon-box {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--size-avatar-md);
    height: var(--size-avatar-md);
    border-radius: var(--radius-md);
    background: var(--color-accent-soft);
    color: var(--color-accent);
    flex-shrink: 0;
  }

  .counter-card[data-tone='warn'] .icon-box {
    background: var(--color-warning-soft);
    color: var(--color-warning);
  }

  .counter-card[data-tone='success'] .icon-box {
    background: var(--color-success-soft);
    color: var(--color-success);
  }

  .label {
    font-size: var(--text-sm);
    color: var(--color-text-muted);
  }

  .value-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .value {
    font-size: var(--text-2xl);
    font-weight: 600;
    line-height: 1;
  }

  .delta {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-0) var(--space-1);
    border-radius: var(--radius-pill);
    font-size: var(--text-xs);
    font-weight: 600;
    color: var(--color-success);
    background: var(--color-success-soft);
  }

  .delta[data-dir='down'] {
    color: var(--color-danger);
    background: var(--color-danger-soft);
  }

  .delta-note {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  /* The sparkline inherits the tone colour via `color` (currentColor). */
  .spark-wrap {
    display: block;
    color: var(--color-accent);
  }

  .counter-card[data-tone='warn'] .spark-wrap {
    color: var(--color-warning);
  }

  .counter-card[data-tone='success'] .spark-wrap {
    color: var(--color-success);
  }

  .view {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--color-accent);
  }
</style>
