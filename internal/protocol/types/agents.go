// Phase 73e (Wave 13 / D-124) — the Console Agents-page Protocol wire
// types. These structs are the single source of truth (D-002 / §8) for
// the eight `agents.*` methods the Console Agents page consumes:
//
//   - agents.list        — paginated, faceted agent catalog projection.
//   - agents.get         — one agent's full registration-identity
//     projection (identity / hosting / status / health / AgentConfig).
//   - agents.tools       — the agent's tool bindings + per-binding OAuth.
//   - agents.memory      — the agent's memory strategy + TTL + scope.
//   - agents.governance  — per-identity-tier ceilings + spend + limits.
//   - agents.skills      — the agent's attached skills.
//   - agents.permissions — the agent's permission model (V1: implicit).
//   - agents.metrics     — the registry-wide rollup the page hero shows.
//
// # agent_id is NOT an isolation principal
//
// Every `agents.*` wire request carries an IdentityScope — the
// (tenant, user, session) triple. The runtime filters by that triple,
// NEVER by AgentID. AgentID is a registration identity (D-059), not a
// WHERE-clause isolation key (CLAUDE.md §6 clarifying note).
//
// # Flat Protocol projection, not a re-export
//
// These types are the flat wire projection per RFC §5.1 — they mirror
// the registry's AgentRecord / AgentSnapshot field-for-field where it
// makes sense but are NOT a 1:1 Go re-export of an internal type
// (CLAUDE.md §8 — "a Protocol method that maps 1:1 to an internal Go
// function signature is a smell"). The Console reads only these.

package types

// DefaultAgentListPageSize is the page size `agents.list` applies when
// the request omits PageSize (or sends 0). Agents are slow-moving
// catalog data; a 50-row page is comfortable for the cards grid.
const DefaultAgentListPageSize = 50

// MaxAgentListPageSize bounds the `agents.list` page size. A request
// above this ceiling is rejected with CodeInvalidRequest rather than
// silently clamped (CLAUDE.md §13 — fail loudly).
const MaxAgentListPageSize = 200

// AgentStatus is the lifecycle-status enum the Agents page renders as a
// status pill. It mirrors the registry's control-verb outcomes.
type AgentStatus string

const (
	// AgentStatusActive — the agent is registered and accepting work.
	AgentStatusActive AgentStatus = "active"
	// AgentStatusPaused — a registry.Pause control verb is in effect.
	AgentStatusPaused AgentStatus = "paused"
	// AgentStatusDrained — a registry.Drain control verb is in effect.
	AgentStatusDrained AgentStatus = "drained"
	// AgentStatusForceStopped — a registry.ForceStop verb is in effect.
	AgentStatusForceStopped AgentStatus = "force_stopped"
	// AgentStatusDeregistered — the agent was removed from the registry.
	AgentStatusDeregistered AgentStatus = "deregistered"
)

// IsValidAgentStatus reports whether s is one of the canonical
// AgentStatus values.
func IsValidAgentStatus(s AgentStatus) bool {
	switch s {
	case AgentStatusActive, AgentStatusPaused, AgentStatusDrained,
		AgentStatusForceStopped, AgentStatusDeregistered:
		return true
	default:
		return false
	}
}

// AgentHealth is the operational-health enum the Agents page renders as
// a health badge. It mirrors the registry's Health enum projected onto
// operator-facing labels.
type AgentHealth string

const (
	// AgentHealthHealthy — the agent reported itself operational.
	AgentHealthHealthy AgentHealth = "Healthy"
	// AgentHealthDegraded — the agent reported impaired operation.
	AgentHealthDegraded AgentHealth = "Degraded"
	// AgentHealthPaused — a Pause control verb is in effect.
	AgentHealthPaused AgentHealth = "Paused"
	// AgentHealthDrained — a Drain control verb is in effect.
	AgentHealthDrained AgentHealth = "Drained"
	// AgentHealthForceStopped — a ForceStop control verb is in effect.
	AgentHealthForceStopped AgentHealth = "Force-Stopped"
	// AgentHealthUnknown — no health has been reported yet.
	AgentHealthUnknown AgentHealth = "Unknown"
)

// AgentHosting discriminates a locally-hosted agent from a
// connect-to-remote agent (D-060).
type AgentHosting string

