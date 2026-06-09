/**
 * Flows page pure-projection tests (Phase 108p / D-188).
 *
 * Pins `derive.ts` — the error/display-status projections, the success-rate
 * chip + health-pill thresholds, and the re-exported `format.ts` projections
 * (duration / rate / cost / relative time / budget fraction / short run id).
 */
import { describe, expect, it } from 'vitest';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { FlowDescription } from '../types.js';
import {
  toPageError,
  displayStatus,
  successKind,
  health,
  formatDurationMS,
  formatRate,
  formatCost,
  formatRelative,
  budgetFraction,
  shortRunID
} from '../derive.js';

describe('error + status projections', () => {
  it('keeps the ProtocolError code / collapses others', () => {
    expect(toPageError(new ProtocolError('not_found', 'gone', 404))).toEqual({
      code: 'not_found',
      message: 'gone'
    });
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
    expect(toPageError('weird')).toEqual({ code: 'runtime_error', message: 'unknown error' });
  });

  it('derives ready/empty live (D-180)', () => {
    expect(displayStatus('loading', 5)).toBe('loading');
    expect(displayStatus('error', 5)).toBe('error');
    expect(displayStatus('disconnected', 5)).toBe('disconnected');
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('ready', 3)).toBe('ready');
  });
});

describe('successKind', () => {
  it('maps rate + run count onto the chip scale', () => {
    expect(successKind(1, 0)).toBe('neutral'); // no runs → no signal
    expect(successKind(0.99, 10)).toBe('success');
    expect(successKind(0.8, 10)).toBe('warning');
    expect(successKind(0.5, 10)).toBe('danger');
  });
});

describe('health pill', () => {
  function descWith(runs: number, rate: number): FlowDescription {
    return {
      flow: {
        id: 'f1',
        name: 'f1',
        node_count: 0,
        edge_count: 0,
        runs_24h: runs,
        p50_latency_ms: 0,
        p95_latency_ms: 0,
        success_rate: rate,
        budget: {}
      },
      nodes: [],
      edges: [],
      budget_consumption: { requests_used: 0, cost_usd_used: 0, tokens_used: 0 }
    };
  }
  it('classifies the detail-header health', () => {
    expect(health(null)).toEqual({ label: 'Unknown', kind: 'neutral' });
    expect(health(descWith(0, 1))).toEqual({ label: 'No runs', kind: 'neutral' });
    expect(health(descWith(10, 0.99))).toEqual({ label: 'Healthy', kind: 'success' });
    expect(health(descWith(10, 0.8))).toEqual({ label: 'Degraded', kind: 'warning' });
    expect(health(descWith(10, 0.4))).toEqual({ label: 'Errored', kind: 'danger' });
  });
});

describe('re-exported format projections', () => {
  it('formats durations', () => {
    expect(formatDurationMS(undefined)).toBe('—');
    expect(formatDurationMS(0)).toBe('—');
    expect(formatDurationMS(250)).toBe('250 ms');
    expect(formatDurationMS(1500)).toBe('1.5 s');
    expect(formatDurationMS(90_000)).toBe('1.5 min');
  });

  it('formats rate + cost', () => {
    expect(formatRate(undefined)).toBe('—');
    expect(formatRate(0.95)).toBe('95%');
    expect(formatCost(undefined)).toBe('$0.00');
    expect(formatCost(1.5)).toBe('$1.50');
  });

  it('formats relative time + short run id + budget fraction', () => {
    const now = new Date('2026-06-07T00:00:00Z');
    expect(formatRelative(undefined, now)).toBe('never');
    expect(formatRelative('2026-06-06T23:30:00Z', now)).toBe('30m ago');
    expect(shortRunID('short')).toBe('short');
    expect(shortRunID('0123456789abcdef')).toBe('0123456789ab…');
    expect(budgetFraction(5, 10)).toBe(0.5);
    expect(budgetFraction(20, 10)).toBe(1);
    expect(budgetFraction(5, 0)).toBe(0);
  });
});
