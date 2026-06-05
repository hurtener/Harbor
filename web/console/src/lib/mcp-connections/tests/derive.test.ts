/**
 * MCP Connections pure-projection tests (Phase 108m / D-185).
 *
 * Pins `derive.ts` against the REAL wire shapes the live probe captured from
 * the validation runtime's `youtube` MCP server (PAGE-POLISH §3). The two
 * load-bearing cases:
 *
 *   (a) the `GET /v1/events` SSE payload marshals exported Go fields UNTAGGED
 *       — so the recent-event decoders read PascalCase (`Source`, `ToolName`,
 *       `URI`, `ErrorClass`), NOT snake_case. A decoder reading the wrong
 *       casing silently drops every value (§3 casing gotcha);
 *   (b) `tool.failed` carries no server field, so it is attributed to a
 *       server ONLY by membership in the server's owned tool-name set — an
 *       event that cannot be attributed is dropped, never mislabelled (§13).
 */
import { describe, expect, it } from 'vitest';
import type { Event } from '$lib/protocol/events.js';
import type { MCPServerView } from '$lib/protocol/mcp.js';
import { ProtocolError } from '$lib/protocol/errors.js';
import {
  mcpStatusKind,
  mcpStateLabel,
  toPageError,
  displayStatus,
  relativeTime,
  serverStateCounts,
  MCP_RECENT_EVENT_TYPES,
  extractEventSource,
  extractEventToolName,
  summarizeMcpEvent,
  projectServerEvents
} from '../derive.js';

function server(name: string, state: MCPServerView['state']): MCPServerView {
  return {
    name,
    transport: 'stdio',
    url_or_command: 'uvx mcp-youtube',
    state,
    last_discovery_at: '2026-06-04T21:38:59.197508-03:00',
    tool_count: 6,
    resource_count: 0,
    prompt_count: 0,
    recent_latency_ms: 0,
    error_rate_per_min: 0,
    oauth_binding_count: 0,
    raw_html_trusted: false
  };
}

/** Builds a wireEvent with a PascalCase payload — the real SSE shape. */
function ev(type: string, sequence: number, payload: Record<string, unknown>): Event {
  return {
    type,
    sequence,
    occurred_at: '2026-06-04T21:40:00Z',
    tenant: 'dev',
    user: 'dev',
    session: 'dev',
    payload
  };
}

describe('status mapping', () => {
  it('maps every MCP state onto the canonical chip scale', () => {
    expect(mcpStatusKind('online')).toBe('success');
    expect(mcpStatusKind('reconnecting')).toBe('warning');
    expect(mcpStatusKind('auth_pending')).toBe('accent');
    expect(mcpStatusKind('error')).toBe('danger');
    expect(mcpStatusKind('offline')).toBe('neutral');
  });

  it('labels each state', () => {
    expect(mcpStateLabel('online')).toBe('Online');
    expect(mcpStateLabel('error')).toBe('Errored');
    expect(mcpStateLabel('auth_pending')).toBe('Auth pending');
  });
});

describe('toPageError', () => {
  it('keeps the ProtocolError wire code', () => {
    expect(toPageError(new ProtocolError('not_found', 'gone', 404))).toEqual({
      code: 'not_found',
      message: 'gone'
    });
  });

  it('collapses a non-protocol throw to runtime_error', () => {
    expect(toPageError(new Error('boom'))).toEqual({ code: 'runtime_error', message: 'boom' });
    expect(toPageError('weird')).toEqual({ code: 'runtime_error', message: 'unknown error' });
  });
});

