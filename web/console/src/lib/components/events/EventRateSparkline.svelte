<script lang="ts">
  // Events page — event-rate sparkline (Phase 73g / D-125).
  //
  // A per-event-type stacked-area chart over the active window. The data
  // source is `events.aggregate` (Phase 72a) projected by `sparkline.ts`
  // — a PURE read; no Protocol mutation fires from this component.
  // Hovering a column highlights its time slice; clicking a column pins
  // that window's dominant event-type facet chip (both Console-local
  // state changes — page-events.md §12). Page-specific component:
  // `components/events/` per CONVENTIONS.md §3. Svelte 5 runes (D-092);
  // design tokens only.
  //
  // Note (page-events.md §12 risk): Pause-stream freezes the TABLE only.
  // The sparkline keeps refreshing while events flow — hiding the
  // rate-over-time view would be a worse UX than freezing only the table.
  import { categoryKind, categoryOf } from '$lib/events/taxonomy.js';
  import type { SparklineSeries } from '$lib/events/sparkline.js';

  let {
    series,
    onpin
  }: {
    /** The projected stacked-area series from `EventsAggregator`. */
    series: SparklineSeries;
    /** Pins an event-type facet chip — fired on a column click. */
    onpin?: (eventType: string) => void;
  } = $props();

  /** The column the operator is hovering, or -1. */
  let hovered = $state(-1);

  /** The dominant (highest-count) event type in a column, for click-to-pin. */
  function dominantType(idx: number): string | null {
    const col = series.columns[idx];
    if (col === undefined) {
      return null;
    }
    let best: string | null = null;
    let bestN = 0;
    for (const [type, n] of Object.entries(col.counts)) {
      if (n > bestN) {
        bestN = n;
        best = type;
      }
    }
    return best;
  }

  /** A column's height as a percentage of the series peak. */
  function columnHeightPct(total: number): number {
    return series.peak === 0 ? 0 : (total / series.peak) * 100;
  }
</script>

<div class="sparkline" data-testid="events-rate-sparkline" role="img" aria-label="Event rate over time">
  {#if series.columns.length === 0}
    <p class="sparkline-empty">No event activity in this window.</p>
  {:else}
    <div class="bars">
      {#each series.columns as col, idx (col.startISO)}
        <button
          type="button"
          class="bar-col"
          class:hovered={hovered === idx}
          data-testid={`sparkline-col-${idx}`}
          title={`${col.total} events`}
          aria-label={`${col.total} events at ${col.startISO}`}
          onmouseenter={() => (hovered = idx)}
          onmouseleave={() => (hovered = -1)}
          onclick={() => {
            const t = dominantType(idx);
            if (t) onpin?.(t);
          }}
        >
          <span class="bar-stack" style:height={`${columnHeightPct(col.total)}%`}>
            {#each Object.entries(col.counts) as [type, n] (type)}
              <span
                class="bar-stripe"
                data-kind={categoryKind(categoryOf(type))}
                style:flex-grow={n}
              ></span>
            {/each}
          </span>
        </button>
      {/each}
    </div>
  {/if}
</div>

<style>
  .sparkline {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    height: var(--size-sparkline-height);
    min-height: var(--size-sparkline-height);
  }

  .sparkline-empty {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .bars {
    display: flex;
    align-items: flex-end;
    gap: var(--space-0);
    height: 100%;
  }

  .bar-col {
    flex: 1 1 0;
    height: 100%;
    display: flex;
    align-items: flex-end;
    background: transparent;
    border: none;
    padding: var(--space-0);
    cursor: pointer;
  }

  .bar-col.hovered {
    background: var(--color-surface-raised);
  }

  .bar-stack {
    width: 100%;
    display: flex;
    flex-direction: column-reverse;
    min-height: var(--size-bar-min);
  }

  .bar-stripe {
    width: 100%;
    min-height: var(--size-bar-min);
  }

  .bar-stripe[data-kind='success'] {
    background: var(--color-success);
  }

  .bar-stripe[data-kind='warning'] {
    background: var(--color-warning);
  }

  .bar-stripe[data-kind='danger'] {
    background: var(--color-danger);
  }

  .bar-stripe[data-kind='accent'] {
    background: var(--color-accent);
  }

  .bar-stripe[data-kind='neutral'] {
    background: var(--color-text-muted);
  }
</style>
