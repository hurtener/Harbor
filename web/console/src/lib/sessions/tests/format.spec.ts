// Harbor Console — Sessions-page formatting helper tests (Phase 73c /
// D-122).

import { describe, expect, it } from 'vitest';
import {
  formatCostCents,
  formatDurationNS,
  formatRelative,
  formatTokens,
  shortSessionID,
  statusKind
} from '../format';

describe('formatDurationNS', () => {
  it('renders a dash for zero / undefined', () => {
    expect(formatDurationNS(undefined)).toBe('—');
    expect(formatDurationNS(0)).toBe('—');
  });
  it('renders sub-second durations in ms', () => {
    expect(formatDurationNS(450_000_000)).toBe('450 ms');
  });
  it('renders sub-minute durations in seconds', () => {
    expect(formatDurationNS(2_500_000_000)).toBe('2.5 s');
  });
  it('renders sub-hour durations in minutes', () => {
    expect(formatDurationNS(90 * 1_000_000_000)).toBe('1.5 min');
  });
  it('renders multi-hour durations in hours', () => {
    expect(formatDurationNS(2 * 3600 * 1_000_000_000)).toBe('2.0 h');
  });
});

describe('formatCostCents', () => {
  it('renders $0.00 for zero / undefined', () => {
    expect(formatCostCents(undefined)).toBe('$0.00');
    expect(formatCostCents(0)).toBe('$0.00');
  });
  it('renders cents as a dollar amount', () => {
    expect(formatCostCents(125)).toBe('$1.25');
  });
});

describe('formatTokens', () => {
  it('renders 0 for zero / undefined', () => {
    expect(formatTokens(undefined)).toBe('0');
  });
  it('renders thousands separators', () => {
    expect(formatTokens(12_345)).toBe('12,345');
  });
});

describe('formatRelative', () => {
  it('renders "never" for undefined', () => {
    expect(formatRelative(undefined)).toBe('never');
  });
  it('renders seconds-ago for a recent timestamp', () => {
    const now = new Date('2026-05-20T12:00:30Z');
    expect(formatRelative('2026-05-20T12:00:00Z', now)).toBe('30s ago');
  });
  it('renders hours-ago for an older timestamp', () => {
    const now = new Date('2026-05-20T12:00:00Z');
    expect(formatRelative('2026-05-20T09:00:00Z', now)).toBe('3h ago');
  });
});

describe('shortSessionID', () => {
  it('passes a short id through', () => {
    expect(shortSessionID('s-abc')).toBe('s-abc');
  });
  it('truncates a long id with an ellipsis', () => {
    expect(shortSessionID('0123456789abcdefghij')).toBe('0123456789abcdef…');
  });
});

describe('statusKind', () => {
  it('maps each status onto the StatusChip token scale', () => {
    expect(statusKind('running')).toBe('success');
    expect(statusKind('paused')).toBe('warning');
    expect(statusKind('failed')).toBe('danger');
    expect(statusKind('completed')).toBe('neutral');
  });
});
