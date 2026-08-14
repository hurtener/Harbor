<script lang="ts">
  // Harbor Console — Observability page (`/observability`) — HA-65,
  // Phase 247 minimal Console consumer.
  //
  // The ONE bounded projection-backed administrative rollup query,
  // rendered through the typed Protocol surface only
  // (`client.observability.query` — CONVENTIONS.md §6; never a raw
  // database/event/history scan, never a live-cursor redesign, no
  // operator analytics beyond the one bounded query). Nav composition is
  // a later exclusive lane — this page ships standalone.
  //
  // Contract the page enforces:
  //   - EXPLICIT bounded UTC-grid-aligned window: presets are aligned by
  //     construction; a hand-edited window is re-aligned through
  //     `alignWindow` (floor start / ceil end) before it reaches the
  //     wire — the wire rejects unaligned edges loudly, so the consumer
  //     never sends one. The effective aligned window is always shown.
  //   - CLOSED control sets exposed verbatim: bucket (minute|hour|day),
  //     group_by (tenant|user|session|model), measures (the 15
  //     source-backed measures), sort (the 4 total sorts). Unsupported
  //     counters are absent, never synthesized.
  //   - AUTHORITY posture: the verified identity triple is server-side;
  //     the body never widens, so the page exposes NO tenant/user/session
  //     filter inputs — the only filter axis is the closed `models`
  //     axis. A widened (admin|console:fleet) fan-in is gated + audited
  //     server-side and surfaced only as an informational note.
  //   - HONEST states: current / catching_up / unavailable, the
  //     watermark, retention coverage (covered|partial|gap), empty, and
  //     typed errors are all DISTINCT (the freshness banner + PageState
  //     branches). A missing measure renders "—", never an ambiguous
  //     zero; exact integer/micro values render via BigInt integer
  //     arithmetic (no float precision loss).
  //
  // Svelte 5 runes (D-092); design tokens only; HarborClient +
  // connection.ts only — no hand-rolled fetch (CONVENTIONS.md §6).
  import { onMount } from 'svelte';
  import {
    PageHeader,
    FilterBar,
    PageState,
    DataTable,
    type DataTableColumn
  } from '$lib/components/ui/index.js';
  import { DISCONNECTED_TOOLTIP } from '$lib/connection.js';
  import type { ProtocolClient } from '$lib/protocol/harbor.js';
  import type {
    ObservabilityBucket,
    ObservabilityQueryRow,
    ObservabilitySort
  } from '$lib/protocol/observability.js';
  import { ObservabilityPageState } from '$lib/observability/state.svelte.js';
  import QualityBanner from '$lib/observability/QualityBanner.svelte';
  import CursorPager from '$lib/observability/CursorPager.svelte';
  import {
    OBSERVABILITY_BUCKETS,
    OBSERVABILITY_DIMENSIONS,
    OBSERVABILITY_MEASURES,
    OBSERVABILITY_PAGE_SIZES,
    OBSERVABILITY_SORTS,
    WINDOW_PRESETS,
    dimensionCellText,
    formatBucketStart,
    measureLabel,
    rowKey,
    rowMeasureText,
    windowLabel
  } from '$lib/observability/derive.js';

  let { client: injectedClient }: { client?: ProtocolClient } = $props();

  const state = new ObservabilityPageState();
  const disconnected = $derived(state.disconnected);

  onMount(() => {
    state.load(injectedClient);
  });

  /** The table columns — the bucket, then each active group dimension,
   * then each selected measure (numeric). Derived live so a control
   * change reshapes the table immediately. */
  const columns = $derived.by(() => {
    const cols: DataTableColumn[] = [{ key: 'bucket', label: 'Bucket' }];
    for (const dim of state.groupBy) {
      const meta = OBSERVABILITY_DIMENSIONS.find((d) => d.key === dim);
      cols.push({ key: dim, label: meta?.label ?? dim });
    }
    for (const key of state.selectedMeasures) {
      cols.push({ key, label: measureLabel(key), numeric: true });
    }
    return cols;
  });

  /** The presets for the current bucket (for the window chips). */
  const presets = $derived(WINDOW_PRESETS[state.bucket]);

  /** The effective aligned window label (shown whenever a window is set). */
  const alignedWindowLabel = $derived(
    state.toMs > state.fromMs ? windowLabel(state.fromMs, state.toMs, state.bucket) : null
  );

  /** The selected dimension keys (for chip highlighting). */
  const grouped = $derived(new Set(state.groupBy));

  /** The selected measure keys (for chip highlighting). */
  const selected = $derived(new Set(state.selectedMeasures));

  /** The closed measures split by family (chip groups — precomputed so
   * the template never re-filters on every render). */
  const llmMeasures = $derived(OBSERVABILITY_MEASURES.filter((m) => m.group === 'llm'));
  const taskMeasures = $derived(OBSERVABILITY_MEASURES.filter((m) => m.group === 'task'));