const (
	// AgentHostingLocal — the agent is hosted by this runtime instance.
	AgentHostingLocal AgentHosting = "local"
	// AgentHostingRemote — the agent runs elsewhere; the local agent_id
	// is a handle and the canonical identity is the remote A2A AgentCard.
	AgentHostingRemote AgentHosting = "remote"
)

// Agent is the catalog-row projection of one registered agent — the
// shape `agents.list` returns per row and `agents.get` nests in its
// response. ID is a registration identity (D-059), NOT an isolation
// principal.
type Agent struct {
	// ID is the agent_id — the stable registration identity (D-059).
	ID string `json:"id"`
	// Name is the operator-facing display name.
	Name string `json:"name"`
	// Description is the operator-facing description.
	Description string `json:"description"`
	// Incarnation bumps on every process start (D-059).
	Incarnation int64 `json:"incarnation"`
	// VersionHash is the SHA-256 over canonical JSON of AgentConfig
	// (D-068). Empty for a HostingRemote agent.
	VersionHash string `json:"version_hash"`
	// Owner is the registration key of the agent — the operator-stable
	// logical-agent key the admin registered it under.
	Owner string `json:"owner"`
	// Status is the lifecycle status pill.
	Status AgentStatus `json:"status"`
	// Health is the operational-health badge.
	Health AgentHealth `json:"health"`
	// Hosting discriminates locally-hosted vs connect-to-remote.
	Hosting AgentHosting `json:"hosting"`
	// PlannerType is the configured planner ("react" / "deterministic"
	// / future). Empty when not declared in AgentConfig.
	PlannerType string `json:"planner_type"`
	// Model is the configured model id. Empty when not declared.
	Model string `json:"model"`
	// ToolsCount is the number of tool bindings on the agent.
	ToolsCount int `json:"tools_count"`
	// MCPCount is the number of MCP-transport tool bindings.
	MCPCount int `json:"mcp_count"`
	// RegisteredAt is the RFC3339 timestamp of the first registration.
	RegisteredAt string `json:"registered_at"`
	// UpdatedAt is the RFC3339 timestamp of the most recent mutation.
	UpdatedAt string `json:"updated_at"`
}

// AgentFilter is the server-enforced facet filter on `agents.list`.
type AgentFilter struct {
	// Status narrows to agents whose Status is in the set; empty = all.
	Status []AgentStatus `json:"status,omitempty"`
	// PlannerType narrows to agents whose PlannerType is in the set;
	// empty = all.
	PlannerType []string `json:"planner_type,omitempty"`
	// Search is a free-text query over Name + Description (case-fold).
	Search string `json:"search,omitempty"`
}

// AgentListRequest is the `agents.list` request body.
type AgentListRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// Filter is the optional facet filter.
	Filter AgentFilter `json:"filter,omitempty"`
	// Page is the 1-based page index. <= 0 is treated as page 1.
	Page int `json:"page,omitempty"`
	// PageSize is the rows-per-page. 0 ⇒ DefaultAgentListPageSize.
	PageSize int `json:"page_size,omitempty"`
}

// AgentAggregates is the four catalog counters over the filtered view.
type AgentAggregates struct {
	// Total is the count of agents in the filtered view.
	Total int64 `json:"total"`
	// Active is the count of AgentStatusActive agents in the view.
	Active int64 `json:"active"`
	// Paused is the count of AgentStatusPaused agents in the view.
	Paused int64 `json:"paused"`
	// Drained is the count of AgentStatusDrained agents in the view.
	Drained int64 `json:"drained"`
}

// AgentListResponse is the `agents.list` reply.
type AgentListResponse struct {
	// Agents is the page of catalog rows.
	Agents []Agent `json:"agents"`
	// Page is the 1-based page index this response covers.
	Page int `json:"page"`
	// PageSize is the rows-per-page applied.
	PageSize int `json:"page_size"`
	// PageCount is the total number of pages in the filtered view.
	PageCount int `json:"page_count"`
	// TotalRows is the total row count in the filtered view.
	TotalRows int64 `json:"total_rows"`
	// Aggregates is the four counters over the filtered view.
	Aggregates AgentAggregates `json:"aggregates"`
}

// AgentGetRequest is the `agents.get` request body.
type AgentGetRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id to fetch.
	ID string `json:"id"`
}

