<script lang="ts">
  // Harbor Console — Overview alerts strip (Phase 108c).
  //
  // The banner-shaped warnings row (page-overview.md §4 row 1 / §5, `[shipped]`).
  // Renders the most-recent event per alert type seen in the last 5m (folded by
  // `$lib/overview/alerts.ts` off the events cursor). Each row deep-links to the
  // Events page filtered to that type; the strip is dismissible (local UI). It
  // renders NOTHING when there are no in-window alerts — never a synthesised
  // "all clear" banner (procedure §1).
  //
  // Svelte 5 runes (D-092); design tokens only (CLAUDE.md §4.5).
  import { goto } from '$app/navigation';
  import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
  import X from '@lucide/svelte/icons/x';
  import type { AlertRow } from '$lib/overview/alerts.js';

  let {
    alerts,
    now
  }: {
    alerts: AlertRow[];
    /** Reference clock for relative timestamps (ms epoch). */
    now: number;
  } = $props();

  let dismissed = $state<Set<string>>(new Set());
  const visible = $derived(alerts.filter((a) => !dismissed.has(a.type)));

  function rel(ms: number): string {
    const s = Math.max(0, Math.round((now - ms) / 1000));
    if (s < 60) return `${s}s ago`;
    const m = Math.round(s / 60);
    return m < 60 ? `${m}m ago` : `${Math.round(m / 60)}h ago`;
  }

  function open(type: string) {
    void goto(`/events?type=${encodeURIComponent(type)}`);
  }

  function dismiss(type: string) {
    const next = new Set(dismissed);
    next.add(type);
    dismissed = next;
  }
</script>

{#if visible.length > 0}
  <div class="alerts" data-testid="overview-alerts-strip">
    {#each visible as a (a.type)}
      <div class="alert" data-severity={a.severity}>
        <TriangleAlert size={14} aria-hidden="true" />
        <button type="button" class="alert-body" data-testid="overview-alert" onclick={() => open(a.type)}>
          <span class="alert-label">{a.label}</span>
          <span class="alert-time">{rel(a.occurredMillis)}</span>
        </button>
        <button
          type="button"
          class="alert-dismiss"
          aria-label={`Dismiss ${a.label}`}
          onclick={() => dismiss(a.type)}
        >
          <X size={13} />
        </button>
      </div>
    {/each}
  </div>
{/if}

<style>
  .alerts {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .alert {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius-sm);
    border: var(--border-hairline);
    background: var(--color-warning-soft);
    border-color: var(--color-warning);
    color: var(--color-warning);
  }

  .alert[data-severity='danger'] {
    background: var(--color-danger-soft);
    border-color: var(--color-danger);
    color: var(--color-danger);
  }

  .alert-body {
    display: inline-flex;
    align-items: baseline;
    gap: var(--space-2);
    background: transparent;
    border: none;
    cursor: pointer;
    color: var(--color-text);
    font-size: var(--text-sm);
  }

  .alert-label {
    font-weight: 600;
  }

  .alert-time {
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .alert-dismiss {
    display: inline-flex;
    background: transparent;
    border: none;
    color: var(--color-text-muted);
    cursor: pointer;
    padding: var(--space-0);
  }

  .alert-dismiss:hover {
    color: var(--color-text);
  }
</style>
