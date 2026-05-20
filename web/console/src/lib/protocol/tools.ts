/**
 * Tools-page Protocol wire types (Phase 73f / D-116; refactored onto
 * D-121).
 *
 * # Wire types only — the client is the unified `HarborClient`
 *
 * The D-121 refactor folded the legacy page-local `ToolsClient` onto the
 * unified `HarborClient`'s `tools` namespace (`$lib/protocol/client.ts`).
 * This module is now the typed wire-shape surface ONLY: the request /
 * response shapes the Tools page narrows `client.tools.*` results into.
 * There is no `fetch` here and no page-local client class.
 *
 * The wire shapes mirror `internal/protocol/types/tools.go` field-for-
 * field (the Go-side single source per D-002). When the
 * `cmd/harbor-gen-protocol-ts` generator (D-093) ships, these types fold
 * into the generated `protocol.ts` and this module re-exports from there
 * — a mechanical migration.
 */

/** Wire enum — a tool's transport. */
export type ToolTransport = 'in-proc' | 'HTTP' | 'MCP' | 'A2A' | 'flow';

/** Wire enum — a tool's OAuth binding status. */
export type ToolOAuthStatus = 'Bound' | 'Required' | 'Expired' | 'n/a';

/** Wire enum — a tool's approval gate. */
export type ToolApprovalPolicy = 'auto' | 'gated' | 'denied';

/** Wire enum — a tool's health pill. */
export type ToolStatus = 'Healthy' | 'Degraded' | 'Offline';

/** Wire enum — the `tools.metrics` observation window. */
export type ToolMetricsWindow = '1h' | '24h' | '7d';

/** The catalog-row projection of a registered tool. */
export interface Tool {
  id: string;
  name: string;
  version: string;
  description: string;
  scope: string;
  transport: ToolTransport;
  oauth_status: ToolOAuthStatus;
  approval_policy: ToolApprovalPolicy;
  reliability_tier: string;
  owner: string;
  last_used_at: string;
}

/** Server-enforced facet filter on `tools.list`. */
export interface ToolFilter {
  scopes?: string[];
  transports?: ToolTransport[];
  oauth_statuses?: ToolOAuthStatus[];
  approval_policies?: ToolApprovalPolicy[];
  reliability_tiers?: string[];
  search?: string;
}

/** The four catalog counters over the filtered view. */
export interface ToolAggregates {
  total: number;
  active: number;
  pending_approval: number;
  awaiting_oauth: number;
}

/** The `tools.list` reply. */
export interface ToolListResponse {
  tools: Tool[];
  page: number;
  page_size: number;
  page_count: number;
  total_rows: number;
  aggregates: ToolAggregates;
}

/** The full descriptor projection `tools.describe` returns. */
export interface ToolManifest {
  tool: Tool;
  side_effect: string;
  args_schema: string;
  out_schema: string;
  examples: string[];
  auth_scopes: string[];
  oauth_binding_scope: string;
  retry_attempts: number;
  timeout_ms: number;
  loading_mode: string;
  display_modes: Record<string, string>;
}

/** The `tools.metrics` reply. */
export interface ToolMetrics {
  id: string;
  window: ToolMetricsWindow;
  error_rate_1h: number;
  error_rate_24h: number;
  error_rate_7d: number;
  invocations: number;
  failures: number;
  status: ToolStatus;
}

/** One power-of-two size bucket in the result-size histogram. */
export interface ToolContentBucket {
  max_bytes: number;
  count: number;
}

/** The `tools.content_stats` reply. */
export interface ToolContentStats {
  id: string;
  histogram: ToolContentBucket[];
  heavy_threshold_bytes: number;
  heavy_count: number;
  negotiated_display: Record<string, string>;
}

/** The `tools.set_approval_policy` reply. */
export interface ToolSetApprovalPolicyResponse {
  id: string;
  policy: ToolApprovalPolicy;
}

/** The `tools.revoke_oauth` reply. */
export interface ToolRevokeOAuthResponse {
  id: string;
  revoked_count: number;
}
