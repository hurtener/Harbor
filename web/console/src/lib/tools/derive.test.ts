// Harbor Console — Tools page derive() projection unit tests
// (Phase 108k / D-183).
//
// Locks the pure projections against the real wire shapes the live probe
// captured. The load-bearing cases: the Go zero time a never-invoked tool
// carries renders an honest "never" (not a fabricated "just now"); the
// StatusChip mappings are exhaustive over the wire enums; the ready/empty
// split is derived LIVE from the loaded-row count (the Events-page D-180
// lesson) while loading/error/disconnected pass straight through.

import { describe, it, expect } from 'vitest';
import {
  toPageError,
  lastUsed,
  oauthKind,
  approvalKind,
  statusKind,
  displayStatus
} from './derive.js';
import { ProtocolError } from '$lib/protocol/errors.js';

const NOW = Date.parse('2026-06-02T12:00:00Z');

describe('lastUsed', () => {
  it('renders the Go zero time as an honest "never" (the never-invoked case)', () => {
    // Every youtube_* tool the live probe returned carried this exact value.
    expect(lastUsed('0001-01-01T00:00:00Z', NOW)).toBe('never');
  });

  it('renders "never" for an empty or unparseable timestamp', () => {
    expect(lastUsed('', NOW)).toBe('never');
    expect(lastUsed('not-a-date', NOW)).toBe('never');
  });

  it('renders sub-minute as "just now"', () => {
    expect(lastUsed('2026-06-02T11:59:40Z', NOW)).toBe('just now');
  });

  it('renders minute / hour / day buckets', () => {
    expect(lastUsed('2026-06-02T11:30:00Z', NOW)).toBe('30m ago');
    expect(lastUsed('2026-06-02T09:00:00Z', NOW)).toBe('3h ago');
    expect(lastUsed('2026-05-30T12:00:00Z', NOW)).toBe('3d ago');
  });
});

describe('oauthKind', () => {
  it('maps each OAuth status onto the StatusChip scale', () => {
    expect(oauthKind('Bound')).toBe('success');
    expect(oauthKind('Required')).toBe('accent');
    expect(oauthKind('Expired')).toBe('danger');
    expect(oauthKind('n/a')).toBe('neutral');
  });
});

describe('approvalKind', () => {
  it('maps each approval policy onto the StatusChip scale', () => {
    expect(approvalKind('auto')).toBe('success');
    expect(approvalKind('gated')).toBe('warning');
    expect(approvalKind('denied')).toBe('danger');
  });
});

describe('statusKind', () => {
  it('maps each health pill onto the StatusChip scale', () => {
    expect(statusKind('Healthy')).toBe('success');
    expect(statusKind('Degraded')).toBe('warning');
    expect(statusKind('Offline')).toBe('danger');
  });
});

describe('toPageError', () => {
  it('keeps a ProtocolError wire code (e.g. the not_found the probe saw)', () => {
    const e = toPageError(new ProtocolError('not_found', 'tools.describe: tool not found', 404));
    expect(e).toEqual({ code: 'not_found', message: 'tools.describe: tool not found' });
  });

  it('collapses a plain Error onto runtime_error (no silent swallow)', () => {
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
  });

  it('collapses a non-Error throw onto runtime_error', () => {
    expect(toPageError('weird')).toEqual({ code: 'runtime_error', message: 'unknown error' });
  });
});

describe('displayStatus', () => {
  it('passes loading / error / disconnected straight through', () => {
    expect(displayStatus('loading', 0)).toBe('loading');
    expect(displayStatus('error', 7)).toBe('error');
    expect(displayStatus('disconnected', 0)).toBe('disconnected');
  });

  it('derives ready when the loaded page has rows', () => {
    expect(displayStatus('ready', 7)).toBe('ready');
    expect(displayStatus('empty', 7)).toBe('ready');
  });

  it('derives empty when a filter drops every row (D-180 — derive live)', () => {
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('empty', 0)).toBe('empty');
  });
});
