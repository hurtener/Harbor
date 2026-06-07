// Harbor Console — Memory page pure projections (Phase 108n / D-186).
//
// The king-file refactor pulls every pure, side-effect-free projection the
// Memory page renders into this unit-testable module — the same split the
// Tools-108k / Agents-108l / MCP-108m rebuilds used. Nothing here touches the
// Protocol client, `localStorage`, the DOM, or any reactive state; the
// `MemoryPageState` controller and the views import these helpers, and
// `derive.test.ts` pins them against the real wire shapes the live probe
// captured (the validation runtime's `sqlite` / `rolling_summary` memory).
//
// Two load-bearing projections live here (PAGE-POLISH §3):
//
//   - `decodeMemoryValue` — `memory.get` returns the record value as a Go
//     `[]byte`, which `encoding/json` marshals as BASE64. The pre-chrome
//     viewer `JSON.parse`d it raw and rendered base64 gibberish; this decodes
//     base64 → UTF-8 → pretty JSON (the §3 wire-shape gotcha, fixed).
//   - `projectMemoryEvents` — the LIVE `events.subscribe` projection that
//     replaces the pre-chrome deferred event-feed card (the three `memory.*`
//     event types are shipped).
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live in
// the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { Event } from '$lib/protocol/events.js';

/* ================================================================ */
/* Status-chip mapping                                              */
/* ================================================================ */

/** Maps a memory strategy onto the shared `StatusChip` kind scale. */
export function strategyKind(strategy: string): StatusKind {
  if (strategy === 'rolling_summary') return 'success';
  if (strategy === 'truncation') return 'accent';
  return 'neutral';
}

/* ================================================================ */
/* Error + page-status projections                                  */
/* ================================================================ */

/** A page-friendly `{code, message}` projection of a thrown error. */
export type PageError = { code: string; message: string };

/**
 * Normalises any thrown value into a `{code, message}` the four-state
 * `<PageState>` boundary renders. A `ProtocolError` keeps its wire code
 * (`not_found` / `scope_mismatch` / …); anything else collapses to
 * `runtime_error` — never a silent swallow (CLAUDE.md §13).
 */
export function toPageError(err: unknown): PageError {
  if (err instanceof ProtocolError) {
    return { code: err.code, message: err.message };
  }
  return {
    code: 'runtime_error',
    message: err instanceof Error ? err.message : 'unknown error'
  };
}

/**
 * The status the list `<PageState>` boundary renders. `loading` / `error` /
 * `disconnected` pass through; the ready/empty split is derived LIVE from the
 * loaded-row count so a scope whose rows all dropped under a tight filter
 * still reads "empty" (the Events-page D-180 lesson).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}

/* ================================================================ */
/* Timestamps                                                       */
/* ================================================================ */

/**
 * An absolute `YYYY-MM-DD HH:MM:SS` label. Go's `time.Time` zero value
 * serialises to `0001-01-01T00:00:00Z` for a nullable timestamp that has no
 * value (records under the `truncation` strategy carry no `expires_at`); that
 * renders an honest `—`, never a literal year-1 date (CLAUDE.md §13).
 */
export function shortTime(iso: string | undefined): string {
  if (!iso || iso.startsWith('0001-01-01')) return '—';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '—' : d.toISOString().slice(0, 19).replace('T', ' ');
}

/**
 * A short relative label for the Created / Last updated columns (the mock §12
 * renders these relative). The Go zero time → `never`; an unparseable / empty
 * value → `—`.
 */
export function relativeTime(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return '—';
  if (iso.startsWith('0001-01-01')) return 'never';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';
  const deltaSec = Math.round((now - then) / 1000);
  if (deltaSec < 60) return 'just now';
  const deltaMin = Math.round(deltaSec / 60);
  if (deltaMin < 60) return `${deltaMin}m ago`;
  const deltaHr = Math.round(deltaMin / 60);
  if (deltaHr < 24) return `${deltaHr}h ago`;
  return `${Math.round(deltaHr / 24)}d ago`;
}

/* ================================================================ */
/* Value decode — base64 → UTF-8 → pretty JSON (PAGE-POLISH §3)      */
/* ================================================================ */

/** The shape of a decoded memory value for the detail viewer. */
export interface DecodedValue {
  /** The display text (pretty JSON, plain UTF-8, or the raw input). */
  text: string;
  /** How the bytes were interpreted — drives the viewer's label. */
  format: 'json' | 'text' | 'raw';
}

/** Decodes a base64 string to UTF-8, or returns null when it is not base64. */
function base64ToUtf8(b64: string): string | null {
  // A non-base64-shaped string (whitespace, length not a multiple of 4, stray
  // chars) is left to the raw path — never force-decode a plain value.
  if (b64.length === 0 || b64.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(b64)) {
    return null;
  }
  try {
    const bin = atob(b64);
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0));
    return new TextDecoder('utf-8', { fatal: false }).decode(bytes);
  } catch {
    return null;
  }
}

