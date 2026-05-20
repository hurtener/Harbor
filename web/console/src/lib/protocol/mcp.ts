/**
 * MCP Connections wire types — the `mcp.servers.*` Protocol shapes
 * (D-121, MCP refactor).
 *
 * # Wire types only — the client lives in `harbor.ts`
 *
 * The legacy MCP page shipped a bespoke `mcpApi` object literal with its
 * own `fetch` choke point and its own `ProtocolCallError` class — which
 * silently DROPPED the HTTP status. The D-121 refactor deletes that:
 * every `mcp.servers.*` call now routes through the unified
 * `HarborClient` (`$lib/protocol`), which raises the single
 * `ProtocolError` carrying `(code, message, status)`.
 *
 * This module is the wire-type surface only — the request/response
 * shapes the `MCPServersNamespace` methods consume and return. They
 * mirror `internal/protocol/types/mcp_servers.go` field-for-field (the
 * Go-side single source per D-002). When `cmd/harbor-gen-protocol-ts`
 * (D-093) ships, these absorb into the generated `protocol.ts`.
 */

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
  total?: number;
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

/**
 * Filter parameters for `mcp.servers.list`. The MCP-page `SavedViewChips`
 * persist a JSON-encoded value of this shape in the Console DB
 * `saved_filters` table (see `$lib/db/saved_filters_mcp.ts`).
 */
export interface MCPListFilter {
  state?: MCPServerState[];
  transport?: string[];
  has_oauth?: boolean;
  has_recent_error?: boolean;
  name_prefix?: string;
  page_token?: string;
  page?: number;
  page_size?: number;
}
