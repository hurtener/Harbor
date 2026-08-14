// Phase 109b — the Console adapter that maps HarborClient onto the chat
// module's injected MCPAppHostClient. Proves each method routes to exactly one
// Protocol call and maps the wire shape correctly — the real production
// consumer of the new `mcp.servers.read_resource` / `mcp.apps.call_tool`
// client methods.

import { afterEach, describe, expect, it, vi } from 'vitest';

import {
  MCPAppRenderAdmissionError,
  MCPAppToolNotFoundError,
} from './chat/renderers/app-bridge-host.js';
import { makeMCPAppHostClient } from './mcp-app-host-client.js';
import type { ProtocolClient } from './protocol/client.js';
import { ProtocolError } from './protocol/errors.js';

function fakeProtocolClient(): { client: ProtocolClient; readResource: ReturnType<typeof vi.fn>; callTool: ReturnType<typeof vi.fn>; resources: ReturnType<typeof vi.fn>; toolsList: ReturnType<typeof vi.fn>; get: ReturnType<typeof vi.fn>; getRef: ReturnType<typeof vi.fn>; toolContext: ReturnType<typeof vi.fn> } {
  // The 4th argument is the HA-56 opt-in flag; recorded so tests can assert
  // the ordinary read never mints while the renderer-internal read does.
  const readResource = vi.fn(async (serverID: string, resourceURI: string, _agentID?: string, _requestRenderAdmission?: boolean) => ({
    resource_uri: resourceURI,
    mime_type: 'text/html',
    content: '<p>app</p>',
    protocol_version: '1',
  }));
  const callTool = vi.fn(async () => ({
    tool: 'srv_echo',
    content: { ok: true },
    is_error: false,
    protocol_version: '1',
  }));
  const resources = vi.fn(async () => ({
    resources: [{ uri: 'ui://srv/app.html', name: 'app', mime_type: 'text/html' }],
    protocol_version: '1',
  }));
  const toolsList = vi.fn(async () => ({
    tools: [
      { name: 'srv_echo', description: 'echo' },
      { name: 'other_tool', description: 'no' },
    ],
  }));
  const getRef = vi.fn(async () => ({
    ref: { id: 'art_studio_abc', size_bytes: 88_500 },
    presigned_url: 'https://artifacts.example/art_studio_abc',
    expires_at: '2026-06-13T00:00:00Z',
    protocol_version: '1',
  }));
  // `artifacts.get` — the driver-independent byte read. `content` is base64
  // (Go `[]byte` JSON encoding), and the response is truthful about its bound.
  const get = vi.fn(async () => ({
    ref: { id: 'art_studio_abc', size_bytes: 14 },
    content: btoa('{"revenue":42}'),
    offset: 0,
    returned_bytes: 14,
    total_size_bytes: 14,
    truncated: false,
    protocol_version: '1',
  }));
  const toolContext = vi.fn(async () => ({
    tool: 'srv_report',
    input: { content: { region: 'emea' } },
    result: { content: { revenue: 42 } },
    is_error: false,
    protocol_version: '1',
  }));
  const client = {
    mcp: { servers: { readResource, resources }, apps: { callTool, toolContext } },
    tools: { list: toolsList },
    artifacts: { get, getRef },
  } as unknown as ProtocolClient;
  return { client, readResource, callTool, resources, toolsList, get, getRef, toolContext };
}

