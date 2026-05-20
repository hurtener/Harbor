// Harbor Console — Flows-page wire types (Phase 73i / D-117).
//
// These TypeScript shapes mirror the canonical Go-side Protocol wire
// types in `internal/protocol/types/flows.go`. When the
// `cmd/harbor-gen-protocol-ts` generator (D-093) is extended to emit the
// flows.* cluster, these move into the generated `src/lib/protocol.ts`
// verbatim. Until then they live here as the hand-authored mirror — NOT
// in `protocol.ts` (which carries the `DO NOT EDIT` generated header).
//
// The Console talks to the Runtime ONLY through the typed client in
// `client.ts`; no `.svelte` component issues a raw `fetch`.

/** The Harbor isolation triple carried by every Flows-page request. */
export interface IdentityScope {
  tenant: string;
  user: string;
  session: string;
}

/** A flow's per-flow Budget (D-023). Read-only at V1. */
export interface FlowBudget {
  deadline_ms?: number;
  request_cap?: number;
  cost_cap_usd?: number;
  token_cap?: number;
}

/** Live consumption of a flow's Budget over the active window. */
export interface FlowBudgetConsumption {
  requests_used: number;
  cost_usd_used: number;
  tokens_used: number;
}

/** One row of the `flows.list` catalog. */
export interface Flow {
  id: string;
  name: string;
  owner?: string;
  version?: string;
  planner_family?: string;
  node_count: number;
  edge_count: number;
  runs_24h: number;
  p50_latency_ms: number;
  p95_latency_ms: number;
  success_rate: number;
  last_run?: string;
  budget: FlowBudget;
}

/** Catalog filter for `flows.list`. */
export interface FlowFilter {
  tenants?: string[];
  planner_families?: string[];
  query?: string;
}

export interface FlowListRequest {
  identity: IdentityScope;
  filter: FlowFilter;
  page?: number;
  page_size?: number;
}

export interface FlowListResponse {
  flows: Flow[];
  page: number;
  page_size: number;
  page_count: number;
  total_rows: number;
}

/** A node's role in a flow's engine graph. */
export type FlowNodeKind =
  | 'subflow'
  | 'tool'
  | 'pause_point'
  | 'artifact_emitter';

export interface FlowNodePolicy {
  max_retries?: number;
  timeout_ms?: number;
}

export interface FlowNode {
  id: string;
  type: FlowNodeKind;
  descriptor?: string;
  policy?: FlowNodePolicy;
}

export interface FlowEdge {
  from: string;
  to: string;
}

export interface FlowDescription {
  flow: Flow;
  nodes: FlowNode[];
  edges: FlowEdge[];
  source?: string;
  budget_consumption: FlowBudgetConsumption;
}

export interface FlowDescribeRequest {
  identity: IdentityScope;
  id: string;
}

/** A flow run's outcome. */
export type FlowRunStatus =
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'cancelled';

/** What initiated a flow run. */
export type FlowRunTrigger = 'user' | 'planner' | 'system';

export interface FlowRun {
  run_id: string;
  flow_id: string;
  status: FlowRunStatus;
  trigger: FlowRunTrigger;
  started_at: string;
  duration_ms?: number;
  cost_usd?: number;
  identity: IdentityScope;
  error_class?: string;
}

export interface FlowRunsListRequest {
  identity: IdentityScope;
  flow_id: string;
  tenants?: string[];
  page?: number;
  page_size?: number;
}

export interface FlowRunsListResponse {
  runs: FlowRun[];
  page: number;
  page_size: number;
  page_count: number;
  total_rows: number;
}

/** By-reference handle for a heavy run output (D-026). */
export interface FlowArtifactRef {
  id: string;
  mime_type?: string;
  size_bytes?: number;
  filename?: string;
  sha256?: string;
}

export interface FlowNodeRunState {
  node_id: string;
  status: FlowRunStatus;
  duration_ms?: number;
  retries?: number;
  error_class?: string;
}

export interface FlowRunDescription {
  run: FlowRun;
  node_states: FlowNodeRunState[];
  output_preview?: string;
  output_ref?: FlowArtifactRef;
}

export interface FlowRunDescribeRequest {
  identity: IdentityScope;
  run_id: string;
}

export interface FlowRunRequest {
  identity: IdentityScope;
  flow_id: string;
  inputs?: Record<string, unknown>;
}

export interface FlowRunResponse {
  run_id: string;
  status: FlowRunStatus;
  started_at: string;
}

export interface FlowMetricsBucket {
  bucket_start: string;
  runs: number;
  p95_latency_ms: number;
  success_rate: number;
  cost_usd: number;
}

export interface FlowMetrics {
  flow_id: string;
  window_start: string;
  window_end: string;
  buckets: FlowMetricsBucket[];
  budget_consumption: FlowBudgetConsumption;
}

export interface FlowMetricsRequest {
  identity: IdentityScope;
  flow_id: string;
  window_ms?: number;
  bucket_ms?: number;
}

/** A canonical Protocol error body. */
export interface ProtocolError {
  code: string;
  message: string;
}
