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
  /**
   * The OAuth requirement the server ADVERTISED and Harbor DISCOVERED on
   * demand (D-297) — inert, server-supplied, UNVERIFIED data. Present only on
   * the detail read (mcp.servers.get / after a probe); omitted on list rows.
   * Mirrors types.MCPServerView.oauth_requirement.
   */
  oauth_requirement?: MCPOAuthRequirementView | null;
  /**
   * The last downstream insufficient-scope step-up observed on this
   * connection (a 403 + WWW-Authenticate error="insufficient_scope") — the
   * required-vs-granted scope gap surfaced as inert data. Present only on the
   * detail read (mcp.servers.get); omitted on list rows. Report-only: Harbor
   * never auto-escalates on it. Mirrors types.MCPServerView.last_scope_shortfall.
   */
  last_scope_shortfall?: MCPScopeShortfallView | null;
}

/**
 * Last observed downstream insufficient-scope step-up, surfaced as inert
 * Protocol data. Harbor never acts on it (no escalation, no re-consent).
 * Mirrors types.MCPScopeShortfallView.
 */
export interface MCPScopeShortfallView {
  tool_name: string;
  downstream_resource: string;
  required_scopes: string[];
  granted_scopes: string[];
  www_authenticate: string;
  origin: string;
  observed_at: string;
}

/**
 * Discovered MCP OAuth requirement, surfaced as inert Protocol data (D-297).
 * Harbor never runs the flow, holds a token, or dials a discovered endpoint —
 * it is a proposal an operator confirms. Mirrors types.MCPOAuthRequirementView.
 */
export interface MCPOAuthRequirementView {
  resource_metadata_url: string;
  authorization_servers: MCPAuthorizationServerView[];
  discovered_at: string;
  source: string;
  source_url: string;
  status: MCPDiscoveryStepStatusView[];
}

/**
 * One RFC 8414 / OIDC authorization-server metadata document, verbatim.
 * registration_endpoint is reported, never invoked. Mirrors
 * types.MCPAuthorizationServerView.
 */
export interface MCPAuthorizationServerView {
  issuer: string;
  authorization_endpoint: string;
  token_endpoint: string;
  scopes_supported: string[];
  code_challenge_methods_supported: string[];
  registration_endpoint?: string;
  resource?: string;
  source_url: string;
}

