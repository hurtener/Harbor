package types

import "time"

// agentconfig.go — the wire types for the admin-scoped `agent_config.*`
// Protocol family: the versioned desired-state registry surface (get /
// set_revision / list_revisions / diff / rollback) and its first
// consumer, skills control (skills.list / skills.upsert / skills.delete).
//
// Single source of truth (CLAUDE.md §8): these are THE Protocol
// agent-config wire types. The runtime-side `agentcfg.Revision` /
// `agentcfg.ConfigPayload` / `skills.Skill` structs are NOT re-exported
// here — the service projects the internal shapes onto these wire types so
// a future change to an internal struct does not silently reshape the
// Protocol surface.
//
// Every write method (set_revision / rollback / skills.upsert /
// skills.delete) is admin-scoped (the verified `auth.ScopeAdmin` claim,
// enforced at the wire handler — the agent-config authorization model) and identity-mandatory. A config
// edit applies to the agent's NEXT run (next-turn projection — never
// mid-flight, per the concurrent-reuse contract).

// AgentConfigSkillsSelection is the wire projection of an agent's skills
// membership in a config revision — the set of skill names active for the
// agent. The revision records membership (names), never skill bodies (the
// bodies stay in the SkillStore).
type AgentConfigSkillsSelection struct {
	// Names is the membership set of skill names active for the agent.
	Names []string `json:"names"`
}

