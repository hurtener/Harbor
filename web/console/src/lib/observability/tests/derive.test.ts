/**
 * Observability — `derive.ts` unit tests (HA-65, Phase 247 minimal
 * Console consumer).
 *
 * Pins the pure projections the page renders:
 *   - the CLOSED wire sets (dimensions / buckets / sorts / measures)
 *     match the canonical Go contract exactly — agent is NOT a dimension
 *     and unsupported counters (attempts, failed LLM calls, retry /
 *     downgrade, task-spawned, user-message) are ABSENT, never
 *     synthesized;
 *   - UTC-grid alignment (floor / ceil / presets / the single alignment
 *     choke point) — the wire rejects unaligned windows loudly, so every
 *     built window is aligned by construction;
 *   - exact integer / micro-unit formatting via BigInt integer
 *     arithmetic — no float division, no `toFixed`, no scientific
 *     notation, no invented precision; a missing / non-finite value is
 *     `null` (the view renders "—"), never an ambiguous zero;
 *   - the freshness-block presentation (current / catching_up /
 *     unavailable + watermark + retention coverage) and the row /
 *     dimension / filter projections.
 */
import { describe, expect, it } from 'vitest';
import {
  COST_SCALE_MICROS,
  DEFAULT_OBSERVABILITY_MEASURES,
  MINUTE_MS,
  OBSERVABILITY_BUCKETS,
  OBSERVABILITY_DIMENSIONS,
  OBSERVABILITY_MEASURES,
  OBSERVABILITY_SORTS,
  WINDOW_PRESETS,
  alignCeilUtc,
  alignFloorUtc,
  alignWindow,
  coverageKind,
  coverageLabel,
  dimensionCellText,
  formatBucketStart,
  formatExactInteger,
  formatMeasureValue,
  formatScaledInteger,
  formatWatermark,
  isAlignedIso,
  isAlignedUtc,
  isQualityUnavailable,
  isRetentionGap,
  measureLabel,
  measureMeta,
  parseModelFilter,
  presetWindow,
  qualityKind,
  qualityLabel,
  retentionHorizonLabel,
  rowKey,
  rowMeasureText,
  toUtcIso,
  windowLabel
} from '../derive.js';
import type { ObservabilityQualityBlock } from '../../protocol/observability.js';

describe('closed wire sets', () => {
  it('exposes exactly the four authoritative dimensions — agent is not a rollup dimension', () => {
    expect(OBSERVABILITY_DIMENSIONS.map((d) => d.key)).toEqual([
      'tenant',
      'user',
      'session',
      'model'
    ]);
  });

  it('exposes exactly the closed bucket grid', () => {
    expect(OBSERVABILITY_BUCKETS.map((b) => b.key)).toEqual(['minute', 'hour', 'day']);
  });

  it('exposes exactly the closed total sort set', () => {
    expect(OBSERVABILITY_SORTS.map((s) => s.key)).toEqual([
      'bucket_asc',
      'bucket_desc',
      'measure_asc',
      'measure_desc'
    ]);
  });

  it('exposes exactly the 15 source-backed measures with their fixed scales', () => {
    // Canonical order mirrors internal/protocol/types/observability.go.
    expect(OBSERVABILITY_MEASURES.map((m) => m.key)).toEqual([
      'llm_completions',
      'llm_tokens_prompt',
      'llm_tokens_completion',
      'llm_tokens_reasoning',
      'llm_tokens_cache_read',
      'llm_tokens_cache_write',
      'llm_tokens_total',
      'llm_cost_micros',
      'llm_latency_count',
      'llm_latency_sum_ms',
      'llm_latency_min_ms',
      'llm_latency_max_ms',
      'tasks_completed',
      'tasks_failed',
      'tasks_cancelled'
    ]);
    // Only the cost measure carries the micro-unit scale; every other
    // measure is a plain integer.
    for (const m of OBSERVABILITY_MEASURES) {
      expect(m.scale).toBe(m.key === 'llm_cost_micros' ? COST_SCALE_MICROS : 1);
    }
  });

  it('never lists an unsupported counter or a non-canonical key', () => {
    const unsupported = [
      'llm_attempts',
      'llm_failed_calls',
      'llm_retry_downgrade',
      'task_spawned',
      'user_messages',
      'agent_runs',
      'cost_usd_float'
    ];
    for (const key of unsupported) {
      expect(measureMeta(key)).toBeNull();
      expect(OBSERVABILITY_MEASURES.some((m) => m.key === key)).toBe(false);
    }
  });

  it('defaults the page selection to members of the closed set only', () => {
    expect(DEFAULT_OBSERVABILITY_MEASURES.length).toBeGreaterThan(0);
    for (const key of DEFAULT_OBSERVABILITY_MEASURES) {
      expect(measureMeta(key)).not.toBeNull();
    }
  });

  it('labels a known measure and falls back to the raw key for an unknown one', () => {
    expect(measureLabel('llm_cost_micros')).toBe('LLM cost');
    expect(measureLabel('not_a_measure')).toBe('not_a_measure');
  });
});

