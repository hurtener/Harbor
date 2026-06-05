// Harbor Console — MCP Connections page pure projections
// (Phase 108m / D-185).
//
// The king-file refactor pulls every pure, side-effect-free projection the
// MCP Connections page renders into this unit-testable module — the same
// split the Tools-108k / Agents-108l rebuilds used (`tools/derive.ts`,
// `agents/derive.ts`). Nothing here touches the Protocol client,
// `localStorage`, the DOM, or any reactive state; the `McpListState` /
// `McpDetailState` controllers and the views import these helpers, and the
// `derive.test.ts` suite pins them against the real wire shapes the live
// probe captured (the validation runtime's `youtube` MCP server).
//
// This module FOLDS IN the former `status.ts` (the `mcpStatusKind` /
// `mcpStateLabel` mappings) so there is one MCP-projection home, and adds
// the relative-time, server-state-count, and live-event-projection helpers
// the deepened detail rail needs.
//
// No Svelte runes here (a plain `.ts` module); design-token concerns live in
// the `.svelte` views, not in these projections.

import type { StatusKind } from '$lib/components/ui/StatusChip.svelte';
import type { PageStatus } from '$lib/components/ui/PageState.svelte';
import { ProtocolError } from '$lib/protocol/errors.js';
import type { Event } from '$lib/protocol/events.js';
import type { MCPServerState, MCPServerView } from '$lib/protocol/mcp.js';

/* ================================================================ */
/* Status-chip mapping (folded in from the former status.ts)        */
/* ================================================================ */

/** Maps an MCP server `state` onto the shared `StatusChip` kind scale. */
export function mcpStatusKind(state: MCPServerState): StatusKind {
  switch (state) {
    case 'online':
      return 'success';
    case 'reconnecting':
      return 'warning';
    case 'auth_pending':
      return 'accent';
    case 'error':
      return 'danger';
    case 'offline':
    default:
      return 'neutral';
  }
}

/** A human-facing label for an MCP server `state`. */
export function mcpStateLabel(state: MCPServerState): string {
  switch (state) {
    case 'online':
      return 'Online';
    case 'reconnecting':
      return 'Reconnecting';
    case 'offline':
      return 'Offline';
    case 'auth_pending':
      return 'Auth pending';
    case 'error':
      return 'Errored';
    default:
      return state;
  }
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
 * `disconnected` pass through; the ready/empty split is derived LIVE from
 * the loaded-row count so a catalog whose rows all dropped under a tight
 * facet still reads "empty" (the Events-page D-180 lesson — derive the
 * display state, never a `status` set once at load that won't flip when
 * data streams in).
 */
export function displayStatus(status: PageStatus, rowCount: number): PageStatus {
  if (status === 'loading' || status === 'error' || status === 'disconnected') {
    return status;
  }
  return rowCount > 0 ? 'ready' : 'empty';
}

/* ================================================================ */
/* Relative time                                                    */
/* ================================================================ */

/**
 * A short relative "last connect" label. The Runtime returns the Go zero
 * time (`0001-01-01T00:00:00Z`) for a server that never completed a
 * discovery handshake — that case renders an honest `never`, never a
 * fabricated "just now" (CLAUDE.md §13). An unparseable / empty value is
 * also `never`.
 */
export function relativeTime(iso: string, now: number = Date.now()): string {
  if (!iso || iso.startsWith('0001-01-01')) {
    return 'never';
  }
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) {
    return 'never';
  }
  const deltaSec = Math.round((now - then) / 1000);
  if (deltaSec < 0) return 'just now';
  if (deltaSec < 60) return 'just now';
  const deltaMin = Math.round(deltaSec / 60);
  if (deltaMin < 60) return `${deltaMin}m ago`;
  const deltaHr = Math.round(deltaMin / 60);
  if (deltaHr < 24) return `${deltaHr}h ago`;
  return `${Math.round(deltaHr / 24)}d ago`;
}

/* ================================================================ */
/* Catalog overview (the idle-rail state)                           */
/* ================================================================ */

/** The five server-state buckets the idle catalog-overview card rolls up. */
export interface ServerStateCounts {
  total: number;
  online: number;
  reconnecting: number;
  offline: number;
  auth_pending: number;
  error: number;
}

/**
 * Rolls up a server page into per-state counts for the idle catalog-overview
 * card. Pure projection of the loaded `mcp.servers.list` rows — never a
 * fabricated total (the disconnected page passes `[]`, so every count is 0).
 */
export function serverStateCounts(servers: MCPServerView[]): ServerStateCounts {
  const counts: ServerStateCounts = {
    total: servers.length,
    online: 0,
    reconnecting: 0,
    offline: 0,
    auth_pending: 0,
    error: 0
  };
  for (const s of servers) {
    counts[s.state] += 1;
  }
  return counts;
}