// AgentConfigToolExposure is the wire projection of an agent's MCP-exposure
// / per-tool policy in a config revision (exclusion-based): the set of
// paused MCP servers and the set of individually-disabled tools. Pausing a
// server excludes its tools from the next run's projection (the live
// transport stays warm); disabling a tool excludes that one tool. Tools are
// keyed `<source>_<tool>`; a server's tools share the source id.
type AgentConfigToolExposure struct {
	// PausedServers names the MCP source ids excluded from the next run's
	// projection (resume is a flag flip, not a re-dial).
	PausedServers []string `json:"paused_servers,omitempty"`
	// DisabledTools names the individually-disabled tools (`<source>_<tool>`).
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

// AgentConfigPromptLayers is the wire projection of an agent's layered
// system prompt in a config revision: an operator-owned base layer plus an
// optional user layer that composes ABOVE the base without mutating it. The
// composition order is the security boundary — the user layer can extend
// the operator's guidance but never precede, replace, or weaken the base.
type AgentConfigPromptLayers struct {
	// Base is the operator-owned base prompt layer. When set it is the run's
	// base system prompt (overriding the agent's configured default base);
	// unset inherits the configured default.
	Base *string `json:"base,omitempty"`
	// User is the optional higher user-instruction layer composed above the
	// base in the lower-trust guidance position.
	User *string `json:"user,omitempty"`
}

// AgentConfigMCPConnectionDescriptor is the wire projection of one
// runtime-added MCP server connection — the NON-SECRET descriptor only
// (name, transport, stdio argv command or http URL). Secret auth material
// (bearer headers, OAuth tokens, credentials) is NEVER part of this
// descriptor: it flows through the live attach + the tool-side OAuth /
// pause-resume path and is never persisted in a revision, diff, or event.
type AgentConfigMCPConnectionDescriptor struct {
	// Name is the unique MCP source id. Required.
	Name string `json:"name"`
	// Transport is the wire transport — "stdio" or "http".
	Transport string `json:"transport"`
	// Command is the stdio argv (argv[0] is the binary; no shell). Set for
	// the stdio transport.
	Command []string `json:"command,omitempty"`
	// URL is the http(s) endpoint. Set for the http transport.
	URL string `json:"url,omitempty"`
}

// AgentConfigConnections is the wire projection of the runtime-added
// MCP-connection section of the config envelope — the set of NON-SECRET
// connection descriptors recorded in a revision (part of the agent's
// versioned desired state for diff / rollback).
type AgentConfigConnections struct {
	// Servers is the set of runtime-added MCP connection descriptors.
	Servers []AgentConfigMCPConnectionDescriptor `json:"servers,omitempty"`
}

// AgentConfigPayload is the wire projection of an agent-config envelope.
// Every section is optional so later consumers extend it without a schema
// break.
type AgentConfigPayload struct {
	// PromptLayers, when non-nil, pins the agent's layered system prompt
	// (operator base + optional user layer) for the revision.
	PromptLayers *AgentConfigPromptLayers `json:"prompt_layers,omitempty"`
	// Skills, when non-nil, pins the agent's skills membership for the
	// revision.
	Skills *AgentConfigSkillsSelection `json:"skills,omitempty"`
	// ToolExposure, when non-nil, pins the agent's MCP-exposure / per-tool
	// policy for the revision.
	ToolExposure *AgentConfigToolExposure `json:"tool_exposure,omitempty"`
	// Connections, when non-nil, pins the agent's runtime-added MCP
	// connection descriptors (non-secret) for the revision.
	Connections *AgentConfigConnections `json:"connections,omitempty"`
}

// AgentConfigRevisionView is the wire projection of one immutable config
// revision.
type AgentConfigRevisionView struct {
	// RevisionID is the revision's unique, time-ordered id.
	RevisionID string `json:"revision_id"`
	// ParentRevisionID is the revision this one descends from (empty for
	// the first revision).
	ParentRevisionID string `json:"parent_revision_id,omitempty"`
	// ContentHash is the full hex SHA-256 over the revision's canonical
	// payload encoding (for diff/audit correlation; never the content).
	ContentHash string `json:"content_hash"`
	// AuthorTenant / AuthorUser identify the author (the audit anchor).
	// The author's session is intentionally omitted — the audit anchor is
	// the (tenant, user) actor, not the session.
	AuthorTenant string `json:"author_tenant,omitempty"`
	AuthorUser   string `json:"author_user,omitempty"`
	// CreatedAt is the revision's creation instant.
	CreatedAt time.Time `json:"created_at"`
	// Payload is the config envelope this revision pins.
	Payload AgentConfigPayload `json:"payload"`
}

// AgentConfigSkillsDiff is the wire projection of the structured skills
// set-diff across two revisions.
type AgentConfigSkillsDiff struct {
	// Added are the skill names present in the to-revision but not the
	// from-revision.
	Added []string `json:"added,omitempty"`
	// Removed are the skill names present in the from-revision but not the
	// to-revision.
	Removed []string `json:"removed,omitempty"`
}

// AgentConfigToolExposureDiff is the wire projection of the structured
// MCP-exposure / per-tool policy set-diff across two revisions.
type AgentConfigToolExposureDiff struct {
	// PausedAdded / PausedResumed are the MCP servers newly paused / newly
	// resumed across the two revisions.
	PausedAdded   []string `json:"paused_added,omitempty"`
	PausedResumed []string `json:"paused_resumed,omitempty"`
	// DisabledAdded / DisabledEnabled are the tools newly disabled / newly
	// re-enabled across the two revisions.
	DisabledAdded   []string `json:"disabled_added,omitempty"`
	DisabledEnabled []string `json:"disabled_enabled,omitempty"`
}

// AgentConfigPromptLayersDiff is the wire projection of the base + user
// prompt-layer text delta across two revisions.
type AgentConfigPromptLayersDiff struct {
	// BaseChanged reports whether the base layer text differs.
	BaseChanged bool `json:"base_changed"`
	// BaseFrom / BaseTo are the base layer text in the from / to revision.
	BaseFrom string `json:"base_from,omitempty"`
	BaseTo   string `json:"base_to,omitempty"`
	// UserChanged reports whether the user layer text differs.
	UserChanged bool `json:"user_changed"`
	// UserFrom / UserTo are the user layer text in the from / to revision.
	UserFrom string `json:"user_from,omitempty"`
	UserTo   string `json:"user_to,omitempty"`
}

// AgentConfigConnectionsDiff is the wire projection of the structured
// runtime-added MCP-connection set-diff (by name) across two revisions.
type AgentConfigConnectionsDiff struct {
	// Added are the connection names present in the to-revision but not the
	// from-revision.
	Added []string `json:"added,omitempty"`
	// Removed are the connection names present in the from-revision but not
	// the to-revision.
	Removed []string `json:"removed,omitempty"`
}

// AgentConfigDiff is the wire projection of a server-side revision
// compare — the structured skills + tool-exposure + connection set-diffs
// and the prompt-layer text delta.
type AgentConfigDiff struct {
	FromRevisionID string                      `json:"from_revision_id"`
	ToRevisionID   string                      `json:"to_revision_id"`
	Skills         AgentConfigSkillsDiff       `json:"skills"`
	ToolExposure   AgentConfigToolExposureDiff `json:"tool_exposure"`
	PromptLayers   AgentConfigPromptLayersDiff `json:"prompt_layers"`
	Connections    AgentConfigConnectionsDiff  `json:"connections"`
}

// AgentConfigGetRequest is the `agent_config.get` request — read the
// agent's current active revision.
type AgentConfigGetRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
}