describe('UTC-grid window alignment', () => {
  // 2026-08-14T05:37:42.123Z — deliberately off every grid.
  const OFF_GRID = Date.parse('2026-08-14T05:37:42.123Z');

  it('floors / ceils onto the minute grid', () => {
    expect(alignFloorUtc(OFF_GRID, 'minute')).toBe(Date.parse('2026-08-14T05:37:00Z'));
    expect(alignCeilUtc(OFF_GRID, 'minute')).toBe(Date.parse('2026-08-14T05:38:00Z'));
  });

  it('floors / ceils onto the hour grid', () => {
    expect(alignFloorUtc(OFF_GRID, 'hour')).toBe(Date.parse('2026-08-14T05:00:00Z'));
    expect(alignCeilUtc(OFF_GRID, 'hour')).toBe(Date.parse('2026-08-14T06:00:00Z'));
  });

  it('floors / ceils onto the day grid', () => {
    expect(alignFloorUtc(OFF_GRID, 'day')).toBe(Date.parse('2026-08-14T00:00:00Z'));
    expect(alignCeilUtc(OFF_GRID, 'day')).toBe(Date.parse('2026-08-15T00:00:00Z'));
  });

  it('a grid-aligned instant is aligned; an off-grid instant is not', () => {
    expect(isAlignedUtc(Date.parse('2026-08-14T05:00:00Z'), 'hour')).toBe(true);
    expect(isAlignedUtc(OFF_GRID, 'hour')).toBe(false);
    expect(isAlignedIso('2026-08-14T05:00:00Z', 'hour')).toBe(true);
    expect(isAlignedIso('2026-08-14T05:37:00Z', 'hour')).toBe(false);
    expect(isAlignedIso('not-a-time', 'minute')).toBe(false);
  });

  it('preset windows are aligned by construction for every bucket', () => {
    const now = Date.parse('2026-08-14T05:37:42Z');
    for (const bucket of ['minute', 'hour', 'day'] as const) {
      for (const preset of WINDOW_PRESETS[bucket]) {
        const w = presetWindow(preset.id, bucket, now);
        expect(isAlignedUtc(Date.parse(w.from), bucket)).toBe(true);
        expect(isAlignedUtc(Date.parse(w.to), bucket)).toBe(true);
        expect(Date.parse(w.to) - Date.parse(w.from)).toBe(preset.durationMs);
        expect(w.from).toBe(toUtcIso(Date.parse(w.from)));
      }
    }
  });

  it('an unknown preset id falls back to the bucket first preset, never throws', () => {
    const w = presetWindow('nope', 'hour', 0);
    expect(w).toEqual(presetWindow(WINDOW_PRESETS.hour[0].id, 'hour', 0));
  });

  it('alignWindow floors the start and ceils the end — the single choke point', () => {
    const from = Date.parse('2026-08-14T05:12:00Z');
    const to = Date.parse('2026-08-14T05:47:00Z');
    const w = alignWindow(from, to, 'hour');
    expect(w.from).toBe('2026-08-14T05:00:00.000Z');
    expect(w.to).toBe('2026-08-14T06:00:00.000Z');
  });

  it('alignWindow enforces at least one full bucket for a degenerate window', () => {
    const w = alignWindow(
      Date.parse('2026-08-14T05:00:00Z'),
      Date.parse('2026-08-14T05:00:00Z'),
      'minute'
    );
    expect(Date.parse(w.to) - Date.parse(w.from)).toBe(MINUTE_MS);
  });

  it('alignWindow never sends a raw operator instant unaligned', () => {
    // A hand-edited window with a mid-hour start/end is floored/ceiled —
    // the wire rejects unaligned edges loudly, so the consumer re-aligns.
    const w = alignWindow(
      Date.parse('2026-08-14T05:12:00Z'),
      Date.parse('2026-08-14T07:38:00Z'),
      'hour'
    );
    expect(isAlignedUtc(Date.parse(w.from), 'hour')).toBe(true);
    expect(isAlignedUtc(Date.parse(w.to), 'hour')).toBe(true);
    expect(w.from).toBe('2026-08-14T05:00:00.000Z');
    expect(w.to).toBe('2026-08-14T08:00:00.000Z');
  });

  it('windowLabel renders a human UTC span for the bucket', () => {
    const from = Date.parse('2026-08-14T05:00:00Z');
    const to = Date.parse('2026-08-14T06:00:00Z');
    expect(windowLabel(from, to, 'hour')).toContain('2026-08-14 05:00');
    expect(windowLabel(from, to, 'hour')).toContain('2026-08-14 06:00');
  });
});

