// Harbor Console — Flows-page formatting helper tests (Phase 73i / D-117).

import { describe, expect, it } from 'vitest';
import {
  budgetFraction,
  formatCost,
  formatDurationMS,
  formatRate,
  formatRelative,
  shortRunID,
} from '../format';

describe('formatDurationMS', () => {
  it('renders a dash for zero / undefined', () => {
    expect(formatDurationMS(undefined)).toBe('—');
    expect(formatDurationMS(0)).toBe('—');
  });
  it('renders sub-second durations in ms', () => {
    expect(formatDurationMS(450)).toBe('450 ms');
  });
  it('renders sub-minute durations in seconds', () => {
    expect(formatDurationMS(2500)).toBe('2.5 s');
  });
  it('renders longer durations in minutes', () => {
    expect(formatDurationMS(90_000)).toBe('1.5 min');
  });
});

describe('formatRate', () => {
  it('renders a percentage', () => {
    expect(formatRate(0.95)).toBe('95%');
    expect(formatRate(1)).toBe('100%');
  });
  it('renders a dash for undefined', () => {
    expect(formatRate(undefined)).toBe('—');
  });
});

describe('formatCost', () => {
  it('renders a USD amount', () => {
    expect(formatCost(1.5)).toBe('$1.50');
  });
  it('renders zero for undefined / zero', () => {
    expect(formatCost(undefined)).toBe('$0.00');
    expect(formatCost(0)).toBe('$0.00');
  });
});

describe('formatRelative', () => {
  const now = new Date('2026-05-20T12:00:00Z');
  it('renders "never" for an empty timestamp', () => {
    expect(formatRelative(undefined, now)).toBe('never');
  });
  it('renders seconds ago', () => {
    expect(formatRelative('2026-05-20T11:59:30Z', now)).toBe('30s ago');
  });
  it('renders minutes ago', () => {
    expect(formatRelative('2026-05-20T11:30:00Z', now)).toBe('30m ago');
  });
  it('renders hours ago', () => {
    expect(formatRelative('2026-05-20T09:00:00Z', now)).toBe('3h ago');
  });
  it('renders days ago', () => {
    expect(formatRelative('2026-05-18T12:00:00Z', now)).toBe('2d ago');
  });
});

describe('budgetFraction', () => {
  it('returns 0 when the cap is zero', () => {
    expect(budgetFraction(5, 0)).toBe(0);
  });
  it('clamps to [0,1]', () => {
    expect(budgetFraction(15, 10)).toBe(1);
    expect(budgetFraction(5, 10)).toBe(0.5);
  });
});

describe('shortRunID', () => {
  it('keeps short ids intact', () => {
    expect(shortRunID('run-abc')).toBe('run-abc');
  });
  it('truncates long ids with an ellipsis', () => {
    expect(shortRunID('run-0123456789abcdef')).toBe('run-01234567…');
  });
});
