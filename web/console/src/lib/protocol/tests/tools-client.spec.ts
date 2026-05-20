/**
 * ToolsClient tests (Phase 73f / D-116).
 *
 * The typed Tools Protocol client is exercised with an injected
 * `fetchImpl` stub so the client is verified without a live Runtime:
 * the request shape (route, identity body, bearer header) and the
 * error mapping onto `ToolsProtocolError`.
 */
import { describe, expect, it, vi } from 'vitest';
import {
  ToolsClient,
  ToolsProtocolError,
  type ToolListResponse
} from '../tools.js';

const IDENTITY = { tenant: 't1', user: 'u1', session: 's1' };

function okResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' }
  });
}

function errorResponse(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ code, message }), {
    status,
    headers: { 'Content-Type': 'application/json' }
  });
}

const EMPTY_LIST: ToolListResponse = {
  tools: [],
  page: 1,
  page_size: 50,
  page_count: 1,
  total_rows: 0,
  aggregates: { total: 0, active: 0, pending_approval: 0, awaiting_oauth: 0 }
};

describe('ToolsClient.list', () => {
  it('POSTs to /v1/tools/list with the identity scope and bearer', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(okResponse(EMPTY_LIST));
    const client = new ToolsClient({
      baseURL: 'http://rt.test',
      token: 'tok-abc',
      identity: IDENTITY,
      fetchImpl: fetchImpl as unknown as typeof fetch
    });
    await client.list({ transports: ['MCP'] });

    expect(fetchImpl).toHaveBeenCalledOnce();
    const [url, init] = fetchImpl.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('http://rt.test/v1/tools/list');
    expect(init.method).toBe('POST');
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer tok-abc');
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.identity).toEqual(IDENTITY);
    expect(body.filter).toEqual({ transports: ['MCP'] });
  });
});

describe('ToolsClient admin methods', () => {
  it('POSTs tools.set_approval_policy with the policy', async () => {
    const fetchImpl = vi
      .fn()
      .mockResolvedValue(okResponse({ id: 'echo', policy: 'gated' }));
    const client = new ToolsClient({
      baseURL: 'http://rt.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: fetchImpl as unknown as typeof fetch
    });
    const resp = await client.setApprovalPolicy('echo', 'gated');
    expect(resp.policy).toBe('gated');
    const [url] = fetchImpl.mock.calls[0] as [string];
    expect(url).toBe('http://rt.test/v1/tools/set_approval_policy');
  });
});

describe('ToolsClient error mapping', () => {
  it('throws ToolsProtocolError carrying the canonical code on a 403', async () => {
    const fetchImpl = vi
      .fn()
      .mockResolvedValue(
        errorResponse(403, 'identity_scope_required', 'admin scope required')
      );
    const client = new ToolsClient({
      baseURL: 'http://rt.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: fetchImpl as unknown as typeof fetch
    });
    await expect(client.revokeOAuth('echo')).rejects.toMatchObject({
      code: 'identity_scope_required',
      status: 403
    });
  });

  it('throws ToolsProtocolError on a 404 not-found', async () => {
    const fetchImpl = vi
      .fn()
      .mockResolvedValue(errorResponse(404, 'not_found', 'tool not found'));
    const client = new ToolsClient({
      baseURL: 'http://rt.test',
      token: 't',
      identity: IDENTITY,
      fetchImpl: fetchImpl as unknown as typeof fetch
    });
    let caught: unknown;
    try {
      await client.get('ghost');
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(ToolsProtocolError);
    expect((caught as ToolsProtocolError).code).toBe('not_found');
  });
});