/**
 * Decodes a `memory.get` record value for the detail viewer. The Runtime
 * marshals the record value as a Go `[]byte`, which `encoding/json` emits as
 * BASE64 — so the bytes are base64-decoded to UTF-8 first, THEN pretty-printed
 * if the decoded text is JSON (the common case — a memory turn is a JSON blob).
 * A value that is not base64 (or whose decode is not JSON) falls through to the
 * plain text honestly, never silently dropped (CLAUDE.md §13).
 */
export function decodeMemoryValue(value: string | undefined): DecodedValue {
  if (!value) return { text: '', format: 'text' };
  const utf8 = base64ToUtf8(value);
  const decoded = utf8 ?? value;
  try {
    return { text: JSON.stringify(JSON.parse(decoded), null, 2), format: 'json' };
  } catch {
    return { text: decoded, format: utf8 !== null ? 'text' : 'raw' };
  }
}

/* ================================================================ */
/* Live memory-event feed — `events.subscribe` projection           */
/* ================================================================ */

/**
 * The three runtime-wide `memory.*` event types the right-rail event-feed card
 * subscribes to (page-memory.md §3 — all `[shipped]`): the fail-loud
 * identity-rejection (D-033), the driver health transition, and the
 * `OverflowDropOldest` recovery drop (D-035). Enumerated (not `[]`) so the
 * named-SSE-frame listeners register and the subscription URL stays bounded.
 */
export const MEMORY_EVENT_TYPES: readonly string[] = [
  'memory.identity_rejected',
  'memory.health_changed',
  'memory.recovery_dropped'
] as const;

/** One projected memory-event row for the right-rail feed. */
export interface MemoryEventEntry {
  sequence: number;
  type: string;
  at: string;
  summary: string;
}

/** Reads the first present field by name off an unknown payload object. */
function payloadField(payload: unknown, ...names: string[]): unknown {
  if (typeof payload !== 'object' || payload === null) return undefined;
  const rec = payload as Record<string, unknown>;
  for (const n of names) {
    if (n in rec) return rec[n];
  }
  return undefined;
}

/**
 * A short, operator-facing one-line summary of one `memory.*` event, derived
 * only from what its payload carries (the SSE payload marshals exported Go
 * fields UNTAGGED — PascalCase; the snake_case alias is read defensively). It
 * never invents detail the runtime did not emit (CLAUDE.md §13).
 */
export function summarizeMemoryEvent(ev: Event): string {
  // Field names mirror the REAL Go payloads (PascalCase on the SSE wire):
  //   MemoryIdentityRejectedPayload{Operation, Reason}
  //   HealthChangedPayload{PriorHealth, NewHealth, Reason}
  //   RecoveryDroppedPayload{Reason}
  const p = ev.payload;
  switch (ev.type) {
    case 'memory.identity_rejected': {
      const reason = payloadField(p, 'Reason', 'reason');
      const op = payloadField(p, 'Operation', 'operation');
      const opTail = typeof op === 'string' && op.length > 0 ? ` on ${op}` : '';
      return typeof reason === 'string' && reason.length > 0
        ? `Identity rejected${opTail} — ${reason} (D-033)`
        : `Identity rejected${opTail} (fail-loud, D-033)`;
    }
    case 'memory.health_changed': {
      const next = payloadField(p, 'NewHealth', 'new_health');
      const reason = payloadField(p, 'Reason', 'reason');
      const tail = typeof reason === 'string' && reason.length > 0 ? ` (${reason})` : '';
      return typeof next === 'string' && next.length > 0
        ? `Driver health changed: ${next}${tail}`
        : `Driver health changed${tail}`;
    }
    case 'memory.recovery_dropped': {
      const reason = payloadField(p, 'Reason', 'reason');
      return typeof reason === 'string' && reason.length > 0
        ? `Recovery dropped items — ${reason} (OverflowDropOldest, D-035)`
        : 'Recovery dropped items (OverflowDropOldest, D-035)';
    }
    default:
      return ev.type;
  }
}

/**
 * Projects the live SSE event page down to the memory-event rows for the
 * right-rail feed, newest-first. The `memory.*` events are runtime-wide (not
 * per-record), so every matching event is surfaced. The runtime has no durable
 * event read-back, so this is a LIVE-only view: it is honestly empty until
 * events stream in (CLAUDE.md §13 — never a fabricated backlog).
 */
export function projectMemoryEvents(events: Event[]): MemoryEventEntry[] {
  const out: MemoryEventEntry[] = [];
  for (const ev of events) {
    if (!MEMORY_EVENT_TYPES.includes(ev.type)) continue;
    out.push({
      sequence: ev.sequence,
      type: ev.type,
      at: ev.occurred_at,
      summary: summarizeMemoryEvent(ev)
    });
  }
  return out;
}