</script>

<svelte:head>
  <title>Observability · Harbor Console</title>
</svelte:head>

<section class="obs-page" data-testid="observability-page">
  <PageHeader
    title="Observability"
    subtitle="The bounded rollup projection — exact cost / token / outcome aggregates over the durable event log, queried through observability.query."
  />

  <!-- Window: explicit, bounded, UTC-grid-aligned. Presets are aligned by
       construction; a hand-edited window is re-aligned (floor/ceil) on
       refresh — the wire rejects unaligned edges loudly, so the consumer
       never sends one. -->
  <section class="panel card window-card">
    <div class="window-row">
      <span class="panel-label">Window</span>
      {#each presets as preset (preset.id)}
        <button
          type="button"
          class="chip"
          class:on={state.presetId === preset.id}
          data-testid={`obs-preset-${preset.id}`}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.applyPreset(preset.id)}
        >
          {preset.label}
        </button>
      {/each}
      <label class="field">
        From
        <input
          type="datetime-local"
          data-testid="obs-window-from"
          value={state.fromRaw}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          oninput={(e) => (state.fromRaw = (e.currentTarget as HTMLInputElement).value)}
        />
      </label>
      <label class="field">
        To
        <input
          type="datetime-local"
          data-testid="obs-window-to"
          value={state.toRaw}
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          oninput={(e) => (state.toRaw = (e.currentTarget as HTMLInputElement).value)}
        />
      </label>
    </div>
    {#if alignedWindowLabel}
      <p class="aligned-note" data-testid="obs-window-aligned">
        Querying the aligned window: <span class="mono">{alignedWindowLabel}</span> —
        {state.bucket} buckets.
      </p>
    {/if}
  </section>

  <!-- Filter card: the CLOSED control sets exposed verbatim -->
  <section class="panel card filter-card">
    <FilterBar>
      {#snippet facets()}
        <div class="control-group">
          <label class="field">
            Bucket
            <select
              value={state.bucket}
              data-testid="obs-bucket"
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onchange={(e) =>
                state.setBucket((e.currentTarget as HTMLSelectElement).value as ObservabilityBucket)
              }
            >
              {#each OBSERVABILITY_BUCKETS as b (b.key)}
                <option value={b.key}>{b.label}</option>
              {/each}
            </select>
          </label>
          <label class="field">
            Sort
            <select
              value={state.sort}
              data-testid="obs-sort"
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onchange={(e) =>
                state.setSort((e.currentTarget as HTMLSelectElement).value as ObservabilitySort)
              }
            >
              {#each OBSERVABILITY_SORTS as s (s.key)}
                <option value={s.key}>{s.label}</option>
              {/each}
            </select>
          </label>
          {#if state.sort === 'measure_asc' || state.sort === 'measure_desc'}
            <label class="field">
              By
              <select
                value={state.effectiveSortMeasure ?? ''}
                data-testid="obs-sort-measure"
                disabled={disconnected}
                title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
                onchange={(e) => state.setSortMeasure((e.currentTarget as HTMLSelectElement).value)}
              >
                {#each state.selectedMeasures as key (key)}
                  <option value={key}>{measureLabel(key)}</option>
                {/each}
              </select>
            </label>
          {/if}
        </div>

        <div class="control-group">
          <span class="panel-label">Group by</span>
          {#each OBSERVABILITY_DIMENSIONS as dim (dim.key)}
            <button
              type="button"
              class="chip"
              class:on={grouped.has(dim.key)}
              data-testid={`obs-group-${dim.key}`}
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onclick={() => state.toggleGroupBy(dim.key)}
            >
              {dim.label}
            </button>
          {/each}
        </div>

        <div class="control-group">
          <span class="panel-label">Measures</span>
          <span class="measure-family">LLM</span>
          {#each llmMeasures as m (m.key)}
            <button
              type="button"
              class="chip"
              class:on={selected.has(m.key)}
              data-testid={`obs-measure-${m.key}`}
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onclick={() => state.toggleMeasure(m.key)}
            >
              {m.label}
            </button>
          {/each}
          <span class="measure-family">Task</span>
          {#each taskMeasures as m (m.key)}
            <button
              type="button"
              class="chip"
              class:on={selected.has(m.key)}
              data-testid={`obs-measure-${m.key}`}
              disabled={disconnected}
              title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
              onclick={() => state.toggleMeasure(m.key)}
            >
              {m.label}
            </button>
          {/each}
        </div>
      {/snippet}

      {#snippet search()}
        <label class="field grow">
          Model filter
          <input
            type="text"
            class="bar-input"
            placeholder="Comma-separated model names (empty = all models, incl. unattributed)"
            value={state.modelFilterText}
            data-testid="obs-model-filter"
            disabled={disconnected}
            title={disconnected
              ? DISCONNECTED_TOOLTIP
              : 'The only non-identity filter axis — tenant/user/session are enforced server-side from your verified triple'}
            oninput={(e) => state.setModelFilterText((e.currentTarget as HTMLInputElement).value)}
            onkeydown={(e) => e.key === 'Enter' && state.applyModelFilter()}
          />
        </label>
      {/snippet}

      {#snippet actions()}
        {#if state.canWiden}
          <span class="widen-note" data-testid="obs-widen-note" title="A widened fan-in emits audit.admin_scope_used server-side">
            Elevated scope
          </span>
        {/if}
        <button
          type="button"
          class="bar-action"
          data-testid="obs-refresh"
          disabled={disconnected}
          title={disconnected ? DISCONNECTED_TOOLTIP : undefined}
          onclick={() => state.refresh()}
        >
          Refresh
        </button>
      {/snippet}
    </FilterBar>
  </section>

  <div class="layout">
    <section class="panel card table-card">
      {#if state.quality !== null}
        <QualityBanner quality={state.quality} />
      {/if}

      <PageState
        status={state.displayStatus}
        error={state.pageError}
        info={state.info}
        onretry={() => state.refresh()}
      >
        {#snippet skeleton()}
          <div class="table-skeleton" aria-hidden="true">
            {#each [0, 1, 2, 3, 4, 5] as i (i)}
              <span class="skeleton-row"></span>
            {/each}
          </div>
        {/snippet}
        {#snippet empty()}
          <div class="empty-block" data-testid="obs-empty">
            {#if state.quality?.state === 'unavailable'}
              <p class="empty-headline">Rollup projection unavailable</p>
              <p class="empty-detail">
                The projector's last ingestion failed — the freshness banner above shows the
                observed watermark and retained horizon. No totals are shown: an unavailable
                projection never reads as zero.
              </p>
            {:else if state.quality?.coverage === 'gap'}
              <p class="empty-headline">No rollup rows retained for this window</p>
              <p class="empty-detail">
                The requested window falls outside the retained horizon (see the freshness
                banner) — empty by retention, not because nothing happened.
              </p>
            {:else}
              <p class="empty-headline">No rollup rows for this window</p>
              <p class="empty-detail">
                The projection holds no rows in the aligned window for the current bucket,
                grouping, and filters.
              </p>
            {/if}
          </div>
        {/snippet}

        <div class="table-scroll">
          <DataTable
            columns={columns}
            rows={state.rows}
            rowKey={(r) => rowKey(r as ObservabilityQueryRow)}
          >
            {#snippet row(r: unknown)}
              {@const qrow = r as ObservabilityQueryRow}
              <td class="mono">{formatBucketStart(qrow.bucket_start, state.bucket)}</td>
              {#each state.groupBy as dim (dim)}
                <td class="mono dims">{dimensionCellText(dim, (qrow.dimensions ?? {})[dim] ?? '')}</td>
              {/each}
              {#each state.selectedMeasures as key (key)}
                {@const text = rowMeasureText(qrow.measures, key)}
                <td class="mono numeric" class:unavailable={text === null} data-testid={`obs-cell-${key}`}>
                  {text ?? '—'}
                </td>
              {/each}
            {/snippet}
          </DataTable>
        </div>
      </PageState>

      {#if state.displayStatus === 'ready' || state.displayStatus === 'empty'}
        <CursorPager
          page={state.pageIndex}
          pageSize={state.limit}
          hasNext={state.hasNextPage}
          pageSizeOptions={OBSERVABILITY_PAGE_SIZES}
          onpage={(p) => (p > state.pageIndex ? state.nextPage() : state.prevPage())}
          onpagesize={(s) => state.setLimit(s)}
        />
      {/if}
    </section>
  </div>
</section>

<style>
  /* Viewport-locked page: the shell content region scrolls only the
     table internally (the Events / Background-Jobs pattern). */
  .obs-page {
    display: flex;
    flex-direction: column;
    height: 100%;
    min-height: 0;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow: hidden;
  }

  .card {
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    min-width: 0;
  }

  .panel-label {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-text-muted);
  }

  /* ---- window card ---- */
  .window-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .window-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .aligned-note {
    margin: var(--space-0);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  /* ---- filter card ---- */
  .filter-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex-shrink: 0;
    padding: var(--space-2) var(--space-3);
  }

  .filter-card :global(.filter-bar) {
    padding: var(--space-1) var(--space-0);
    gap: var(--space-2);
  }

  .control-group {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-2);
  }

  .measure-family {
    font-size: var(--text-2xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wider);
    color: var(--color-text-muted);
    margin-left: var(--space-2);
  }

  .chip {
    background: var(--color-bg);
    color: var(--color-text-muted);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
    text-transform: capitalize;
  }

  .chip.on {
    color: var(--color-accent);
    border-color: var(--color-accent);
    font-weight: 600;
  }

  .chip:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .field {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .field.grow {
    flex: 1;
    min-width: var(--size-search-min);
  }

  .field input,
  .field select {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    font-family: var(--font-sans);
  }

  .field input:disabled,
  .field select:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .bar-action {
    background: var(--color-bg);
    color: var(--color-text);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-3);
    font-size: var(--text-xs);
    cursor: pointer;
  }

  .bar-action:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .widen-note {
    font-size: var(--text-2xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--color-warning);
    cursor: help;
  }

  /* ---- table layout ---- */
  .layout {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
  }

  .table-card {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-height: 0;
    gap: var(--space-3);
  }

  .table-card :global(.page-state) {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }

  .table-scroll {
    flex: 1;
    min-height: 0;
    overflow: auto;
  }

  .table-skeleton {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .skeleton-row {
    height: var(--space-8);
    background: var(--color-surface-raised);
    border-radius: var(--radius-sm);
  }

  .empty-block {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-8) var(--space-4);
    text-align: center;
  }

  .empty-headline {
    margin: var(--space-0);
    font-size: var(--text-lg);
    font-weight: 600;
    color: var(--color-text);
  }

  .empty-detail {
    margin: var(--space-0);
    font-size: var(--text-sm);
    color: var(--color-text-muted);
    max-width: var(--size-modal-width);
  }

  .mono {
    font-family: var(--font-mono);
    font-variant-numeric: var(--font-variant-tabular);
    font-size: var(--text-xs);
  }

  .numeric {
    text-align: right;
  }

  .dims {
    color: var(--color-text-muted);
  }

  .unavailable {
    color: var(--color-text-muted);
  }
</style>
