<script lang="ts">
  // Harbor Console — Observability freshness banner (HA-65, Phase 247).
  //
  // The MANDATORY freshness block stamp on every `observability.query`
  // response, surfaced loudly — never hidden behind the rows:
  //   - the projector's catch-up state: current | catching_up |
  //     unavailable (three DISTINCT chip kinds — an unavailable or
  //     catching-up projection must never read as "no data");
  //   - the observed watermark (the last applied sequence of the local
  //     durable sequence), rendered exactly (BigInt integer arithmetic —
  //     never float-normalised);
  //   - the retained horizon and the window-coverage quality
  //     (covered | partial | gap).
  //
  // When the state is `unavailable` or the coverage is `gap`, the banner
  // adds an explicit one-line notice so an empty table is never mistaken
  // for "nothing happened" — the Phase 247 honesty contract (a query
  // never returns zero as a substitute for "projection unavailable").
  import { StatusChip, type StatusKind } from '$lib/components/ui/index.js';
  import type { ObservabilityQualityBlock } from '$lib/protocol/observability.js';
  import {
    coverageKind,
    coverageLabel,
    formatBucketStart,
    formatWatermark,
    isQualityUnavailable,
    isRetentionGap,
    qualityKind,
    qualityLabel,
    retentionHorizonLabel
  } from './derive.js';

  let { quality }: { quality: ObservabilityQualityBlock } = $props();

  const stateKind = $derived(qualityKind(quality.state) as StatusKind);
  const coverageKindResolved = $derived(coverageKind(quality.coverage) as StatusKind);
  const watermarkText = $derived(formatWatermark(quality.watermark) ?? '—');
  const watermarkAt = $derived(
    quality.watermark_at ? formatBucketStart(quality.watermark_at, 'minute') : '—'
  );
  const unavailable = $derived(isQualityUnavailable(quality));
  const gap = $derived(isRetentionGap(quality));
</script>

<section class="quality-banner" data-testid="obs-quality-banner">
  <div class="quality-row">
    <span class="key">Projection</span>
    <StatusChip kind={stateKind} label={qualityLabel(quality.state)} />
    <span class="key">Coverage</span>
    <StatusChip kind={coverageKindResolved} label={coverageLabel(quality.coverage)} />
    <span class="key">Watermark</span>
    <span class="mono" data-testid="obs-watermark">{watermarkText}</span>
    <span class="dim">at {watermarkAt}</span>
    <span class="key">Retained</span>
    <span class="mono" data-testid="obs-retention">{retentionHorizonLabel(quality)}</span>
  </div>
  {#if unavailable}
    <p class="notice danger" data-testid="obs-quality-unavailable">
      The rollup projection is unavailable — the projector's last ingestion
      failed. Rows below (if any) are the last exact values the store held;
      totals are NOT current and never read as zero.
    </p>
  {:else if gap}
    <p class="notice warning" data-testid="obs-quality-gap">
      The requested window falls outside the retained horizon — totals are
      empty BY RETENTION, not because nothing happened.
    </p>
  {:else if quality.coverage === 'partial'}
    <p class="notice warning" data-testid="obs-quality-partial">
      The requested window overlaps the retained horizon — totals for the
      window are incomplete by retention.
    </p>
  {/if}
</section>

<style>
  .quality-banner {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: var(--color-surface);
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    padding: var(--space-2) var(--space-3);
  }

  .quality-row {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--color-text-muted);
  }

  .key {
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin-left: var(--space-2);
  }

  .key:first-child {
    margin-left: var(--space-0);
  }

  .mono {
    font-family: var(--font-mono);
    font-variant-numeric: var(--font-variant-tabular);
    color: var(--color-text);
  }

  .dim {
    color: var(--color-text-muted);
  }

  .notice {
    margin: var(--space-0);
    font-size: var(--text-xs);
  }

  .notice.danger {
    color: var(--color-danger);
  }

  .notice.warning {
    color: var(--color-warning);
  }
</style>
