<script lang="ts">
  // Harbor Console — Overview counter card (Phase 73a / D-127).
  //
  // One of the four cards in the counter row (Events/min, Tasks
  // Running, Background Jobs, MCP Connections — page-overview.md §4
  // row 2). The card carries a label, a headline value, an optional
  // sparkline, and is clickable — it deep-links to its detail page
  // (CONVENTIONS.md §1, unprefixed routes).
  //
  // Svelte 5 runes mode (D-092); design tokens only (CLAUDE.md §4.5).
  import CounterCardSparkline from './CounterCardSparkline.svelte';
  import type { RateSeries } from '$lib/overview/aggregations.js';

  let {
    label,
    value,
    href,
    testid,
    series
  }: {
    /** The counter label (e.g. `Events/min`). */
    label: string;
    /** The headline value the card displays. */
    value: string;
    /** The unprefixed Console route the card deep-links into. */
    href: string;
    /** Stable test id for the card root. */
    testid: string;
    /** Optional rate-series sparkline (only the Events/min card carries one). */
    series?: RateSeries;
  } = $props();
</script>

<a class="counter-card" {href} data-testid={testid}>
  <span class="label">{label}</span>
  <span class="value" data-testid={`${testid}-value`}>{value}</span>
  {#if series}
    <CounterCardSparkline {series} />
  {/if}
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

  .label {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .value {
    font-size: var(--text-xl);
    font-weight: 600;
    line-height: 1;
  }
</style>
