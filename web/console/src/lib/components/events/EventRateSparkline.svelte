<script lang="ts">
  // Events page — event-rate chart (Phase 73g / D-125; Phase 108h rework
  // to the mock's multi-line + legend composition, page-events.md §12).
  //
  // A per-category multi-line chart over the active window with a Type /
  // Rate / Total legend on the right of the SAME card. The data source is
  // `events.aggregate` (Phase 72a) projected by `sparkline.ts` — a PURE
  // read; no Protocol mutation fires from this component. Per-event-type
  // counts are folded by category (`tool.*`, `task.*`, …) so the chart
  // reads like the mockup; clicking a legend row pins that category's
  // dominant exact type (Console-local). Page-specific component:
  // `components/events/` per CONVENTIONS.md §3. Svelte 5 runes (D-092);
  // design tokens only.
  import { categoryOf } from '$lib/events/taxonomy.js';
  import type { SparklineSeries } from '$lib/events/sparkline.js';

  let {
    series,
    windowSeconds = 3600,
    onpin
  }: {
    /** The projected per-bucket series from `EventsAggregator`. */
    series: SparklineSeries;
    /** The active window span in seconds — drives the per-second rate. */
    windowSeconds?: number;
    /** Pins an event-type facet chip — fired on a legend-row click. */
    onpin?: (eventType: string) => void;
  } = $props();

  /** The categorical line-colour palette (token-only; 5 distinct hues). */
  const PALETTE = [
    'var(--color-success)',
    'var(--color-accent)',
    'var(--color-warning)',
    'var(--color-danger)',
    'var(--color-text-muted)'
  ];

  interface CategorySeries {
    category: string;
    values: number[];
    total: number;
    rate: number;
    dominant: string;
    color: string;
  }

  /** Folds the per-type per-bucket counts into per-category line series. */
  const categories = $derived.by<CategorySeries[]>(() => {
    const n = series.columns.length;
    const map = new Map<string, { values: number[]; total: number; types: Record<string, number> }>();
    series.columns.forEach((col, i) => {
      for (const [type, c] of Object.entries(col.counts)) {
        const cat = categoryOf(type);
        let e = map.get(cat);
        if (e === undefined) {
          e = { values: new Array(n).fill(0), total: 0, types: {} };
          map.set(cat, e);
        }
        e.values[i] += c;
        e.total += c;
        e.types[type] = (e.types[type] ?? 0) + c;
      }
    });
    const arr = [...map.entries()].map(([category, e]) => ({
      category,
      values: e.values,
      total: e.total,
      rate: windowSeconds > 0 ? e.total / windowSeconds : 0,
      dominant: Object.entries(e.types).sort((a, b) => b[1] - a[1])[0]?.[0] ?? category
    }));
    arr.sort((a, b) => b.total - a.total);
    return arr.map((c, i) => ({ ...c, color: PALETTE[i % PALETTE.length] }));
  });

  /** The lines drawn — the top categories (the legend caps the rest). */
  const topCategories = $derived(categories.slice(0, 5));
  const maxValue = $derived(Math.max(1, ...categories.flatMap((c) => c.values)));

  /** Builds an SVG polyline `points` string in the 100×32 viewBox. */
  function points(values: number[]): string {
    const n = values.length;
    if (n === 0) return '';
    if (n === 1) {
      const y = (32 - (values[0] / maxValue) * 32).toFixed(2);
      return `0,${y} 100,${y}`;
    }
    return values
      .map((v, i) => `${((i / (n - 1)) * 100).toFixed(2)},${(32 - (v / maxValue) * 32).toFixed(2)}`)
      .join(' ');
  }

  /** Formats a per-second rate compactly. */
  function fmtRate(r: number): string {
    if (r >= 10) return r.toFixed(0);
    if (r >= 1) return r.toFixed(1);
    return r.toFixed(2);
  }
</script>

<div class="rate" data-testid="events-rate-sparkline" role="img" aria-label="Event rate over time">
  {#if categories.length === 0}
    <p class="rate-empty">No event activity in this window.</p>
  {:else}
    <div class="chart">
      <svg viewBox="0 0 100 32" preserveAspectRatio="none" class="chart-svg" aria-hidden="true">
        {#each topCategories as cat (cat.category)}
          <polyline
            points={points(cat.values)}
            fill="none"
            stroke-width="1.2"
            vector-effect="non-scaling-stroke"
            style:stroke={cat.color}
          />
        {/each}
      </svg>
    </div>

    <table class="legend" data-testid="events-rate-legend">
      <thead>
        <tr><th>Type</th><th class="num">Rate</th><th class="num">Total</th></tr>
      </thead>
      <tbody>
        {#each topCategories as cat (cat.category)}
          <tr>
            <td>
              <button
                type="button"
                class="legend-type"
                data-testid={`legend-${cat.category}`}
                title={`Filter by ${cat.dominant}`}
                onclick={() => onpin?.(cat.dominant)}
              >
                <span class="dot" style:background={cat.color}></span>
                {cat.category}.*
              </button>
            </td>
            <td class="num mono">{fmtRate(cat.rate)}</td>
            <td class="num mono">{cat.total.toLocaleString('en-US')}</td>
          </tr>
        {/each}
      </tbody>
      {#if categories.length > topCategories.length}
        <tfoot>
          <tr><td colspan="3" class="view-all">View all ({categories.length})</td></tr>
        </tfoot>
      {/if}
    </table>
  {/if}
</div>

<style>
  .rate {
    display: flex;
    gap: var(--space-4);
    align-items: stretch;
    height: var(--size-rate-chart-height);
    min-height: var(--size-rate-chart-height);
    overflow: hidden;
  }

  .rate-empty {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .chart {
    flex: 1;
    min-width: var(--space-0);
  }

  .chart-svg {
    width: 100%;
    height: 100%;
    display: block;
  }

  .legend {
    flex-shrink: 0;
    border-collapse: collapse;
    font-size: var(--text-xs);
    align-self: flex-start;
  }

  .legend th {
    text-align: left;
    padding: var(--space-0) var(--space-2) var(--space-1);
    color: var(--color-text-muted);
    font-weight: 600;
  }

  .legend td {
    padding: var(--space-0) var(--space-2);
  }

  .legend .num {
    text-align: right;
  }

  .legend .mono {
    font-family: var(--font-mono);
    color: var(--color-text);
  }

  .legend-type {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    background: none;
    border: none;
    padding: var(--space-0);
    color: var(--color-text);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .legend-type:hover {
    color: var(--color-accent);
  }

  .dot {
    width: var(--space-2);
    height: var(--space-2);
    border-radius: var(--radius-pill);
    flex-shrink: 0;
  }

  .view-all {
    padding-top: var(--space-1);
    color: var(--color-accent);
    cursor: pointer;
  }
</style>
