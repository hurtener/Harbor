// Harbor Console — typed Sessions Protocol surface tests (Phase 245 /
// D-424, HA-63 consumer).
//
// Exercises the REAL `SessionsProtocol` wrapper over the REAL
// `HarborClient` transport (injected `fetchImpl`): the list projection is
// forwarded verbatim on the wire, an omitted projection stays
// default-compatible (no `projection` key is sent — the runtime resolves
// empty to `full`), and the chat-catalog request the Playground session
// switcher sends (`chatCatalogListRequest`) explicitly requests
// `projection: 'lifecycle'` with NO counter-dependent filter or sort (the
// runtime rejects those `invalid_request` on a lifecycle request).

import { describe, expect, it, vi } from 'vitest';

import { HarborClient } from '../harbor.js';
import { chatCatalogListRequest, SessionsProtocol } from '../sessions.js';
import type { RuntimeConnection } from '../../connection.js';

const CONNECTION: RuntimeConnection = {
  baseURL: 'http://127.0.0.1:18080',
  token: 'dummy-bearer-token',
  identity: { tenant: 't1', user: 'u1', session: 's1' },
  scopes: ['admin']
};

function okResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' }
  });
}

const EMPTY_LIST = { rows: [], next_cursor: '', truncated: false };

/** The JSON body the transport sent on the n-th request. */
function sentBody(
  fetchImpl: { mock: { calls: unknown[][] } },
  callIndex = 0
): Record<string, unknown> {
  const init = fetchImpl.mock.calls[callIndex][1] as RequestInit;
  return JSON.parse(init.body as string) as Record<string, unknown>;
}

describe('SessionsProtocol.list — projection forwarding (D-424)', () => {
  it('forwards an explicit lifecycle projection verbatim on the wire', async () => {
    const fetchImpl = vi.fn(async () => okResponse(EMPTY_LIST));
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });
    const sessions = new SessionsProtocol(client);
    await sessions.list({ filter: {}, limit: 50, projection: 'lifecycle' });
    const url = (fetchImpl.mock.calls[0] as unknown as [string])[0];
    expect(url).toBe('http://127.0.0.1:18080/v1/sessions/list');
    expect(sentBody(fetchImpl).projection).toBe('lifecycle');
  });

  it('forwards an explicit full projection verbatim on the wire', async () => {
    const fetchImpl = vi.fn(async () => okResponse(EMPTY_LIST));
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });
    const sessions = new SessionsProtocol(client);
    await sessions.list({ filter: {}, projection: 'full' });
    expect(sentBody(fetchImpl).projection).toBe('full');
  });

  it('an omitted projection is default-compatible — no projection key on the wire', async () => {
    const fetchImpl = vi.fn(async () => okResponse(EMPTY_LIST));
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });
    const sessions = new SessionsProtocol(client);
    await sessions.list({ filter: {} });
    // The runtime resolves an omitted projection to `full` at the method
    // edge — the Console must keep sending the pre-D-424 body so older
    // runtimes (strict decoders) and the default path both stay valid.
    expect(sentBody(fetchImpl).projection).toBeUndefined();
  });
});

describe('chatCatalogListRequest — the Playground switcher request (D-424)', () => {
  it('explicitly requests the lifecycle projection', () => {
    expect(chatCatalogListRequest().projection).toBe('lifecycle');
  });

  it('requests NO counter-dependent filter or sort (rejected invalid_request on lifecycle)', () => {
    const req = chatCatalogListRequest();
    expect(req.sort).toBeUndefined(); // started_desc runtime default — never cost_desc
    expect(req.filter).toEqual({});
    expect(req.filter?.cost_above_cents).toBeUndefined();
    expect(req.filter?.has_failed_task).toBeUndefined();
    expect(req.filter?.has_intervention).toBeUndefined();
  });

  it('is bounded to the default catalog page size', () => {
    expect(chatCatalogListRequest().limit).toBe(50);
  });

  it('round-trips through the real transport with projection:lifecycle and no counter facets', async () => {
    const fetchImpl = vi.fn(async () => okResponse(EMPTY_LIST));
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });
    const sessions = new SessionsProtocol(client);
    await sessions.list(chatCatalogListRequest());
    const body = sentBody(fetchImpl);
    expect(body.projection).toBe('lifecycle');
    expect(body.filter).toEqual({});
    expect(body.sort).toBeUndefined();
    expect(body.limit).toBe(50);
  });

  it('keeps the switcher label shape typed: rows carry session_id / title / last_activity_at', async () => {
    const fetchImpl = vi.fn(async () =>
      okResponse({
        rows: [
          {
            session_id: 's1',
            title: 'Research',
            last_activity_at: '2026-08-01T10:30:00Z',
            status: 'running',
            user_id: 'u1',
            tenant_id: 't1',
            started_at: '2026-08-01T10:00:00Z',
            duration: 0,
            tasks_count: 0,
            events_count: 0,
            total_cost_cents: 0,
            total_tokens: 0,
            has_pending_intervention: false,
            has_failed_task: false,
            identity: { tenant: 't1', user: 'u1', session: 's1' },
            counter_status: 'not_requested'
          }
        ],
        next_cursor: '',
        truncated: false
      })
    );
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });
    const sessions = new SessionsProtocol(client);
    const resp = await sessions.list(chatCatalogListRequest());
    expect(resp.rows[0].session_id).toBe('s1');
    expect(resp.rows[0].title).toBe('Research');
    // A lifecycle row carries the explicit not_requested marker — the
    // switcher reads only the catalog fields and never treats the zeros
    // as measured.
    expect(resp.rows[0].counter_status).toBe('not_requested');
  });
});
