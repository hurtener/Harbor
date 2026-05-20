/**
 * McpListState / McpDetailState tests (D-121, MCP refactor).
 *
 * The MCP page state is migrated off the legacy `mcpApi` object literal
 * onto the unified `HarborClient` + `connection.ts`. These tests pin the
 * three behaviours the refactor is responsible for:
 *
 *   (a) the FOUR-state `PageStatus` contract — including the Disconnected
 *       branch the legacy state machine entirely lacked;
 *   (b) a `ProtocolError` (carrying the HTTP status the legacy
 *       `ProtocolCallError` dropped) routes into the `error` status;
 *   (c) data flows through `HarborClient` — the request hits the
 *       `/v1/control/mcp.servers.*` control surface, never a bespoke URL.
 *
 * The transport is exercised with a stubbed `globalThis.fetch`; the
 * connection is seeded into `localStorage` (the `connection.ts` storage
 * convention) so no live Runtime is required.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { STORAGE_KEYS } from '$lib/connection.js';
import { McpListState, McpDetailState } from '../state.svelte.js';

function seedConnection(): void {
  localStorage.setItem(STORAGE_KEYS.baseURL, 'http://127.0.0.1:18080');
  localStorage.setItem(STORAGE_KEYS.token, 'dummy-token');
  localStorage.setItem(STORAGE_KEYS.tenant, 't1');
  localStorage.setItem(STORAGE_KEYS.user, 'u1');
  localStorage.setItem(STORAGE_KEYS.session, 's1');
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' }
  });
}

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('McpListState — four-state PageStatus contract', () => {
  it('enters the disconnected status when no Runtime is attached', async () => {
    const list = new McpListState();
    await list.load();
    expect(list.status).toBe('disconnected');
    expect(list.error).toBeNull();
  });

  it('routes a populated list onto the control surface and is ready', async () => {
    seedConnection();
    const fetchImpl = vi.fn(async () =>
      jsonResponse({
        servers: [
          {
            name: 'github-server',
            transport: 'http+sse',
            url_or_command: 'https://mcp.example/github',
            state: 'online',
            last_discovery_at: '2026-05-20T00:00:00Z',
            tool_count: 4,
            resource_count: 1,
            prompt_count: 0,
            recent_latency_ms: 12,
            error_rate_per_min: 0,
            oauth_binding_count: 1,
            raw_html_trusted: false
          }
        ],
        total: 1,
        protocol_version: '1.0.0'
      })
    );
    vi.stubGlobal('fetch', fetchImpl);

    const list = new McpListState();
    await list.load();

    expect(fetchImpl).toHaveBeenCalledOnce();
    const [url] = fetchImpl.mock.calls[0] as unknown as [string];
    expect(url).toBe('http://127.0.0.1:18080/v1/control/mcp.servers.list');
    expect(list.status).toBe('ready');
    expect(list.servers).toHaveLength(1);
    expect(list.total).toBe(1);
  });

  it('enters the empty status when the list returns zero rows', async () => {
    seedConnection();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ servers: [], total: 0, protocol_version: '1.0.0' }))
    );

    const list = new McpListState();
    await list.load();
    expect(list.status).toBe('empty');
  });

  it('routes a ProtocolError into the error status and keeps the HTTP status', async () => {
    seedConnection();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ code: 'scope_mismatch', message: 'denied' }, 403))
    );

    const list = new McpListState();
    await list.load();

    expect(list.status).toBe('error');
    expect(list.error?.code).toBe('scope_mismatch');
    // The Error status suppresses any stale primary view (CONVENTIONS.md §4).
    expect(list.servers).toHaveLength(0);
  });

  it('search narrows the visible rows Console-side without a Protocol call', async () => {
    seedConnection();
    const fetchImpl = vi.fn(async () =>
      jsonResponse({
        servers: [
          {
            name: 'github-server',
            transport: 'http+sse',
            url_or_command: 'https://mcp.example/github',
            state: 'online',
            last_discovery_at: '',
            tool_count: 0,
            resource_count: 0,
            prompt_count: 0,
            recent_latency_ms: 0,
            error_rate_per_min: 0,
            oauth_binding_count: 0,
            raw_html_trusted: false
          },
          {
            name: 'slack-server',
            transport: 'stdio',
            url_or_command: 'slack-mcp',
            state: 'offline',
            last_discovery_at: '',
            tool_count: 0,
            resource_count: 0,
            prompt_count: 0,
            recent_latency_ms: 0,
            error_rate_per_min: 0,
            oauth_binding_count: 0,
            raw_html_trusted: false
          }
        ],
        total: 2,
        protocol_version: '1.0.0'
      })
    );
    vi.stubGlobal('fetch', fetchImpl);

    const list = new McpListState();
    await list.load();
    expect(list.visibleServers).toHaveLength(2);

    list.setSearch('slack');
    expect(list.visibleServers).toHaveLength(1);
    expect(list.visibleServers[0].name).toBe('slack-server');
    // Search is Console-side: no extra fetch.
    expect(fetchImpl).toHaveBeenCalledOnce();
  });
});

describe('McpDetailState — four-state contract + control-surface routing', () => {
  it('enters the disconnected status when no Runtime is attached', async () => {
    const detail = new McpDetailState();
    await detail.load('github-server');
    expect(detail.status).toBe('disconnected');
  });

  it('loads the per-server header onto the control surface', async () => {
    seedConnection();
    const fetchImpl = vi.fn(async () =>
      jsonResponse({
        server: {
          name: 'github-server',
          transport: 'http+sse',
          url_or_command: 'https://mcp.example/github',
          state: 'online',
          last_discovery_at: '',
          tool_count: 2,
          resource_count: 0,
          prompt_count: 0,
          recent_latency_ms: 0,
          error_rate_per_min: 0,
          oauth_binding_count: 0,
          raw_html_trusted: false
        },
        display_modes_advertised: [],
        content_shapes: [],
        tool_policy: { timeout_ms: 5000, max_retries: 2, concurrency_cap: 4 },
        bindings_summary: [],
        protocol_version: '1.0.0'
      })
    );
    vi.stubGlobal('fetch', fetchImpl);

    const detail = new McpDetailState();
    await detail.load('github-server');

    const [url] = fetchImpl.mock.calls[0] as unknown as [string];
    expect(url).toBe('http://127.0.0.1:18080/v1/control/mcp.servers.get');
    expect(detail.status).toBe('ready');
    expect(detail.server?.name).toBe('github-server');
    expect(detail.toolPolicy?.timeout_ms).toBe(5000);
  });

  it('maps a not-found ProtocolError into the error status', async () => {
    seedConnection();
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => jsonResponse({ code: 'not_found', message: 'gone' }, 404))
    );

    const detail = new McpDetailState();
    await detail.load('__missing__');
    expect(detail.status).toBe('error');
    expect(detail.error?.code).toBe('not_found');
    expect(detail.server).toBeNull();
  });
});