// AgentConfig is the Protocol projection of the agent's configuration —
// planner config + model + max-steps + cost ceilings. It mirrors the
// registry's AgentConfig but is the flat wire shape, not the internal
// type (RFC §5.1).
type AgentConfig struct {
	// PlannerType is the configured planner ("react" / "deterministic").
	PlannerType string `json:"planner_type"`
	// PlannerConfig is the planner's key/value configuration set
	// (MaxSteps, repair policy, etc.).
	PlannerConfig map[string]string `json:"planner_config,omitempty"`
	// Model is the configured model id.
	Model string `json:"model"`
	// ModelPolicy is the model-selection / model-policy key/value set.
	ModelPolicy map[string]string `json:"model_policy,omitempty"`
	// MaxSteps is the planner's max-steps ceiling (0 ⇒ not declared).
	MaxSteps int `json:"max_steps"`
}

// AgentGetResponse is the `agents.get` reply — the full projection of
// one agent.
type AgentGetResponse struct {
	// Agent is the catalog-row projection.
	Agent Agent `json:"agent"`
	// Config is the agent's configuration projection.
	Config AgentConfig `json:"config"`
	// AgentCardRef references the canonical A2A AgentCard for a
	// HostingRemote agent. Empty for a HostingLocal agent.
	AgentCardRef string `json:"agent_card_ref,omitempty"`
}

// AgentToolsRequest is the `agents.tools` request body.
type AgentToolsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id whose tool bindings to list.
	ID string `json:"id"`
}

// AgentToolBinding is one tool binding on an agent — a tool + the
// per-binding OAuth status (D-083).
type AgentToolBinding struct {
	// ToolID is the catalog key of the bound tool.
	ToolID string `json:"tool_id"`
	// ToolName is the operator-facing tool name.
	ToolName string `json:"tool_name"`
	// Transport is the tool's transport ("in-proc" / "HTTP" / "MCP" /
	// "A2A" / "flow").
	Transport string `json:"transport"`
	// AuthStatus is the per-binding OAuth status: "no_auth" / "headers"
	// / "oauth_user_bound" / "oauth_agent_bound" / "oauth_expired".
	AuthStatus string `json:"auth_status"`
	// BindingScope is the OAuth binding scope (auth.BindingScope per
	// D-083): "user" / "agent". Empty for a non-OAuth binding.
	BindingScope string `json:"binding_scope,omitempty"`
}

// AgentToolsResponse is the `agents.tools` reply.
type AgentToolsResponse struct {
	// AgentID echoes the requested agent_id.
	AgentID string `json:"agent_id"`
	// Bindings is the agent's tool bindings.
	Bindings []AgentToolBinding `json:"bindings"`
}

// AgentMemoryRequest is the `agents.memory` request body.
type AgentMemoryRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id whose memory binding to fetch.
	ID string `json:"id"`
}

// AgentMemoryBinding is the agent's configured memory strategy.
type AgentMemoryBinding struct {
	// StrategyID is the memory strategy id (Phase 24).
	StrategyID string `json:"strategy_id"`
	// TTLSeconds is the configured memory TTL in seconds (0 ⇒ no TTL).
	TTLSeconds int64 `json:"ttl_seconds"`
	// Scope is the memory scope: "session" / "user" / "tenant".
	Scope string `json:"scope"`
}

// AgentMemoryResponse is the `agents.memory` reply.
type AgentMemoryResponse struct {
	// AgentID echoes the requested agent_id.
	AgentID string `json:"agent_id"`
	// Binding is the agent's memory-strategy binding.
	Binding AgentMemoryBinding `json:"binding"`
}

// AgentGovernanceRequest is the `agents.governance` request body.
type AgentGovernanceRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id whose governance posture to fetch.
	ID string `json:"id"`
}

// AgentCostCeiling is one per-identity-tier cost ceiling (Phase 36a).
type AgentCostCeiling struct {
	// Tier is the identity tier the ceiling applies to.
	Tier string `json:"tier"`
	// LimitUSD is the configured spend ceiling in USD.
	LimitUSD float64 `json:"limit_usd"`
	// SpendUSD is the current accumulated spend in USD.
	SpendUSD float64 `json:"spend_usd"`
}