describe('exact integer / micro-unit formatting (no float precision loss)', () => {
  it('renders integer values exactly as decimal strings', () => {
    expect(formatExactInteger(0)).toBe('0'); // a PRESENT zero — not ambiguous
    expect(formatExactInteger(42)).toBe('42');
    expect(formatExactInteger(1_234_567_890)).toBe('1234567890');
    expect(formatExactInteger(-7)).toBe('-7');
  });

  it('renders values above 2^53 without scientific notation or float arithmetic', () => {
    // JSON.parse already holds 9007199254740992 (2^53) exactly; the
    // consumer must never push it through float division / toFixed.
    expect(formatExactInteger(9_007_199_254_740_992)).toBe('9007199254740992');
    // A value as received (already integral) renders its integer part.
    expect(formatExactInteger(1e21)).toBe('1000000000000000000000');
  });

  it('returns null for non-finite payloads — never an invented zero', () => {
    expect(formatExactInteger(Number.NaN)).toBeNull();
    expect(formatExactInteger(Number.POSITIVE_INFINITY)).toBeNull();
  });

  it('formats integer measures (scale 1) as plain integers', () => {
    expect(formatScaledInteger(123_456, 1)).toBe('123456');
    expect(formatScaledInteger(0, 1)).toBe('0');
  });

  it('formats cost micro-units exactly as decimal USD (N / 1_000_000)', () => {
    expect(formatScaledInteger(123_456_789, COST_SCALE_MICROS)).toBe('123.456789');
    expect(formatScaledInteger(5, COST_SCALE_MICROS)).toBe('0.000005');
    expect(formatScaledInteger(1_000_000, COST_SCALE_MICROS)).toBe('1');
    expect(formatScaledInteger(-5, COST_SCALE_MICROS)).toBe('-0.000005');
    expect(formatScaledInteger(0, COST_SCALE_MICROS)).toBe('0');
  });

  it('formats a wire measure value exactly and rejects bad scales', () => {
    expect(formatMeasureValue({ n: 123_456_789, scale: COST_SCALE_MICROS })).toBe('123.456789');
    expect(formatMeasureValue({ n: 42, scale: 1 })).toBe('42');
    expect(formatMeasureValue({ n: Number.NaN, scale: 1 })).toBeNull();
    expect(formatMeasureValue({ n: 42, scale: 0 })).toBeNull();
  });

  it('rowMeasureText returns null (unavailable) for a missing measure — never "0"', () => {
    const measures = { llm_completions: { n: 7, scale: 1 } };
    expect(rowMeasureText(measures, 'llm_completions')).toBe('7');
    expect(rowMeasureText(measures, 'llm_cost_micros')).toBeNull();
    expect(rowMeasureText({}, 'tasks_completed')).toBeNull();
  });

  it('renders the watermark exactly as an integer string', () => {
    expect(formatWatermark(0)).toBe('0');
    expect(formatWatermark(98_765)).toBe('98765');
    expect(formatWatermark(Number.NaN)).toBeNull();
  });
});

