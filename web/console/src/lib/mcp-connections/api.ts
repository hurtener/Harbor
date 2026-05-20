// Phase 73k (D-119) — typed Protocol client for the MCP Connections page.
//
// # Deviation note (CLAUDE.md §4.5 #5 / D-093)
//
// D-093 specifies a generated `src/lib/protocol.ts` produced by
// `cmd/harbor-gen-protocol-ts`. At the time Phase 73k lands, that
// generator does not yet exist — `protocol.ts` is the Phase 72h
// empty-but-typed stub. This module is the hand-authored stand-in:
// every type below mirrors the Go wire shape in
// `internal/protocol/types/mcp_servers.go` verbatim, and the calls go
// through the single `protocolCall` choke point. When the generator
// lands, these types regenerate into `protocol.ts` and this file
// becomes a thin re-export. The §13 rule the page satisfies today is
// "no hand-rolled `fetch` in `.svelte` files" — every Protocol call is
// funnelled through THIS `.ts` module, never inlined in a component.

import { PROTOCOL_TS_GENERATED, type OperatorIdentity } from '$lib/protocol';

// Touch the generated stub so a future generator swap is a compile-time
// signal rather than a silent divergence.
void PROTOCOL_TS_GENERATED;

/** The flat identity scope every MCP Protocol request carries. */
export interface IdentityScope {
  tenant: string;
  user: string;
  session: string;
}

/** The canonical MCP server state chip. */
export type MCPServerState =
  | 'online'
  | 'reconnecting'
  | 'offline'
  | 'auth_pending'
  | 'error';

/** One MCP server row — mirrors types.MCPServerView. */
export interface MCPServerView {
  name: string;
  transport: string;
  url_or_command: string;
  state: MCPServerState;
  last_discovery_at: string;
  tool_count: number;
  resource_count: number;
  prompt_count: number;
  recent_latency_ms: number;
  error_rate_per_min: number;
  oauth_binding_count: number;
  raw_html_trusted: boolean;
}

/** mcp.servers.list response — mirrors types.MCPServersListResponse. */
export interface MCPServersListResponse {
  servers: MCPServerView[];
  next_page_token?: string;
  protocol_version: string;
}

/** Read-only ToolPolicy projection — mirrors types.MCPToolPolicyView. */
export interface MCPToolPolicyView {
  timeout_ms: number;
  max_retries: number;
  concurrency_cap: number;
}

/** Per-scope binding count — mirrors types.MCPBindingScopeCount. */
export interface MCPBindingScopeCount {
  binding_scope: string;
  count: number;
}

/** mcp.servers.get response — mirrors types.MCPServerGetResponse. */
export interface MCPServerGetResponse {
  server: MCPServerView;
  display_modes_advertised: string[];
  content_shapes: string[];
  tool_policy: MCPToolPolicyView;
  bindings_summary: MCPBindingScopeCount[];
  protocol_version: string;
}

/** One advertised resource — mirrors types.MCPResourceView. */
export interface MCPResourceView {
  uri: string;
  mime_type?: string;
  size_bytes?: number;
  name?: string;
  title?: string;
}

/** mcp.servers.resources response. */
export interface MCPServerResourcesResponse {
  resources: MCPResourceView[];
  protocol_version: string;
}

/** One declared prompt argument. */
export interface MCPPromptArg {
  name: string;
  description?: string;
  required: boolean;
}

/** One advertised prompt — mirrors types.MCPPromptView. */
export interface MCPPromptView {
  name: string;
  description?: string;
  arguments: MCPPromptArg[];
}

/** mcp.servers.prompts response. */
export interface MCPServerPromptsResponse {
  prompts: MCPPromptView[];
  protocol_version: string;
}

/** mcp.servers.health response — mirrors types.MCPServerHealthResponse. */
export interface MCPServerHealthResponse {
  handshake_latency_buckets: { start_ms: number; latency_ms: number }[];
  reconnect_history: { occurred_at: string; reason?: string }[];
  transport_error_rate: number;
  protocol_version: string;
}

/** One OAuth binding row — mirrors types.MCPBindingView (no token plaintext). */
export interface MCPBindingView {
  principal_id: string;
  binding_scope: string;
  scopes: string[];
  expires_at: string;
  last_used_at: string;
}

/** mcp.servers.bindings.list response. */
export interface MCPServerBindingsListResponse {
  bindings: MCPBindingView[];
  protocol_version: string;
}

/** mcp.servers.policy response. */
export interface MCPServerPolicyResponse {
  tool_policy: MCPToolPolicyView;
  protocol_version: string;
}

/** mcp.servers.refresh_discovery response. */
export interface MCPServerRefreshDiscoveryResponse {
  discovery_id: string;
  tool_count: number;
  resource_count: number;
  prompt_count: number;
  protocol_version: string;
}

