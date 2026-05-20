/**
 * MemoryClient tests (Phase 73j / D-118) — the typed Memory-page
 * Protocol client. The client is exercised against an injected `fetch`
 * so the wire shape (route, method, bearer header, body) and the error
 * classification (a non-2xx → MemoryProtocolError carrying the
 * canonical Code) are pinned without a live runtime.
 */
import { describe, expect, it } from 'vitest';
import {
  MemoryClient,
  MemoryProtocolError,
  type MemoryListResponse,
  type MemoryGetResponse,
  type MemoryHealthResponse
} from '../protocol-memory.js';

const IDENTITY = { tenantID: 't-test', userID: 'u-test' };

/** Builds a fetch stub that records the last request and returns `resp`. */
function stubFetch(
  resp: { status: number; body: unknown },
  capture?: { route?: string; method?: string; auth?: string; body?: string }
): typeof fetch {
  return (async (input: RequestInfo | URL, init?: RequestInit) => {
    if (capture) {
      capture.route = String(input);
      capture.method = init?.method;
      capture.auth = (init?.headers as Record<string, string> | undefined)?.[
        'Authorization'
      ];
      capture.body = typeof init?.body === 'string' ? init.body : undefined;
    }
    return new Response(JSON.stringify(resp.body), { status: resp.status });
  }) as typeof fetch;
}

describe('MemoryClient: typed memory.* Protocol client', () => {
  it('memory.list issues a POST to /v1/memory/list with the bearer', async () => {
    const listResp: MemoryListResponse = {
      items: [],
      page: 1,
      page_size: 50,
      page_count: 0,
      total_rows: 0,
      aggregates: {
        total: 0,
        expiring_in_1h: 0,
        identity_rejected_24h: 0,
        recovery_dropped_24h: 0
      },
      protocol_version: '1.0.0'
    };
    const capture: { route?: string; method?: string; auth?: string; body?: string } = {};
    const client = new MemoryClient({
      baseURL: 'http://runtime.test/',
      token: 'tok-abc',
      identity: IDENTITY,
      fetchImpl: stubFetch({ status: 200, body: listResp }, capture)
    });

    const got = await client.list({ filter: { scopes: ['session'] } });
    expect(got.protocol_version).toBe('1.0.0');
    expect(capture.route).toBe('http://runtime.test/v1/memory/list');
    expect(capture.method).toBe('POST');
    expect(capture.auth).toBe('Bearer tok-abc');
    expect(capture.body).toContain('"scopes":["session"]');
  });

  it('memory.get round-trips a light-value detail', async () => {
    const getResp: MemoryGetResponse = {
      detail: {
        item: {
          key: 'mem_abc',
          strategy: 'truncation',
          scope: 'session',
          identity: { tenant: 't', user: 'u', session: 's' },
          created_at: '2026-05-20T00:00:00Z',
          last_updated_at: '2026-05-20T00:00:00Z',
          size_bytes: 42,
          driver: 'inmem'
        },
        value: '{"x":1}',
        metadata: {}
      },
      protocol_version: '1.0.0'
    };
    const client = new MemoryClient({
      baseURL: 'http://runtime.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: stubFetch({ status: 200, body: getResp })
    });
    const got = await client.get({ key: 'mem_abc' });
    expect(got.detail.item.key).toBe('mem_abc');
    expect(got.detail.value).toBe('{"x":1}');
    expect(got.detail.value_artifact).toBeUndefined();
  });

  it('memory.health round-trips the aggregate counters', async () => {
    const healthResp: MemoryHealthResponse = {
      aggregate: {
        total: 7,
        expiring_in_1h: 1,
        identity_rejected_24h: 2,
        recovery_dropped_24h: 0,
        driver_by_scope: { session: 'inmem' }
      },
      protocol_version: '1.0.0'
    };
    const client = new MemoryClient({
      baseURL: 'http://runtime.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: stubFetch({ status: 200, body: healthResp })
    });
    const got = await client.health();
    expect(got.aggregate.total).toBe(7);
    expect(got.aggregate.driver_by_scope.session).toBe('inmem');
  });

  it('raises a MemoryProtocolError carrying the canonical Code on a non-2xx', async () => {
    const client = new MemoryClient({
      baseURL: 'http://runtime.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: stubFetch({
        status: 403,
        body: {
          code: 'identity_scope_required',
          message: 'cross-tenant memory.list requires a verified admin scope claim'
        }
      })
    });
    await expect(
      client.list({ filter: { tenant_ids: ['t-other'] } })
    ).rejects.toThrowError(MemoryProtocolError);

    try {
      await client.list({ filter: { tenant_ids: ['t-other'] } });
    } catch (e) {
      expect(e).toBeInstanceOf(MemoryProtocolError);
      const err = e as MemoryProtocolError;
      expect(err.code).toBe('identity_scope_required');
      expect(err.status).toBe(403);
    }
  });

  it('raises identity_required on a 401', async () => {
    const client = new MemoryClient({
      baseURL: 'http://runtime.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: stubFetch({
        status: 401,
        body: { code: 'identity_required', message: 'identity scope incomplete' }
      })
    });
    try {
      await client.health();
      expect.unreachable('health() should have thrown');
    } catch (e) {
      expect((e as MemoryProtocolError).code).toBe('identity_required');
    }
  });
});
