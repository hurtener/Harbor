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

// AgentConfigPayload is the wire projection of an agent-config envelope.
// Every section is optional so later consumers extend it without a schema
// break.
type AgentConfigPayload struct {
	// Skills, when non-nil, pins the agent's skills membership for the
	// revision.
	Skills *AgentConfigSkillsSelection `json:"skills,omitempty"`
	// ToolExposure, when non-nil, pins the agent's MCP-exposure / per-tool
	// policy for the revision.
	ToolExposure *AgentConfigToolExposure `json:"tool_exposure,omitempty"`
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

// AgentConfigDiff is the wire projection of a server-side revision
// compare — the structured skills + tool-exposure set-diffs.
type AgentConfigDiff struct {
	FromRevisionID string                      `json:"from_revision_id"`
	ToRevisionID   string                      `json:"to_revision_id"`
	Skills         AgentConfigSkillsDiff       `json:"skills"`
	ToolExposure   AgentConfigToolExposureDiff `json:"tool_exposure"`
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