describe('makeMCPAppHostClient', () => {
  it('readResource routes to mcp.servers.read_resource WITHOUT the opt-in flag (ordinary, non-minting)', async () => {
    const { client, readResource } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const res = await host.readResource('srv', 'ui://srv/app.html', 'agent-weather');
    expect(readResource).toHaveBeenCalledWith('srv', 'ui://srv/app.html', 'agent-weather');
    // The ordinary read never asks the Runtime to mint callback authority —
    // the AppBridge's app-initiated `resources/read` handler routes here.
    const flag = readResource.mock.calls[0][3];
    expect(flag).toBeUndefined();
    expect(res.content).toBe('<p>app</p>');
    expect(res.mimeType).toBe('text/html');
  });

  it('readRenderDocument routes to mcp.servers.read_resource WITH the opt-in flag and maps the admission', async () => {
    const { client, readResource } = fakeProtocolClient();
    readResource.mockResolvedValueOnce({
      resource_uri: 'ui://srv/app.html',
      mime_type: 'text/html',
      content: '<p>app</p>',
      render_admission: {
        token: 'opaque-sealed-render-token-1',
        issued_at: '2026-08-14T00:00:00Z',
        expires_at: '2026-08-14T01:00:00Z',
        availability: 'available',
      },
      protocol_version: '1',
    });
    const host = makeMCPAppHostClient(client);
    const res = await host.readRenderDocument('srv', 'ui://srv/app.html', 'agent-weather');

    // The DISTINCT renderer-internal read carries `request_render_admission: true`.
    expect(readResource).toHaveBeenCalledWith('srv', 'ui://srv/app.html', 'agent-weather', true);
    expect(res.content).toBe('<p>app</p>');
    expect(res.renderAdmission).toEqual({
      token: 'opaque-sealed-render-token-1',
      issuedAt: '2026-08-14T00:00:00Z',
      expiresAt: '2026-08-14T01:00:00Z',
      availability: 'available',
    });
  });

  it('readRenderDocument maps a typed UNAVAILABLE admission (no token) without fabricating one', async () => {
    const { client, readResource } = fakeProtocolClient();
    readResource.mockResolvedValueOnce({
      resource_uri: 'ui://srv/app.html',
      mime_type: 'text/html',
      content: '<p>app</p>',
      render_admission: { token: '', issued_at: '', expires_at: '', availability: 'unavailable' },
      protocol_version: '1',
    });
    const host = makeMCPAppHostClient(client);
    const res = await host.readRenderDocument('srv', 'ui://srv/app.html');
    expect(res.renderAdmission?.availability).toBe('unavailable');
    expect(res.renderAdmission?.token).toBe('');
  });

  it('readRenderDocument maps an absent admission object to undefined (the old surface)', async () => {
    const { client, readResource } = fakeProtocolClient();
    // The read succeeded but carried no `render_admission` field — the
    // runtime answered the old (non-admission) surface.
    readResource.mockResolvedValueOnce({
      resource_uri: 'ui://srv/app.html',
      mime_type: 'text/html',
      content: '<p>app</p>',
      protocol_version: '1',
    });
    const host = makeMCPAppHostClient(client);
    const res = await host.readRenderDocument('srv', 'ui://srv/app.html');
    expect(res.renderAdmission).toBeUndefined();
  });

  it('callTool forwards host binding and resource authority, not sandbox-authored values', async () => {
    const { client, callTool } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const hostBinding = 'opaque-host-binding';
    const hostResourceURI = 'ui://app/main.html';
    const sandboxArgs = { q: 1, binding: 'forged', resource_uri: 'ui://forged.html' };
    const res = await host.callTool('srv', 'srv_echo', sandboxArgs, 'agent-weather', hostBinding, hostResourceURI);
    expect(callTool).toHaveBeenCalledWith('srv', 'srv_echo', sandboxArgs, 'agent-weather', hostBinding, hostResourceURI, undefined);
    expect(res.isError).toBe(false);
    expect(res.content).toEqual({ ok: true });
  });

  it('callTool forwards the FRESH render admission as the DISTINCT 7th argument', async () => {
    // HA-56: the admission is a separate authority from the legacy binding —
    // it rides the `render_admission` wire field, and the adapter forwards it
    // verbatim so the Runtime can re-verify it against the CURRENT tuple.
    const { client, callTool } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const freshAdmission = 'opaque-sealed-render-token-1';
    const res = await host.callTool('srv', 'srv_echo', { q: 1 }, 'agent-weather', undefined, 'ui://app/main.html', freshAdmission);
    expect(callTool).toHaveBeenCalledWith('srv', 'srv_echo', { q: 1 }, 'agent-weather', undefined, 'ui://app/main.html', freshAdmission);
    expect(res.content).toEqual({ ok: true });
  });

  it('callTool maps a typed render-admission refusal onto MCPAppRenderAdmissionError', async () => {
    // The verdict is a distinct, actionable fact the host branches on (the
    // single bounded refresh vs explicit failure) — it must never collapse
    // into the not-found shape or an undifferentiated runtime failure.
    for (const code of [
      'render_admission_expired',
      'render_admission_unavailable',
      'render_admission_invalid',
      'render_admission_mismatch',
      'render_admission_missing',
      'render_authority_ambiguous',
    ]) {
      const { client, callTool } = fakeProtocolClient();
      callTool.mockRejectedValueOnce(new ProtocolError(code, `${code}: nope`, 400));
      const host = makeMCPAppHostClient(client);
      const err = await host
        .callTool('srv', 'srv_echo', {}, undefined, undefined, 'ui://app/main.html', 'tok')
        .catch((e: unknown) => e);
      expect(err).toBeInstanceOf(MCPAppRenderAdmissionError);
      expect((err as MCPAppRenderAdmissionError).code).toBe(code);
      expect(err).not.toBeInstanceOf(MCPAppToolNotFoundError);
    }
  });

  it('listResources routes to mcp.servers.resources', async () => {
    const { client, resources } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const rows = await host.listResources('srv', 'agent-weather');
    expect(resources).toHaveBeenCalledWith('srv', 'agent-weather');
    expect(rows[0].uri).toBe('ui://srv/app.html');
  });

  it('listTools narrows the catalog to the server prefix', async () => {
    const { client } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const rows = await host.listTools('srv');
    expect(rows.map((t) => t.name)).toEqual(['srv_echo']);
  });

  it('resolveArtifact routes to artifacts.get_ref and returns the presigned_url (not the absent `url`)', async () => {
    const { client, getRef } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const url = await host.resolveArtifact('art_studio_abc');
    expect(getRef).toHaveBeenCalledWith({ id: 'art_studio_abc' });
    // The Go wire field is `presigned_url` — a `.url` read would be undefined.
    expect(url).toBe('https://artifacts.example/art_studio_abc');
  });

  it('toolContext routes to mcp.apps.tool_context and maps the input/result payloads', async () => {
    const { client, toolContext } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    const ctx = await host.toolContext('srv', 'tc_1');
    expect(toolContext).toHaveBeenCalledWith('srv', 'tc_1');
    expect(ctx).toEqual({
      tool: 'srv_report',
      input: { content: { region: 'emea' }, artifactRef: undefined },
      result: { content: { revenue: 42 }, artifactRef: undefined },
      isError: false,
    });
  });

  it('toolContext maps a heavy by-reference payload to an artifactRef stub', async () => {
    const { client, toolContext } = fakeProtocolClient();
    toolContext.mockResolvedValueOnce({
      tool: 'srv_report',
      input: { content: { region: 'emea' } },
      result: { artifact_ref: { id: 'art_big', mime_type: 'application/json', size_bytes: 90_000 } },
      is_error: false,
      protocol_version: '1',
    });
    const host = makeMCPAppHostClient(client);
    const ctx = await host.toolContext('srv', 'tc_1');
    expect(ctx?.result.artifactRef).toEqual({
      id: 'art_big',
      mimeType: 'application/json',
      sizeBytes: 90_000,
    });
    expect(ctx?.result.content).toBeUndefined();
  });

  // `null` is the MISS signal, not a "degraded but carry on" one: the renderer
  // resolves the context BEFORE mounting the iframe, so a null means the app is
  // not rendered at all and the turn shows the honest "this view is no longer
  // available" placeholder instead (D-348).
  it('toolContext maps a Runtime not_found to null (the miss — the app is not rendered)', async () => {
    const { client, toolContext } = fakeProtocolClient();
    toolContext.mockRejectedValueOnce(new ProtocolError('not_found', 'no context', 404));
    const host = makeMCPAppHostClient(client);
    expect(await host.toolContext('srv', 'gone')).toBeNull();
  });

  it('toolContext re-throws a non-not_found Protocol error (fail loud)', async () => {
    const { client, toolContext } = fakeProtocolClient();
    toolContext.mockRejectedValueOnce(new ProtocolError('identity_scope_required', 'nope', 401));
    const host = makeMCPAppHostClient(client);
    await expect(host.toolContext('srv', 'x')).rejects.toThrow(/nope/);
  });

  it('callTool maps a Runtime not_found onto the TYPED MCPAppToolNotFoundError', async () => {
    // The caller has already qualified the name into the app's own server
    // namespace, so `not_found` means "no such tool on YOUR server" — the one
    // outcome an app can act on. Before this, the Runtime answered an
    // unresolvable tool with a generic `runtime_error` reading "MCP read
    // failed", indistinguishable from a broken southbound transport.
    const { client, callTool } = fakeProtocolClient();
    callTool.mockRejectedValueOnce(new ProtocolError('not_found', 'tools: tool not found', 404));
    const host = makeMCPAppHostClient(client);
    const err = await host.callTool('srv', 'srv_nope').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(MCPAppToolNotFoundError);
    expect((err as MCPAppToolNotFoundError).tool).toBe('srv_nope');
  });

  it('callTool re-throws a non-not_found Protocol error unchanged (fail loud)', async () => {
    const { client, callTool } = fakeProtocolClient();
    callTool.mockRejectedValueOnce(new ProtocolError('scope_mismatch', 'server paused', 403));
    const host = makeMCPAppHostClient(client);
    const err = await host.callTool('srv', 'srv_echo').catch((e: unknown) => e);
    expect(err).not.toBeInstanceOf(MCPAppToolNotFoundError);
    expect((err as Error).message).toContain('server paused');
  });

  it('listResourceTemplates answers the advertised capability with the honest empty list', async () => {
    // Harbor's Protocol exposes no resource-template method. The host still
    // advertises `serverResources`, so the probe must ANSWER rather than error.
    const { client } = fakeProtocolClient();
    const host = makeMCPAppHostClient(client);
    await expect(host.listResourceTemplates('srv')).resolves.toEqual([]);
  });

  it('fetchArtifactText reads the bytes through artifacts.get, issuing NO raw network call', async () => {
    // It used to resolve `artifacts.get_ref` and `fetch` the presigned URL —
    // a route only the s3 driver serves, and not the `inmem` default. The byte
    // read resolves through the mandatory store `Get` (D-353), so it answers on
    // every driver, and the adapter no longer issues a raw `fetch` at all.
    // (The full transcript-driven guard lives in the byte-path spec.)
    const { client, get, getRef } = fakeProtocolClient();
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const host = makeMCPAppHostClient(client);
    const text = await host.fetchArtifactText('art_studio_abc');
    expect(get).toHaveBeenCalledWith(
      expect.objectContaining({ id: 'art_studio_abc', offset: 0 }),
    );
    expect(getRef).not.toHaveBeenCalled();
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(text).toBe('{"revenue":42}');
  });

  it('fetchArtifactText propagates a read failure (fail loud, never silently empty)', async () => {
    const { client, get } = fakeProtocolClient();
    get.mockRejectedValueOnce(new ProtocolError('runtime_error', 'artifact store failed', 500));
    const host = makeMCPAppHostClient(client);
    await expect(host.fetchArtifactText('art_studio_abc')).rejects.toThrow(/artifact store failed/);
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});