/** mcp.servers.probe response. */
export interface MCPServerProbeResponse {
  ok: boolean;
  latency_ms: number;
  error?: string;
  protocol_version: string;
}

/** mcp.servers.refresh_binding response — the AuthorizeURL + flow State. */
export interface MCPServerRefreshBindingResponse {
  authorize_url: string;
  state: string;
  protocol_version: string;
}

/** mcp.servers.revoke_binding response. */
export interface MCPServerRevokeBindingResponse {
  revoked: boolean;
  protocol_version: string;
}

/** mcp.servers.set_raw_html_trust response. */
export interface MCPServerSetRawHTMLTrustResponse {
  name: string;
  trusted: boolean;
  protocol_version: string;
}

/** A Protocol error body — mirrors errors.Error. */
export interface ProtocolError {
  code: string;
  message: string;
}

/** ProtocolCallError carries the canonical Protocol error code so the
 * page can branch on `scope_mismatch` / `not_found` / `identity_required`. */
export class ProtocolCallError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = 'ProtocolCallError';
    this.code = code;
  }
}

/** The MCP Connections runtime base URL. Defaults to the dev runtime. */
function runtimeBaseURL(): string {
  return import.meta.env.VITE_HARBOR_RUNTIME_URL ?? 'http://127.0.0.1:18080';
}

/**
 * protocolCall is the single Protocol-call choke point for the MCP
 * Connections page. Every `mcp.servers.*` method routes through here —
 * components never call `fetch` directly (CLAUDE.md §13).
 */
async function protocolCall<T>(method: string, body: Record<string, unknown>): Promise<T> {
  const res = await fetch(`${runtimeBaseURL()}/v1/control/${method}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  });
  const text = await res.text();
  const parsed: unknown = text.length > 0 ? JSON.parse(text) : {};
  if (!res.ok) {
    const err = parsed as Partial<ProtocolError>;
    throw new ProtocolCallError(err.code ?? 'runtime_error', err.message ?? res.statusText);
  }
  return parsed as T;
}

/** Builds the identity scope from the operator identity. The Console DB
 * `operator_id` keys the local saved-view rows; the Protocol identity is
 * the same operator's (tenant, user) plus a Console session id. */
function scopeFrom(op: OperatorIdentity, session: string): IdentityScope {
  return { tenant: op.tenantID, user: op.userID, session };
}

/** Filter parameters for mcp.servers.list. */
export interface MCPListFilter {
  state?: MCPServerState[];
  transport?: string[];
  has_oauth?: boolean;
  has_recent_error?: boolean;
  name_prefix?: string;
  page_token?: string;
  page_size?: number;
}

/** The typed MCP Connections Protocol client. */
export const mcpApi = {
  list(op: OperatorIdentity, session: string, filter: MCPListFilter = {}) {
    return protocolCall<MCPServersListResponse>('mcp.servers.list', {
      identity: scopeFrom(op, session),
      ...filter
    });
  },
  get(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerGetResponse>('mcp.servers.get', {
      identity: scopeFrom(op, session),
      name
    });
  },
  resources(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerResourcesResponse>('mcp.servers.resources', {
      identity: scopeFrom(op, session),
      name
    });
  },
  prompts(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerPromptsResponse>('mcp.servers.prompts', {
      identity: scopeFrom(op, session),
      name
    });
  },
  health(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerHealthResponse>('mcp.servers.health', {
      identity: scopeFrom(op, session),
      name
    });
  },
  policy(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerPolicyResponse>('mcp.servers.policy', {
      identity: scopeFrom(op, session),
      name
    });
  },
  bindings(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerBindingsListResponse>('mcp.servers.bindings.list', {
      identity: scopeFrom(op, session),
      name
    });
  },
  refreshDiscovery(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerRefreshDiscoveryResponse>('mcp.servers.refresh_discovery', {
      identity: scopeFrom(op, session),
      name
    });
  },
  probe(op: OperatorIdentity, session: string, name: string) {
    return protocolCall<MCPServerProbeResponse>('mcp.servers.probe', {
      identity: scopeFrom(op, session),
      name
    });
  },
  refreshBinding(op: OperatorIdentity, session: string, name: string, principalId: string) {
    return protocolCall<MCPServerRefreshBindingResponse>('mcp.servers.refresh_binding', {
      identity: scopeFrom(op, session),
      name,
      principal_id: principalId
    });
  },
  revokeBinding(op: OperatorIdentity, session: string, name: string, principalId: string) {
    return protocolCall<MCPServerRevokeBindingResponse>('mcp.servers.revoke_binding', {
      identity: scopeFrom(op, session),
      name,
      principal_id: principalId
    });
  },
  setRawHTMLTrust(op: OperatorIdentity, session: string, name: string, trusted: boolean) {
    return protocolCall<MCPServerSetRawHTMLTrustResponse>('mcp.servers.set_raw_html_trust', {
      identity: scopeFrom(op, session),
      name,
      trusted
    });
  }
};
