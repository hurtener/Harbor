// Harbor Console — Flows-page typed client tests (Phase 73i / D-117).
//
// The FlowsClient is exercised with an injected `fetch` stub so the
// transport behaviour — body shape, identity headers, typed decode,
// fail-loud error mapping — is testable without a live Runtime.

import { describe, expect, it, vi } from 'vitest';
import { FlowsClient, FlowsClientError } from '../client';
import type { IdentityScope } from '../types';

const identity: IdentityScope = {
  tenant: 't1',
  user: 'u1',
  session: 's1',
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('FlowsClient', () => {
  it('posts list with the identity folded into the body', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse({ flows: [], page: 1, page_size: 50, page_count: 0, total_rows: 0 }),
    );
    const client = new FlowsClient({
      baseURL: 'http://runtime.test',
      token: 'tok',
      identity,
      fetchImpl,
    });
    await client.list({ filter: {} });
    expect(fetchImpl).toHaveBeenCalledOnce();
    const [url, init] = fetchImpl.mock.calls[0];
    expect(url).toBe('http://runtime.test/v1/flows/list');
    const body = JSON.parse((init as RequestInit).body as string);
    expect(body.identity).toEqual(identity);
    const headers = (init as RequestInit).headers as Record<string, string>;
    expect(headers.Authorization).toBe('Bearer tok');
    expect(headers['X-Harbor-Tenant']).toBe('t1');
  });

  it('decodes a typed describe response', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse({
        flow: { id: 'f1', name: 'f1', node_count: 2, edge_count: 1 },
        nodes: [],
        edges: [],
        budget_consumption: { requests_used: 0, cost_usd_used: 0, tokens_used: 0 },
      }),
    );
    const client = new FlowsClient({
      baseURL: 'http://runtime.test',
      token: 'tok',
      identity,
      fetchImpl,
    });
    const desc = await client.describe({ id: 'f1' });
    expect(desc.flow.id).toBe('f1');
  });

  it('maps a Protocol error body onto a typed FlowsClientError', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse(
        { code: 'identity_scope_required', message: 'cross-tenant needs admin' },
        403,
      ),
    );
    const client = new FlowsClient({
      baseURL: 'http://runtime.test',
      token: 'tok',
      identity,
      fetchImpl,
    });
    await expect(client.list({ filter: { tenants: ['t1', 't2'] } })).rejects.toMatchObject({
      code: 'identity_scope_required',
      status: 403,
    });
  });

  it('fails loud on a non-JSON error body', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      new Response('upstream exploded', { status: 502 }),
    );
    const client = new FlowsClient({
      baseURL: 'http://runtime.test',
      token: 'tok',
      identity,
      fetchImpl,
    });
    await expect(client.list({ filter: {} })).rejects.toBeInstanceOf(FlowsClientError);
  });

  it('posts a run request to the mutating route', async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse({ run_id: 'r1', status: 'running', started_at: '2026-05-20T12:00:00Z' }),
    );
    const client = new FlowsClient({
      baseURL: 'http://runtime.test',
      token: 'tok',
      identity,
      fetchImpl,
    });
    const resp = await client.run({ flow_id: 'f1', inputs: { k: 'v' } });
    expect(resp.run_id).toBe('r1');
    expect(fetchImpl.mock.calls[0][0]).toBe('http://runtime.test/v1/flows/run');
  });
});
