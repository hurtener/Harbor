/**
 * Memory page pure-projection tests (Phase 108n / D-186).
 *
 * Pins `derive.ts`. The two load-bearing cases (PAGE-POLISH §3):
 *
 *   (a) `decodeMemoryValue` — the Runtime marshals the `memory.get` value as a
 *       Go `[]byte`, which `encoding/json` emits as BASE64. The pre-chrome
 *       viewer `JSON.parse`d the raw base64 and rendered gibberish; this proves
 *       the base64 → UTF-8 → pretty-JSON decode (incl. a multibyte UTF-8 value).
 *   (b) the `memory.*` event SSE payload marshals exported Go fields UNTAGGED —
 *       PascalCase — so `summarizeMemoryEvent` reads `Missing` / `State` /
 *       `Dropped`, not snake_case.
 */
import { describe, expect, it } from 'vitest';
import type { Event } from '$lib/protocol/events.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import {
  strategyKind,
  toPageError,
  displayStatus,
  shortTime,
  relativeTime,
  decodeMemoryValue,
  MEMORY_EVENT_TYPES,
  summarizeMemoryEvent,
  projectMemoryEvents
} from '../derive.js';

/** Encodes a UTF-8 string to base64 the way Go marshals a []byte value. */
function goBytesBase64(s: string): string {
  const bytes = new TextEncoder().encode(s);
  let bin = '';
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function ev(type: string, sequence: number, payload: Record<string, unknown>): Event {
  return {
    type,
    sequence,
    occurred_at: '2026-06-05T00:00:00Z',
    tenant: 'dev',
    user: 'dev',
    session: 'dev',
    payload
  };
}

describe('status + error projections', () => {
  it('maps strategy onto the chip scale', () => {
    expect(strategyKind('rolling_summary')).toBe('success');
    expect(strategyKind('truncation')).toBe('accent');
    expect(strategyKind('none')).toBe('neutral');
  });

  it('keeps the ProtocolError code / collapses others', () => {
    expect(toPageError(new ProtocolError('scope_mismatch', 'no', 403))).toEqual({
      code: 'scope_mismatch',
      message: 'no'
    });
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
  });

  it('derives ready/empty live', () => {
    expect(displayStatus('loading', 5)).toBe('loading');
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('ready', 3)).toBe('ready');
  });
});

describe('timestamps', () => {
  const now = Date.parse('2026-06-05T00:00:00Z');
  it('renders the Go zero time honestly', () => {
    expect(shortTime('0001-01-01T00:00:00Z')).toBe('—');
    expect(shortTime(undefined)).toBe('—');
    expect(relativeTime('0001-01-01T00:00:00Z', now)).toBe('never');
    expect(relativeTime(undefined, now)).toBe('—');
  });
  it('renders relative ages', () => {
    expect(relativeTime('2026-06-04T23:30:00Z', now)).toBe('30m ago');
    expect(relativeTime('2026-06-03T00:00:00Z', now)).toBe('2d ago');
  });
});

describe('decodeMemoryValue (the base64 wire-shape fix)', () => {
  it('base64-decodes a Go []byte JSON value and pretty-prints it', () => {
    const value = goBytesBase64('{"user_message":"Summarise the archived threads","assistant_response":"ok"}');
    const out = decodeMemoryValue(value);
    expect(out.format).toBe('json');
    // Pretty-printed (multi-line) JSON, not the raw base64.
    expect(out.text).toContain('"user_message": "Summarise the archived threads"');
    expect(out.text).not.toBe(value);
  });

  it('decodes a multibyte UTF-8 value (smart quotes) correctly', () => {
    const value = goBytesBase64('{"assistant_response":"I can’t do that — yet"}');
    const out = decodeMemoryValue(value);
    expect(out.text).toContain('I can’t do that — yet');
  });

  it('falls through to plain text for a non-base64 value', () => {
    const out = decodeMemoryValue('not base64 !!!');
    expect(out.format).toBe('raw');
    expect(out.text).toBe('not base64 !!!');
  });

  it('is empty for an absent value', () => {
    expect(decodeMemoryValue(undefined)).toEqual({ text: '', format: 'text' });
  });
});

describe('memory-event projection', () => {
  it('subscribes to the three documented types', () => {
    expect([...MEMORY_EVENT_TYPES]).toEqual([
      'memory.identity_rejected',
      'memory.health_changed',
      'memory.recovery_dropped'
    ]);
  });

  it('summarizes each type from its real PascalCase payload', () => {
    expect(
      summarizeMemoryEvent(ev('memory.identity_rejected', 1, { Operation: 'AddTurn', Reason: 'user_id empty' }))
    ).toBe('Identity rejected on AddTurn — user_id empty (D-033)');
    expect(summarizeMemoryEvent(ev('memory.health_changed', 2, { NewHealth: 'degraded', Reason: 'retries_exhausted' }))).toBe(
      'Driver health changed: degraded (retries_exhausted)'
    );
    expect(summarizeMemoryEvent(ev('memory.recovery_dropped', 3, { Reason: 'backlog_overflow' }))).toBe(
      'Recovery dropped items — backlog_overflow (OverflowDropOldest, D-035)'
    );
  });

  it('projects only memory.* events, preserving order, dropping others', () => {
    const rows = projectMemoryEvents([
      ev('memory.health_changed', 5, { NewHealth: 'degraded' }),
      ev('tool.failed', 4, { ToolName: 'x' }),
      ev('memory.identity_rejected', 3, { Reason: 'tenant_id empty' })
    ]);
    expect(rows.map((r) => r.sequence)).toEqual([5, 3]);
    expect(rows[0].type).toBe('memory.health_changed');
  });
});
