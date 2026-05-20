/**
 * Agents-page Protocol wire types + typed client view (Phase 73e /
 * D-124; built against D-121).
 *
 * # Wire types + a typed view — the client is the unified `HarborClient`
 *
 * The `HarborClient.agents` namespace (`$lib/protocol/client.ts`) exposes
 * generic `agents.*` methods typed `<R = unknown>`. This module is the
 * thin typed surface the Agents page narrows them with: the request /
 * response wire shapes plus an `AgentsProtocol` view that binds the
 * generic namespace methods to those shapes. There is no `fetch` here
 * and no page-local client class (CLAUDE.md §4.5 rule 5, §13).
 *
 * The wire shapes mirror `internal/protocol/types/agents.go` field-for-
 * field (the Go-side single source per D-002). When the
 * `cmd/harbor-gen-protocol-ts` generator (D-093) ships, these types fold
 * into the generated `protocol.ts` and this module re-exports from there
 * — a mechanical migration.
 *
 * # agent_id is NOT an isolation principal
 *
 * `Agent.id` is a registration identity (D-059). The runtime scopes
 * every `agents.*` read by the operator's `(tenant, user, session)`
 * triple — never by `agent_id`. The Console likewise treats `agent_id`
 * as an opaque handle, never as an isolation key.
 */

import type { ProtocolClient } from './client.js';

/** Wire enum — an agent's lifecycle status pill. */
export type AgentStatus =
  | 'active'
  | 'paused'
  | 'drained'
  | 'force_stopped'
  | 'deregistered';

/** Wire enum — an agent's operational-health badge. */
export type AgentHealth =
  | 'Healthy'
  | 'Degraded'
  | 'Paused'
  | 'Drained'
  | 'Force-Stopped'
  | 'Unknown';

/** Wire enum — an agent's hosting model (D-060). */
export type AgentHosting = 'local' | 'remote';

/** The catalog-row projection of one registered agent. */
export interface Agent {
  id: string;
  name: string;
  description: string;
  incarnation: number;
  version_hash: string;
  owner: string;
  status: AgentStatus;
  health: AgentHealth;
  hosting: AgentHosting;
  planner_type: string;
  model: string;
  tools_count: number;
  mcp_count: number;
  registered_at: string;
  updated_at: string;
}

/** Server-enforced facet filter on `agents.list`. */
export interface AgentFilter {
  status?: AgentStatus[];
  planner_type?: string[];
  search?: string;
}

/** The `agents.list` request body. */
export interface AgentListRequest {
  filter?: AgentFilter;
  page?: number;
  page_size?: number;
}

/** The four catalog counters over the filtered view. */
export interface AgentAggregates {
  total: number;
  active: number;
  paused: number;
  drained: number;
}

/** The `agents.list` reply. */
export interface AgentListResponse {
  agents: Agent[];
  page: number;
  page_size: number;
  page_count: number;
  total_rows: number;
  aggregates: AgentAggregates;
}

/** The Protocol projection of an agent's configuration. */
export interface AgentConfig {
  planner_type: string;
  planner_config?: Record<string, string>;
  model: string;
  model_policy?: Record<string, string>;
  max_steps: number;
}

/** The `agents.get` reply — one agent's full projection. */
export interface AgentGetResponse {
  agent: Agent;
  config: AgentConfig;
  agent_card_ref?: string;
}

/** One tool binding on an agent + per-binding OAuth status (D-083). */
export interface AgentToolBinding {
  tool_id: string;
  tool_name: string;
  transport: string;
  auth_status: string;
  binding_scope?: string;
}

/** The `agents.tools` reply. */
export interface AgentToolsResponse {
  agent_id: string;
  bindings: AgentToolBinding[];
}

/** The agent's configured memory strategy. */
export interface AgentMemoryBinding {
  strategy_id: string;
  ttl_seconds: number;
  scope: string;
}

/** The `agents.memory` reply. */
export interface AgentMemoryResponse {
  agent_id: string;
  binding: AgentMemoryBinding;
}

/** One per-identity-tier cost ceiling (Phase 36a). */
export interface AgentCostCeiling {
  tier: string;
  limit_usd: number;
  spend_usd: number;
}

/** One per-identity-tier rate-limit posture (Phase 36b). */
export interface AgentRateLimit {
  tier: string;
  requests_per_minute: number;
  max_tokens: number;
}

/** The agent's governance posture. */
export interface AgentGovernance {
  ceilings: AgentCostCeiling[];
  rate_limits: AgentRateLimit[];
}

/** The `agents.governance` reply. */
export interface AgentGovernanceResponse {
  agent_id: string;
  governance: AgentGovernance;
}

/** One skill attached to an agent (Phase 38 + Phase 41 generated). */
export interface AgentSkillBinding {
  skill_id: string;
  name: string;
  generated: boolean;
}

/** The `agents.skills` reply. */
export interface AgentSkillsResponse {
  agent_id: string;
  skills: AgentSkillBinding[];
}

/** The agent's permission model (V1 default: implicit). */
export interface AgentPermissions {
  model: string;
  description: string;
  allowed_principals?: string[];
}

/** The `agents.permissions` reply. */
export interface AgentPermissionsResponse {
  agent_id: string;
  permissions: AgentPermissions;
}

/** The registry-wide rollup the Agents page hero shows. */
export interface AgentMetrics {
  active_agents: number;
  running_tasks: number;
  total_cost_usd: number;
  total_tokens: number;
}

/** The `agents.metrics` reply. */
export interface AgentMetricsResponse {
  metrics: AgentMetrics;
}

/**
 * `AgentsProtocol` is the typed Agents-page view over the unified
 * `HarborClient.agents` namespace. The page constructs one over an
 * injected {@link ProtocolClient} and calls its typed methods; the
 * Playwright harness injects a deterministic client so the page is
 * exercised without a live Runtime.
 */
export class AgentsProtocol {
  readonly #client: ProtocolClient;

  constructor(client: ProtocolClient) {
    this.#client = client;
  }

  /** `agents.list` — the paginated, faceted agent catalog. */
  list(req: AgentListRequest = {}): Promise<AgentListResponse> {
    return this.#client.agents.list<AgentListResponse>(
      req as Record<string, unknown>
    );
  }

  /** `agents.get` — one agent's full projection. */
  get(id: string): Promise<AgentGetResponse> {
    return this.#client.agents.get<AgentGetResponse>(id);
  }

  /** `agents.tools` — the agent's tool bindings. */
  tools(id: string): Promise<AgentToolsResponse> {
    return this.#client.agents.tools<AgentToolsResponse>(id);
  }

  /** `agents.memory` — the agent's memory binding. */
  memory(id: string): Promise<AgentMemoryResponse> {
    return this.#client.agents.memory<AgentMemoryResponse>(id);
  }

  /** `agents.governance` — the agent's governance posture. */
  governance(id: string): Promise<AgentGovernanceResponse> {
    return this.#client.agents.governance<AgentGovernanceResponse>(id);
  }

  /** `agents.skills` — the agent's attached skills. */
  skills(id: string): Promise<AgentSkillsResponse> {
    return this.#client.agents.skills<AgentSkillsResponse>(id);
  }

  /** `agents.permissions` — the agent's permission model. */
  permissions(id: string): Promise<AgentPermissionsResponse> {
    return this.#client.agents.permissions<AgentPermissionsResponse>(id);
  }

  /** `agents.metrics` — the registry-wide rollup. */
  metrics(): Promise<AgentMetricsResponse> {
    return this.#client.agents.metrics<AgentMetricsResponse>();
  }
}
