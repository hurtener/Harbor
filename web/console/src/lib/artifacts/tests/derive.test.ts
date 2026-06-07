/**
 * Artifacts page pure-projection tests (Phase 108o / D-187).
 *
 * Pins `derive.ts` — the size formatter, the source→chip mapping, the
 * error/display-status projections, the Go-zero-time relative label, and the
 * preview-family classifier.
 */
import { describe, expect, it } from 'vitest';
import { ProtocolError } from '$lib/protocol/errors.js';
import {
  toPageError,
  displayStatus,
  fmtSize,
  sourceKind,
  relativeTime,
  previewFamily
} from '../derive.js';

describe('error + status projections', () => {
  it('keeps the ProtocolError code / collapses others', () => {
    expect(toPageError(new ProtocolError('not_found', 'gone', 404))).toEqual({
      code: 'not_found',
      message: 'gone'
    });
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
  });

  it('derives ready/empty live', () => {
    expect(displayStatus('loading', 5)).toBe('loading');
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('ready', 3)).toBe('ready');
  });
});

describe('fmtSize', () => {
  it('formats bytes / KB / MB', () => {
    expect(fmtSize(512)).toBe('512 B');
    expect(fmtSize(2048)).toBe('2.0 KB');
    expect(fmtSize(5 * 1024 * 1024)).toBe('5.0 MB');
  });
});

describe('sourceKind', () => {
  it('maps each source onto the chip scale', () => {
    expect(sourceKind('user_upload')).toBe('accent');
    expect(sourceKind('system')).toBe('warning');
    expect(sourceKind('tool')).toBe('success');
    expect(sourceKind('planner')).toBe('neutral');
    expect(sourceKind(undefined)).toBe('neutral');
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-06-07T00:00:00Z');
  it('renders the Go zero time / empty as —', () => {
    expect(relativeTime('0001-01-01T00:00:00Z', now)).toBe('—');
    expect(relativeTime(undefined, now)).toBe('—');
    expect(relativeTime('not-a-date', now)).toBe('—');
  });
  it('renders relative ages', () => {
    expect(relativeTime('2026-06-06T23:30:00Z', now)).toBe('30m ago');
    expect(relativeTime('2026-06-05T00:00:00Z', now)).toBe('2d ago');
  });
});

describe('previewFamily', () => {
  it('classifies renderable mime families', () => {
    expect(previewFamily('image/png')).toBe('image');
    expect(previewFamily('application/pdf')).toBe('pdf');
    expect(previewFamily('text/plain')).toBe('text');
    expect(previewFamily('application/json')).toBe('text');
    expect(previewFamily('audio/mpeg')).toBe('audio');
    expect(previewFamily('application/octet-stream')).toBe('other');
    expect(previewFamily(undefined)).toBe('other');
  });
});