// AgentRateLimit is one per-identity-tier rate-limit posture (Phase
// 36b).
type AgentRateLimit struct {
	// Tier is the identity tier the rate limit applies to.
	Tier string `json:"tier"`
	// RequestsPerMinute is the configured per-minute request ceiling.
	RequestsPerMinute int64 `json:"requests_per_minute"`
	// MaxTokens is the configured per-request MaxTokens ceiling.
	MaxTokens int64 `json:"max_tokens"`
}

// AgentGovernance is the agent's governance posture — per-identity-tier
// ceilings + spend + rate-limit posture.
type AgentGovernance struct {
	// Ceilings is the per-identity-tier cost ceilings + spend.
	Ceilings []AgentCostCeiling `json:"ceilings"`
	// RateLimits is the per-identity-tier rate-limit posture.
	RateLimits []AgentRateLimit `json:"rate_limits"`
}

// AgentGovernanceResponse is the `agents.governance` reply.
type AgentGovernanceResponse struct {
	// AgentID echoes the requested agent_id.
	AgentID string `json:"agent_id"`
	// Governance is the agent's governance posture.
	Governance AgentGovernance `json:"governance"`
}

// AgentSkillsRequest is the `agents.skills` request body.
type AgentSkillsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id whose attached skills to list.
	ID string `json:"id"`
}

// AgentSkillBinding is one skill attached to an agent (Phase 38 + Phase
// 41 generated skills).
type AgentSkillBinding struct {
	// SkillID is the catalog key of the attached skill.
	SkillID string `json:"skill_id"`
	// Name is the operator-facing skill name.
	Name string `json:"name"`
	// Generated reports whether the skill was generated in-runtime
	// (Phase 41) rather than imported (Phase 38).
	Generated bool `json:"generated"`
}

// AgentSkillsResponse is the `agents.skills` reply.
type AgentSkillsResponse struct {
	// AgentID echoes the requested agent_id.
	AgentID string `json:"agent_id"`
	// Skills is the agent's attached skills.
	Skills []AgentSkillBinding `json:"skills"`
}

// AgentPermissionsRequest is the `agents.permissions` request body.
type AgentPermissionsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
	// ID is the agent_id whose permission model to fetch.
	ID string `json:"id"`
}

// AgentPermissions is the agent's permission model. V1 default is
// implicit ("every authenticated user in the tenant can invoke this
// agent"); an explicit ACL surface is post-V1 (page-agents.md §10).
type AgentPermissions struct {
	// Model is the permission model: "implicit" (V1 default) or
	// "explicit" (post-V1).
	Model string `json:"model"`
	// Description is the operator-facing explanation of the model.
	Description string `json:"description"`
	// AllowedPrincipals is the explicit ACL when Model is "explicit";
	// empty for the implicit model.
	AllowedPrincipals []string `json:"allowed_principals,omitempty"`
}

// AgentPermissionsResponse is the `agents.permissions` reply.
type AgentPermissionsResponse struct {
	// AgentID echoes the requested agent_id.
	AgentID string `json:"agent_id"`
	// Permissions is the agent's permission model.
	Permissions AgentPermissions `json:"permissions"`
}

// AgentMetricsRequest is the `agents.metrics` request body.
type AgentMetricsRequest struct {
	// Identity is the (tenant, user, session) scope. Mandatory.
	Identity IdentityScope `json:"identity"`
}

// AgentMetrics is the registry-wide rollup the Agents page hero shows —
// the four "Active Agents / Running Tasks / Total Cost / Total Tokens"
// numbers, computed over the operator's identity scope.
type AgentMetrics struct {
	// ActiveAgents is the count of AgentStatusActive agents in scope.
	ActiveAgents int64 `json:"active_agents"`
	// RunningTasks is the count of in-flight tasks across those agents.
	RunningTasks int64 `json:"running_tasks"`
	// TotalCostUSD is the total accumulated spend across those agents.
	TotalCostUSD float64 `json:"total_cost_usd"`
	// TotalTokens is the total token consumption across those agents.
	TotalTokens int64 `json:"total_tokens"`
}

// AgentMetricsResponse is the `agents.metrics` reply.
type AgentMetricsResponse struct {
	// Metrics is the registry-wide rollup.
	Metrics AgentMetrics `json:"metrics"`
}