// AgentConfigGetResponse is the `agent_config.get` response.
type AgentConfigGetResponse struct {
	// Revision is the active revision; nil when the agent has no active
	// config (Set is false).
	Revision *AgentConfigRevisionView `json:"revision,omitempty"`
	// Set reports whether an active revision exists.
	Set             bool   `json:"set"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigSetRevisionRequest is the admin-scoped
// `agent_config.set_revision` request — write a new revision and advance
// the active pointer.
type AgentConfigSetRevisionRequest struct {
	Identity IdentityScope      `json:"identity"`
	AgentID  string             `json:"agent_id"`
	Payload  AgentConfigPayload `json:"payload"`
}

// AgentConfigSetRevisionResponse is the `agent_config.set_revision`
// response — the new (or, on an idempotent re-set, existing) revision.
type AgentConfigSetRevisionResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigListRevisionsRequest is the `agent_config.list_revisions`
// request — the agent's revision chain, newest-first.
type AgentConfigListRevisionsRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Limit caps the returned chain length. 0 = no cap.
	Limit int `json:"limit,omitempty"`
}

// AgentConfigListRevisionsResponse is the `agent_config.list_revisions`
// response.
type AgentConfigListRevisionsResponse struct {
	Revisions       []AgentConfigRevisionView `json:"revisions"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigDiffRequest is the `agent_config.diff` request — compare two
// existing revisions.
type AgentConfigDiffRequest struct {
	Identity     IdentityScope `json:"identity"`
	AgentID      string        `json:"agent_id"`
	FromRevision string        `json:"from_revision"`
	ToRevision   string        `json:"to_revision"`
}

// AgentConfigDiffResponse is the `agent_config.diff` response.
type AgentConfigDiffResponse struct {
	Diff            AgentConfigDiff `json:"diff"`
	ProtocolVersion string          `json:"protocol_version"`
}

// AgentConfigRollbackRequest is the admin-scoped `agent_config.rollback`
// request — repoint the active pointer to an existing revision.
type AgentConfigRollbackRequest struct {
	Identity   IdentityScope `json:"identity"`
	AgentID    string        `json:"agent_id"`
	RevisionID string        `json:"revision_id"`
}

// AgentConfigRollbackResponse is the `agent_config.rollback` response —
// the revision the active pointer now points to.
type AgentConfigRollbackResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigSetToolExposureRequest is the admin-scoped
// `agent_config.set_tool_exposure` request — set the agent's MCP-exposure /
// per-tool policy as a desired-state REPLACE of the tool-exposure section
// (the skills + prompt sections of the active revision are preserved). The
// edit records a new config revision and applies to the agent's NEXT run.
type AgentConfigSetToolExposureRequest struct {
	Identity     IdentityScope           `json:"identity"`
	AgentID      string                  `json:"agent_id"`
	ToolExposure AgentConfigToolExposure `json:"tool_exposure"`
}

// AgentConfigSetToolExposureResponse is the `agent_config.set_tool_exposure`
// response — the recorded config revision (or, on an idempotent re-set, the
// existing active revision).
type AgentConfigSetToolExposureResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigSetPromptLayersRequest is the admin-scoped
// `agent_config.set_prompt_layers` request — set the agent's layered system
// prompt (operator base and/or user layer) as a desired-state REPLACE of the
// prompt-layer section (the skills + tool-exposure sections of the active
// revision are preserved). The edit records a new config revision and applies
// to the agent's NEXT run.
type AgentConfigSetPromptLayersRequest struct {
	Identity     IdentityScope           `json:"identity"`
	AgentID      string                  `json:"agent_id"`
	PromptLayers AgentConfigPromptLayers `json:"prompt_layers"`
}

// AgentConfigSetPromptLayersResponse is the `agent_config.set_prompt_layers`
// response — the recorded config revision (or, on an idempotent re-set, the
// existing active revision).
type AgentConfigSetPromptLayersResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigAddMCPConnectionRequest is the admin-scoped
// `agent_config.add_mcp_connection` request — add a NEW MCP server
// connection (the separable async-dial path). The runtime drives the real
// attach lifecycle (dial → initialize handshake → discover → register),
// records the NON-SECRET descriptor as a config revision (preserving the
// skills / tool-exposure / prompt-layer sections), and — for a stdio add —
// gates on the operator allowlist (fail-closed; argv-form only). An
// auth-required server parks on the unified pause/resume primitive.
type AgentConfigAddMCPConnectionRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Connection is the NON-SECRET descriptor (name, transport, command/URL).
	Connection AgentConfigMCPConnectionDescriptor `json:"connection"`
	// Headers are OPTIONAL operator-supplied auth headers used ONLY for the
	// live attach (e.g. a bearer token for an http server). They are treated
	// as SECRETS: they flow to the transport but are NEVER persisted in the
	// recorded revision, the diff, or any emitted event (CLAUDE.md §7).
	Headers map[string]string `json:"headers,omitempty"`
}