/* ================================================================ */
/* Recent-events feed — live `events.subscribe` projection          */
/* ================================================================ */

/**
 * The three event types the detail rail's Recent-events card subscribes to
 * (page-mcp-connections.md §4 / §5): the server's resource-update
 * notifications, the OAuth-flow start, and the transport-error subset of
 * tool failures. Enumerated (not `[]`) so the named-SSE-frame listeners
 * register and the subscription URL stays bounded (subscription.svelte.ts).
 */
export const MCP_RECENT_EVENT_TYPES: readonly string[] = [
  'mcp.resource_updated',
  'tool.auth_required',
  'tool.failed'
] as const;

/** One projected recent-event row for the detail rail's card. */
export interface RecentEventEntry {
  /** The per-bus monotonic sequence — the stable keyed-each key. */
  sequence: number;
  /** The event type, e.g. `tool.failed`. */
  type: string;
  /** RFC3339 timestamp of the event. */
  at: string;
  /** A short operator-facing summary. */
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
 * Extracts the MCP attachment source id an `mcp.resource_updated` /
 * `tool.auth_required` event carries. The `GET /v1/events` SSE payload
 * marshals exported Go fields UNTAGGED — so the field is PascalCase
 * (`Source`); the snake_case alias is read defensively for a future tagged
 * generator (PAGE-POLISH §3 casing rule).
 */
export function extractEventSource(payload: unknown): string {
  const v = payloadField(payload, 'Source', 'source', 'SourceName', 'source_name');
  return typeof v === 'string' ? v : '';
}

/** Extracts the `ToolName` a `tool.*` event carries (PascalCase SSE field). */
export function extractEventToolName(payload: unknown): string {
  const v = payloadField(payload, 'ToolName', 'tool_name');
  return typeof v === 'string' ? v : '';
}

/**
 * A short, operator-facing one-line summary of one recent MCP-scoped event,
 * derived only from what its payload carries — it never invents detail the
 * runtime did not emit (CLAUDE.md §13).
 */
export function summarizeMcpEvent(ev: Event): string {
  const p = ev.payload;
  switch (ev.type) {
    case 'mcp.resource_updated': {
      const uri = payloadField(p, 'URI', 'uri');
      return typeof uri === 'string' && uri.length > 0
        ? `Resource updated: ${uri}`
        : 'Resource updated';
    }
    case 'tool.auth_required': {
      const scope = payloadField(p, 'BindingScope', 'binding_scope');
      const tail = typeof scope === 'string' && scope.length > 0 ? ` (${scope} scope)` : '';
      return `OAuth required${tail}`;
    }
    case 'tool.failed': {
      const tool = extractEventToolName(p);
      const cls = payloadField(p, 'ErrorClass', 'error_class');
      const clsTail = typeof cls === 'string' && cls.length > 0 ? ` — ${cls}` : '';
      return tool ? `Tool failed: ${tool}${clsTail}` : `Tool failed${clsTail}`;
    }
    default:
      return ev.type;
  }
}

/**
 * Projects the live SSE event page down to the recent-event rows attributed
 * to ONE MCP server, newest-first (the subscription keeps its page
 * newest-first). Attribution (PAGE-POLISH §3 — verified against the real
 * Go payloads):
 *
 *   - `mcp.resource_updated` / `tool.auth_required` carry the MCP attachment
 *     `Source` id — matched directly against the server name.
 *   - `tool.failed` carries only `ToolName` (no server field), so it is
 *     attributed by membership in `serverToolNames` — the set of tool names
 *     this server owns (from `tools.list` filtered on `owner === name`).
 *
 * The runtime has no durable event read-back, so this is a LIVE-only view:
 * it is honestly empty until events stream in (CLAUDE.md §13 — never a
 * fabricated backlog). An event that cannot be attributed to this server is
 * dropped, NOT mislabelled.
 */
export function projectServerEvents(
  events: Event[],
  serverName: string,
  serverToolNames: ReadonlySet<string>
): RecentEventEntry[] {
  if (serverName === '') return [];
  const out: RecentEventEntry[] = [];
  for (const ev of events) {
    let belongs = false;
    if (ev.type === 'tool.failed') {
      belongs = serverToolNames.has(extractEventToolName(ev.payload));
    } else {
      belongs = extractEventSource(ev.payload) === serverName;
    }
    if (!belongs) continue;
    out.push({
      sequence: ev.sequence,
      type: ev.type,
      at: ev.occurred_at,
      summary: summarizeMcpEvent(ev)
    });
  }
  return out;
}