/** One typed per-hop discovery status. Mirrors types.MCPDiscoveryStepStatusView. */
export interface MCPDiscoveryStepStatusView {
  step: string;
  target: string;
  ok: boolean;
  reason?: string;
  detail?: string;
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

/* ------------------------------------------------------------------ */
/* MCP Apps host wire types                                            */
/*                                                                     */
/* Hand-synced field-for-field from internal/protocol/types/mcp_apps.go */
/* (the Go-side single source, D-002). Three methods consume these:    */
/*   - mcp.servers.read_resource — ReadMCPResourceResponse             */
/*   - mcp.apps.call_tool        — MCPAppCallToolResponse              */
/*   - mcp.apps.tool_context     — ToolContextResponse                 */
/* The request bodies fold identity at the Transport choke point, so   */
/* the TS request shapes are constructed inline by the namespace        */
/* methods and only the response + projection types are declared here. */
/* ------------------------------------------------------------------ */

/**
 * The host-side projection of an MCP App reference parsed from a tool
 * result's `_meta.ui.resourceUri` slot — mirrors `types.MCPAppRef`. Empty
 * (absent on the response) for an ordinary, non-app tool result.
 */
export interface MCPAppRef {
  /**
   * Runtime-authored effective agent configuration that produced this App.
   * The host echoes it on resource reads and App tool calls; the Runtime
   * re-authorizes signed reach before consuming any agent-owned authority.
   */
  agent_id?: string;
  /**
   * The MCP server (source id) hosting the app — paired with
   * `resource_uri` to fetch the document via `mcp.servers.read_resource`.
   * Empty for a non-app tool result.
   */
  server_id?: string;
  /**
   * The stable per-invocation id of the tool call that declared the app —
   * paired with `server_id` to fetch the tool context (input + lowered
   * result) via `mcp.apps.tool_context`. Empty for a non-app result, and
   * empty when no tool context was captured for the invocation — a
   * non-empty value promises a fetchable record, so a client may treat a
   * miss as an expired one.
   */
  tool_call_id?: string;
  /** The `ui://`-scheme URI of the app's UI document. */
  resource_uri: string;
  /**
   * The negotiated MCP-Apps display mode (one of inline / fullscreen /
   * pip), or empty when the server stated no preference. Phase 109b
   * consumes only `inline`; fullscreen + pip are 109c.
   */
  display_mode?: string;
  /**
   * The per-server raw-HTML trust flag — default-deny. The host sandboxes
   * the iframe unless an operator explicitly opted in via
   * `mcp.servers.set_raw_html_trust`.
   */
  raw_html_trusted: boolean;
  binding?: string;
}

/**
 * The by-reference stub the MCP Apps surface returns when fetched content
 * meets or exceeds the heavy-content threshold (D-026) — mirrors
 * `types.MCPResourceArtifactRef`. The Console fetches the bytes via the
 * artifacts surface against this stub.
 */
export interface MCPResourceArtifactRef {
  /** The content-addressed identifier (`{namespace}_{sha256[:12]}`). */
  id: string;
  /** The IANA media type, when known. */
  mime_type?: string;
  /** The length of the referenced bytes. */
  size_bytes?: number;
  /** Metadata only — never used for path construction. */
  filename?: string;
  /** The full hex digest of the referenced bytes. */
  sha256?: string;
}

/**
 * `mcp.servers.read_resource` reply — mirrors `types.ReadMCPResourceResponse`.
 * EXACTLY ONE of `content` / `artifact_ref` is set: `content` carries the
 * inline bytes below the heavy threshold; `artifact_ref` carries the
 * by-reference stub at or above it.
 */
export interface ReadMCPResourceResponse {
  /** Echoes the fetched resource URI. */
  resource_uri: string;
  /** The resource's declared media type. */
  mime_type?: string;
  /** Inline resource bytes — set only below the heavy threshold. */
  content?: string;
  /** By-reference stub — set only at or above the heavy threshold. */
  artifact_ref?: MCPResourceArtifactRef;
  /** The Protocol version the Runtime answered under. */
  protocol_version: string;
}

/**
 * `mcp.apps.call_tool` reply — mirrors `types.MCPAppCallToolResponse`.
 * EXACTLY ONE of `content` / `artifact_ref` is set (the same heavy-content
 * discipline as `ReadMCPResourceResponse`). `app` is non-null when the tool
 * result declared a `ui://` MCP App.
 */
export interface MCPAppCallToolResponse {
  /** Echoes the invoked tool name. */
  tool: string;
  /** Inline tool-result JSON — set only below the heavy threshold. */
  content?: unknown;
  /** By-reference stub — set only at or above the heavy threshold. */
  artifact_ref?: MCPResourceArtifactRef;
  /** Whether the MCP server returned a tool error (the result's IsError). */
  is_error: boolean;
  /** The MCP App reference parsed from `_meta.ui.resourceUri`, or absent. */
  app?: MCPAppRef;
  /** The Protocol version the Runtime answered under. */
  protocol_version: string;
}

/**
 * `mcp.apps.tool_context` request — a rendered MCP App asking the host for
 * the tool context (input + lowered result) that produced it. Mirrors
 * `types.ToolContextRequest`. Identity is threaded by the typed client from
 * the verified session; an unknown or cross-identity (server_id,
 * tool_call_id) is not found.
 */
export interface ToolContextRequest {
  /** The MCP server (source id) carried on the `mcp.app_available` event. */
  server_id: string;
  /** The stable per-invocation id carried on the `mcp.app_available` event. */
  tool_call_id: string;
}

/**
 * One half (input or result) of a captured tool context — mirrors
 * `types.ToolContextPayload`. EXACTLY ONE of `content` / `artifact_ref` is
 * set: `content` carries inline JSON below the heavy threshold;
 * `artifact_ref` carries the by-reference stub at or above it.
 */
export interface ToolContextPayload {
  /** Inline JSON — set only below the heavy threshold. */
  content?: unknown;
  /** By-reference stub — set only at or above the heavy threshold. */
  artifact_ref?: MCPResourceArtifactRef;
}

/**
 * `mcp.apps.tool_context` reply — the captured input + lowered result that
 * produced a rendered app. Mirrors `types.ToolContextResponse`. Each of
 * `input` / `result` is inline-or-by-reference (a large result rides by
 * reference). An unknown or cross-identity id fails with `not_found`.
 */
export interface ToolContextResponse {
  /** The server-side tool name that declared the app. */
  tool: string;
  /** The tool's input arguments (inline JSON or by reference). */
  input: ToolContextPayload;
  /** The tool's lowered result (inline JSON or by reference). */
  result: ToolContextPayload;
  /** Whether the tool returned a server-side error result. */
  is_error: boolean;
  /** The Protocol version the Runtime answered under. */
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