describe('freshness-block presentation', () => {
  it('maps the three states to distinct kinds and labels', () => {
    expect(qualityKind('current')).toBe('success');
    expect(qualityKind('catching_up')).toBe('warning');
    expect(qualityKind('unavailable')).toBe('danger');
    expect(qualityKind('something_else')).toBe('neutral');
    expect(qualityLabel('current')).toBe('Current');
    expect(qualityLabel('catching_up')).toBe('Catching up');
    expect(qualityLabel('unavailable')).toBe('Unavailable');
  });

  it('maps the three coverage values to distinct kinds and labels', () => {
    expect(coverageKind('covered')).toBe('success');
    expect(coverageKind('partial')).toBe('warning');
    expect(coverageKind('gap')).toBe('danger');
    expect(coverageKind('other')).toBe('neutral');
    expect(coverageLabel('covered')).toBe('Covered');
    expect(coverageLabel('partial')).toBe('Partial');
    expect(coverageLabel('gap')).toBe('Gap');
  });

  it('flags unavailable and gap distinctly from a plain empty result', () => {
    const unavailable: ObservabilityQualityBlock = {
      state: 'unavailable',
      watermark: 0,
      coverage: 'gap'
    };
    const catchingUp: ObservabilityQualityBlock = {
      state: 'catching_up',
      watermark: 5,
      coverage: 'partial'
    };
    const current: ObservabilityQualityBlock = {
      state: 'current',
      watermark: 9,
      coverage: 'covered',
      retention_start: '2026-08-13T00:00:00Z',
      retention_end: '2026-08-14T05:00:00Z'
    };
    expect(isQualityUnavailable(unavailable)).toBe(true);
    expect(isQualityUnavailable(catchingUp)).toBe(false);
    expect(isRetentionGap(unavailable)).toBe(true);
    expect(isRetentionGap(catchingUp)).toBe(false);
    expect(isRetentionGap(current)).toBe(false);
  });

  it('renders the retained horizon, or "—" when the store holds no rows', () => {
    const q: ObservabilityQualityBlock = {
      state: 'current',
      watermark: 9,
      coverage: 'covered',
      retention_start: '2026-08-13T00:00:00Z',
      retention_end: '2026-08-14T05:37:00Z'
    };
    expect(retentionHorizonLabel(q)).toContain('2026-08-13 00:00');
    expect(retentionHorizonLabel(q)).toContain('2026-08-14 05:37');
    const empty: ObservabilityQualityBlock = { state: 'current', watermark: 0, coverage: 'gap' };
    expect(retentionHorizonLabel(empty)).toBe('—');
  });
});

describe('row / dimension / filter projections', () => {
  it('bucket starts render coarse per bucket', () => {
    const iso = '2026-08-14T05:37:42Z';
    expect(formatBucketStart(iso, 'day')).toBe('2026-08-14');
    expect(formatBucketStart(iso, 'hour')).toBe('2026-08-14 05:00');
    expect(formatBucketStart(iso, 'minute')).toBe('2026-08-14 05:37');
  });

  it('renders authoritative dimension values; empty model reads "unattributed"', () => {
    expect(dimensionCellText('model', 'gpt-5')).toBe('gpt-5');
    expect(dimensionCellText('model', '')).toBe('unattributed');
    expect(dimensionCellText('tenant', '')).toBe('—');
    expect(dimensionCellText('user', 'u-1')).toBe('u-1');
  });

  it('derives stable, unique row keys', () => {
    const a = { bucket_start: '2026-08-14T05:00:00Z', dimensions: { user: 'u1', model: 'm1' } };
    const b = { bucket_start: '2026-08-14T05:00:00Z', dimensions: { model: 'm1', user: 'u1' } };
    const c = { bucket_start: '2026-08-14T05:00:00Z', dimensions: { user: 'u1', model: 'm2' } };
    const d = { bucket_start: '2026-08-14T05:00:00Z' };
    expect(rowKey(a)).toBe(rowKey(b)); // dimension order-insensitive
    expect(rowKey(a)).not.toBe(rowKey(c));
    expect(rowKey(d)).toBe('2026-08-14T05:00:00Z');
  });

  it('parses a comma-separated model filter into the closed models axis', () => {
    expect(parseModelFilter('  gpt-5 , claude, gpt-5 , , claude ')).toEqual(['gpt-5', 'claude']);
    expect(parseModelFilter('')).toEqual([]);
    expect(parseModelFilter(', ,')).toEqual([]);
  });
});