// AgentConfigAddMCPConnectionResponse is the `agent_config.add_mcp_connection`
// response — the recorded revision (when one was recorded), the descriptor,
// and the explicit attach lifecycle state.
type AgentConfigAddMCPConnectionResponse struct {
	// Revision is the recorded config revision (set when State is "online"
	// or "auth_required"; nil for a "failed" add, which records no revision).
	Revision *AgentConfigRevisionView `json:"revision,omitempty"`
	// Connection is the NON-SECRET descriptor that was added.
	Connection AgentConfigMCPConnectionDescriptor `json:"connection"`
	// State is the explicit attach lifecycle state: "online" (attached),
	// "failed" (dial / handshake / discover failed — no half-attach), or
	// "auth_required" (parked on the unified pause/resume primitive).
	State string `json:"state"`
	// Reason is a SAFE, operator-facing failure reason (set only when State
	// is "failed"); never a secret.
	Reason string `json:"reason,omitempty"`
	// PauseToken is the unified-pause/resume token the auth_required attach
	// parked on (set only when State is "auth_required"). An opaque runtime
	// handle, NOT a credential.
	PauseToken      string `json:"pause_token,omitempty"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigSkillSummary is the wire projection of one skill in the
// agent's store — metadata only, never the full body.
type AgentConfigSkillSummary struct {
	Name        string    `json:"name"`
	Title       string    `json:"title,omitempty"`
	Trigger     string    `json:"trigger,omitempty"`
	TaskType    string    `json:"task_type,omitempty"`
	Origin      string    `json:"origin"`
	Scope       string    `json:"scope"`
	ContentHash string    `json:"content_hash,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AgentConfigSkillInput is the wire request shape for a skill upsert. It
// maps onto the runtime `skills.Skill` mandatory fields (name, trigger,
// ≥1 step, origin, scope) plus the descriptive fields.
type AgentConfigSkillInput struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Trigger     string   `json:"trigger"`
	TaskType    string   `json:"task_type,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Steps       []string `json:"steps"`
	// Origin is the skill provenance (pack | generated). A skills.upsert
	// over a pack-origin skill with a non-pack input is refused (the
	// `ErrPackOverwriteRefused` path surfaces as a typed Protocol error).
	Origin string `json:"origin"`
	// Scope is the skill visibility (session | project | tenant | global).
	Scope string `json:"scope"`
}

// AgentConfigSkillsListRequest is the `agent_config.skills.list` request.
type AgentConfigSkillsListRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
}

// AgentConfigSkillsListResponse is the `agent_config.skills.list`
// response.
type AgentConfigSkillsListResponse struct {
	Skills          []AgentConfigSkillSummary `json:"skills"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigSkillsUpsertRequest is the admin-scoped
// `agent_config.skills.upsert` request.
type AgentConfigSkillsUpsertRequest struct {
	Identity IdentityScope         `json:"identity"`
	AgentID  string                `json:"agent_id"`
	Skill    AgentConfigSkillInput `json:"skill"`
}

// AgentConfigSkillsUpsertResponse is the `agent_config.skills.upsert`
// response — the recorded config revision plus the upserted skill
// summary.
type AgentConfigSkillsUpsertResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	Skill           AgentConfigSkillSummary `json:"skill"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigSkillsDeleteRequest is the admin-scoped
// `agent_config.skills.delete` request.
type AgentConfigSkillsDeleteRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	Name     string        `json:"name"`
}

// AgentConfigSkillsDeleteResponse is the `agent_config.skills.delete`
// response — the recorded config revision after the membership change.
type AgentConfigSkillsDeleteResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}