describe('displayStatus', () => {
  it('passes loading / error / disconnected through', () => {
    expect(displayStatus('loading', 5)).toBe('loading');
    expect(displayStatus('error', 5)).toBe('error');
    expect(displayStatus('disconnected', 0)).toBe('disconnected');
  });

  it('derives ready/empty live from the row count', () => {
    expect(displayStatus('ready', 3)).toBe('ready');
    expect(displayStatus('ready', 0)).toBe('empty');
    expect(displayStatus('empty', 2)).toBe('ready');
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-06-04T21:40:00Z');

  it('renders the Go zero time as "never"', () => {
    expect(relativeTime('0001-01-01T00:00:00Z', now)).toBe('never');
    expect(relativeTime('', now)).toBe('never');
    expect(relativeTime('not-a-date', now)).toBe('never');
  });

  it('renders relative ages', () => {
    expect(relativeTime('2026-06-04T21:39:30Z', now)).toBe('just now');
    expect(relativeTime('2026-06-04T21:10:00Z', now)).toBe('30m ago');
    expect(relativeTime('2026-06-04T18:40:00Z', now)).toBe('3h ago');
    expect(relativeTime('2026-06-02T21:40:00Z', now)).toBe('2d ago');
  });
});

describe('serverStateCounts', () => {
  it('rolls a page up per-state', () => {
    const counts = serverStateCounts([
      server('a', 'online'),
      server('b', 'online'),
      server('c', 'error'),
      server('d', 'offline')
    ]);
    expect(counts).toEqual({
      total: 4,
      online: 2,
      reconnecting: 0,
      offline: 1,
      auth_pending: 0,
      error: 1
    });
  });

  it('zeroes an empty page', () => {
    expect(serverStateCounts([]).total).toBe(0);
  });
});

describe('recent-event projection', () => {
  it('subscribes to the three documented types', () => {
    expect([...MCP_RECENT_EVENT_TYPES]).toEqual([
      'mcp.resource_updated',
      'tool.auth_required',
      'tool.failed'
    ]);
  });

  it('reads the PascalCase Source / ToolName off the real SSE payload', () => {
    expect(extractEventSource({ Source: 'youtube', URI: 'res://x' })).toBe('youtube');
    // snake_case fallback (a future tagged generator)
    expect(extractEventSource({ source_name: 'youtube' })).toBe('youtube');
    expect(extractEventSource({})).toBe('');
    expect(extractEventToolName({ ToolName: 'youtube_get_metadata' })).toBe('youtube_get_metadata');
    expect(extractEventToolName({})).toBe('');
  });

  it('summarizes each event type from its real payload', () => {
    expect(summarizeMcpEvent(ev('mcp.resource_updated', 1, { Source: 'youtube', URI: 'res://v/1' }))).toBe(
      'Resource updated: res://v/1'
    );
    expect(summarizeMcpEvent(ev('tool.auth_required', 2, { Source: 'youtube', BindingScope: 'user' }))).toBe(
      'OAuth required (user scope)'
    );
    expect(
      summarizeMcpEvent(
        ev('tool.failed', 3, { ToolName: 'youtube_get_metadata', Transport: 'MCP', ErrorClass: 'transient' })
      )
    ).toBe('Tool failed: youtube_get_metadata — transient');
  });

  it('attributes resource/auth events by Source and tool.failed by owned tool name', () => {
    const events: Event[] = [
      ev('mcp.resource_updated', 5, { Source: 'youtube', URI: 'res://v/1' }),
      ev('mcp.resource_updated', 4, { Source: 'other-server', URI: 'res://z' }),
      ev('tool.auth_required', 3, { Source: 'youtube', BindingScope: 'user' }),
      ev('tool.failed', 2, { ToolName: 'youtube_get_metadata', Transport: 'MCP', ErrorClass: 'timeout' }),
      ev('tool.failed', 1, { ToolName: 'artifact_fetch', Transport: 'in-proc', ErrorClass: 'permanent' })
    ];
    const owned = new Set(['youtube_get_metadata', 'youtube_download_video']);
    const rows = projectServerEvents(events, 'youtube', owned);

    // The other-server resource event and the in-proc artifact_fetch failure
    // are NOT attributed to youtube — dropped, never mislabelled.
    expect(rows.map((r) => r.sequence)).toEqual([5, 3, 2]);
    expect(rows[0].summary).toBe('Resource updated: res://v/1');
    expect(rows[2].summary).toBe('Tool failed: youtube_get_metadata — timeout');
  });

  it('is honestly empty when no server is selected', () => {
    expect(projectServerEvents([ev('tool.failed', 1, { ToolName: 'x' })], '', new Set())).toEqual([]);
  });
});
