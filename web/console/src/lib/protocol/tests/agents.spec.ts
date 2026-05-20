/**
 * Agents-page Protocol surface tests (Phase 73e / D-124).
 *
 * Exercises the `HarborClient.agents` namespace + the `AgentsProtocol`
 * typed view with an injected `fetchImpl` stub — verified without a live
 * Runtime. Asserts: the eight `agents.*` methods target the
 * `POST /v1/agents/{verb}` routes, fold the identity triple into the
 * body, attach the bearer + `X-Harbor-*` headers, and that
 * `AgentsProtocol` narrows the generic namespace results to the typed
 * Agents-page wire shapes (CONVENTIONS.md §6).
 */
import { describe, expect, it, vi } from 'vitest';
import { HarborClient } from '../harbor.js';
import { AgentsProtocol, type AgentListResponse } from '../agents.js';
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

describe('HarborClient.agents namespace', () => {
  it('agents.list targets POST /v1/agents/list with the identity body', async () => {
    const fetchImpl = vi.fn(async () =>
      okResponse({ agents: [], page: 1, page_size: 50, page_count: 1, total_rows: 0 })
    );
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });

    await client.agents.list({ page: 1 });

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [
      string,
      RequestInit
    ];
    expect(url).toBe('http://127.0.0.1:18080/v1/agents/list');
    expect(init.method).toBe('POST');
    const headers = init.headers as Record<string, string>;
    expect(headers.Authorization).toBe('Bearer dummy-bearer-token');
    expect(headers['X-Harbor-Tenant']).toBe('t1');
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    expect(body.identity).toEqual({ tenant: 't1', user: 'u1', session: 's1' });
    expect(body.page).toBe(1);
  });

  it('agents.get / tools / memory / governance / skills / permissions target their routes', async () => {
    const fetchImpl = vi.fn(async () => okResponse({}));
    const client = new HarborClient({ connection: CONNECTION, fetchImpl });

    await client.agents.get('agent-1');
    await client.agents.tools('agent-1');
    await client.agents.memory('agent-1');
    await client.agents.governance('agent-1');
    await client.agents.skills('agent-1');
    await client.agents.permissions('agent-1');
    await client.agents.metrics();

    const routes = (fetchImpl.mock.calls as unknown as [string, RequestInit][]).map(
      (c) => c[0]
    );
    expect(routes).toEqual([
      'http://127.0.0.1:18080/v1/agents/get',
      'http://127.0.0.1:18080/v1/agents/tools',
      'http://127.0.0.1:18080/v1/agents/memory',
      'http://127.0.0.1:18080/v1/agents/governance',
      'http://127.0.0.1:18080/v1/agents/skills',
      'http://127.0.0.1:18080/v1/agents/permissions',
      'http://127.0.0.1:18080/v1/agents/metrics'
    ]);
    // The detail methods fold the agent id into the body.
    const firstCall = fetchImpl.mock.calls[0] as unknown as [
      string,
      RequestInit
    ];
    const getBody = JSON.parse(firstCall[1].body as string) as Record<
      string,
      unknown
    >;
    expect(getBody.id).toBe('agent-1');
  });
});

describe('AgentsProtocol typed view', () => {
  it('narrows agents.list onto the typed AgentListResponse', async () => {
    const wire: AgentListResponse = {
      agents: [
        {
          id: 'a1',
          name: 'Support Bot',
          description: '',
          incarnation: 1,
          version_hash: 'vh1',
          owner: 'support',
          status: 'active',
          health: 'Healthy',
          hosting: 'local',
          planner_type: 'react',
          model: 'gpt',
          tools_count: 2,
          mcp_count: 1,
          registered_at: '2026-05-20T12:00:00Z',
          updated_at: '2026-05-20T12:00:00Z'
        }
      ],
      page: 1,
      page_size: 50,
      page_count: 1,
      total_rows: 1,
      aggregates: { total: 1, active: 1, paused: 0, drained: 0 }
    };
    const fetchImpl = vi.fn(async () => okResponse(wire));
    const api = new AgentsProtocol(new HarborClient({ connection: CONNECTION, fetchImpl }));

    const resp = await api.list({ filter: { status: ['active'] } });
    expect(resp.total_rows).toBe(1);
    expect(resp.agents[0].id).toBe('a1');
    expect(resp.aggregates.active).toBe(1);
  });

  it('metrics() narrows onto the typed rollup', async () => {
    const fetchImpl = vi.fn(async () =>
      okResponse({
        metrics: {
          active_agents: 4,
          running_tasks: 2,
          total_cost_usd: 12.5,
          total_tokens: 9000
        }
      })
    );
    const api = new AgentsProtocol(new HarborClient({ connection: CONNECTION, fetchImpl }));

    const resp = await api.metrics();
    expect(resp.metrics.active_agents).toBe(4);
    expect(resp.metrics.total_cost_usd).toBe(12.5);
  });
});
