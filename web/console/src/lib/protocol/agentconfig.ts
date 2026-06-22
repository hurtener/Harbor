/**
 * Agent-config control-plane wire types — the `agent_config.*` Protocol
 * family: the versioned desired-state registry (get / set_revision /
 * list_revisions / diff / rollback) and its first consumer, skills control
 * (skills.list / skills.upsert / skills.delete).
 *
 * # Wire types only — the client lives in `client.ts`
 *
 * These mirror `internal/protocol/types/agentconfig.go` field-for-field —
 * the Go side is the single source (D-093 / D-223), kept in lockstep with
 * `wire-manifest.gen.json` by `npm run lint`
 * (`check-protocol-ts-lockstep.mjs`). A dropped/renamed/mistyped field
 * fails the guard. The `AgentConfigNamespace` (in `client.ts`) issues the
 * calls; these are the request/response shapes it sends and narrows.
 *
 * Every write (set_revision / rollback / skills.upsert / skills.delete) is
 * admin-scoped (D-235) and applies to the agent's NEXT run (next-turn
 * projection — never mid-flight, per D-025).
 */

import type { IdentityScope } from './memory-types';

/** An agent's skills membership in a config revision — the set of skill
 * names active for the agent. Mirrors `types.AgentConfigSkillsSelection`. */
export interface AgentConfigSkillsSelection {
	names: string[];
}

/** An agent's MCP-exposure / per-tool policy in a config revision
 * (exclusion-based): the paused MCP servers + individually-disabled tools.
 * Pausing a server excludes its tools from the next run (the live transport
 * stays warm); tools are keyed `<source>_<tool>`. Mirrors
 * `types.AgentConfigToolExposure`. */
export interface AgentConfigToolExposure {
	paused_servers?: string[];
	disabled_tools?: string[];
}

/** An agent's layered system prompt in a config revision: an operator-owned
 * base layer plus an optional user layer composed ABOVE the base without
 * mutating it. The composition order is the security boundary — the user layer
 * can extend but never replace or weaken the operator base. Mirrors
 * `types.AgentConfigPromptLayers`. */
export interface AgentConfigPromptLayers {
	base?: string;
	user?: string;
}

/** An agent's per-agent LLM-parameter section — the sampling defaults pinned
 * for the agent (model / temperature / max-tokens / reasoning-effort). Every
 * field optional; an unset field falls through to the tenant-wide baseline,
 * then the config default. Mirrors `types.AgentConfigLLMParams`. */
export interface AgentConfigLLMParams {
	model?: string;
	temperature?: number;
	max_tokens?: number;
	/** "off" | "low" | "medium" | "high". */
	reasoning_effort?: string;
}

/** One runtime-added MCP server connection — the NON-SECRET descriptor only
 * (name, transport, stdio argv command or http URL). Secret auth material is
 * NEVER part of this descriptor. Mirrors
 * `types.AgentConfigMCPConnectionDescriptor`. */
export interface AgentConfigMCPConnectionDescriptor {
	name: string;
	/** "stdio" | "http". */
	transport: string;
	command?: string[];
	url?: string;
}

/** The runtime-added MCP-connection section of the config envelope. Mirrors
 * `types.AgentConfigConnections`. */
export interface AgentConfigConnections {
	servers?: AgentConfigMCPConnectionDescriptor[];
}

/** An agent-config envelope — every section optional and forward-compatible.
 * Mirrors `types.AgentConfigPayload`. */
export interface AgentConfigPayload {
	prompt_layers?: AgentConfigPromptLayers;
	skills?: AgentConfigSkillsSelection;
	tool_exposure?: AgentConfigToolExposure;
	connections?: AgentConfigConnections;
	llm_params?: AgentConfigLLMParams;
}

/** One immutable config revision. Mirrors `types.AgentConfigRevisionView`. */
export interface AgentConfigRevisionView {
	revision_id: string;
	parent_revision_id?: string;
	content_hash: string;
	author_tenant?: string;
	author_user?: string;
	/** RFC3339 timestamp. */
	created_at: string;
	payload: AgentConfigPayload;
}

/** The structured skills set-diff across two revisions. Mirrors
 * `types.AgentConfigSkillsDiff`. */
export interface AgentConfigSkillsDiff {
	added?: string[];
	removed?: string[];
}

/** The structured MCP-exposure / per-tool policy set-diff across two
 * revisions. Mirrors `types.AgentConfigToolExposureDiff`. */
export interface AgentConfigToolExposureDiff {
	paused_added?: string[];
	paused_resumed?: string[];
	disabled_added?: string[];
	disabled_enabled?: string[];
}

/** The base + user prompt-layer text delta across two revisions. Mirrors
 * `types.AgentConfigPromptLayersDiff`. */
export interface AgentConfigPromptLayersDiff {
	base_changed: boolean;
	base_from?: string;
	base_to?: string;
	user_changed: boolean;
	user_from?: string;
	user_to?: string;
}

/** The structured runtime-added MCP-connection set-diff (by name) across two
 * revisions. Mirrors `types.AgentConfigConnectionsDiff`. */
export interface AgentConfigConnectionsDiff {
	added?: string[];
	removed?: string[];
}

/** The per-agent LLM-parameter per-field delta across two revisions. Mirrors
 * `types.AgentConfigLLMParamsDiff`. */
export interface AgentConfigLLMParamsDiff {
	model_changed: boolean;
	model_from?: string;
	model_to?: string;
	temperature_changed: boolean;
	temperature_from?: string;
	temperature_to?: string;
	max_tokens_changed: boolean;
	max_tokens_from?: string;
	max_tokens_to?: string;
	reasoning_effort_changed: boolean;
	reasoning_effort_from?: string;
	reasoning_effort_to?: string;
}

/** A server-side revision compare. Mirrors `types.AgentConfigDiff`. */
export interface AgentConfigDiff {
	from_revision_id: string;
	to_revision_id: string;
	skills: AgentConfigSkillsDiff;
	tool_exposure: AgentConfigToolExposureDiff;
	prompt_layers: AgentConfigPromptLayersDiff;
	connections: AgentConfigConnectionsDiff;
	llm_params: AgentConfigLLMParamsDiff;
}

/** `agent_config.get` request. */
export interface AgentConfigGetRequest {
	identity: IdentityScope;
	agent_id: string;
}

/** `agent_config.get` response. */
export interface AgentConfigGetResponse {
	revision?: AgentConfigRevisionView;
	set: boolean;
	protocol_version: string;
}

/** `agent_config.set_revision` request — admin-scoped. */
export interface AgentConfigSetRevisionRequest {
	identity: IdentityScope;
	agent_id: string;
	payload: AgentConfigPayload;
}

/** `agent_config.set_revision` response. */
export interface AgentConfigSetRevisionResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.list_revisions` request — admin-scoped. */
export interface AgentConfigListRevisionsRequest {
	identity: IdentityScope;
	agent_id: string;
	limit?: number;
}

/** `agent_config.list_revisions` response — newest-first. */
export interface AgentConfigListRevisionsResponse {
	revisions: AgentConfigRevisionView[];
	protocol_version: string;
}

/** `agent_config.diff` request — admin-scoped. */
export interface AgentConfigDiffRequest {
	identity: IdentityScope;
	agent_id: string;
	from_revision: string;
	to_revision: string;
}

/** `agent_config.diff` response. */
export interface AgentConfigDiffResponse {
	diff: AgentConfigDiff;
	protocol_version: string;
}

/** `agent_config.rollback` request — admin-scoped. */
export interface AgentConfigRollbackRequest {
	identity: IdentityScope;
	agent_id: string;
	revision_id: string;
}

/** `agent_config.rollback` response. */
export interface AgentConfigRollbackResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.set_tool_exposure` request — admin-scoped. Replaces ONLY
 * the tool-exposure section (the skills + prompt sections are preserved). */
export interface AgentConfigSetToolExposureRequest {
	identity: IdentityScope;
	agent_id: string;
	tool_exposure: AgentConfigToolExposure;
}

/** `agent_config.set_tool_exposure` response — the recorded revision. */
export interface AgentConfigSetToolExposureResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.set_prompt_layers` request — admin-scoped. Replaces ONLY
 * the prompt-layer section (the skills + tool-exposure sections are
 * preserved). The user layer composes above the base in the lower-trust
 * position; it can never replace or weaken the operator base. */
export interface AgentConfigSetPromptLayersRequest {
	identity: IdentityScope;
	agent_id: string;
	prompt_layers: AgentConfigPromptLayers;
}

/** `agent_config.set_prompt_layers` response — the recorded revision. */
export interface AgentConfigSetPromptLayersResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.set_llm_params` request — admin-scoped. Replaces ONLY the
 * LLM-params section (the prompt-layer + skills + tool-exposure + connection
 * sections are preserved). A set `model` is validated against the configured
 * ModelProfiles at set time (an unknown model is rejected). The per-agent
 * params override the tenant-wide baseline for the agent's next run. */
export interface AgentConfigSetLLMParamsRequest {
	identity: IdentityScope;
	agent_id: string;
	llm_params: AgentConfigLLMParams;
}

/** `agent_config.set_llm_params` response — the recorded revision. */
export interface AgentConfigSetLLMParamsResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.add_mcp_connection` request — admin-scoped. Adds a NEW MCP
 * server connection (the async dial + initialize handshake + possible OAuth
 * path). `headers` are OPTIONAL operator-supplied auth material used ONLY for
 * the live attach — they are NEVER persisted in the revision, diff, or events.
 * Mirrors `types.AgentConfigAddMCPConnectionRequest`. */
export interface AgentConfigAddMCPConnectionRequest {
	identity: IdentityScope;
	agent_id: string;
	connection: AgentConfigMCPConnectionDescriptor;
	headers?: Record<string, string>;
}

/** `agent_config.add_mcp_connection` response — the recorded revision (when
 * one was recorded), the descriptor, and the explicit attach lifecycle state
 * ("online" | "failed" | "auth_required"). Mirrors
 * `types.AgentConfigAddMCPConnectionResponse`. */
export interface AgentConfigAddMCPConnectionResponse {
	revision?: AgentConfigRevisionView;
	connection: AgentConfigMCPConnectionDescriptor;
	/** "online" | "failed" | "auth_required". */
	state: string;
	reason?: string;
	pause_token?: string;
	protocol_version: string;
}

/** One skill in the agent's store — metadata only. Mirrors
 * `types.AgentConfigSkillSummary`. */
export interface AgentConfigSkillSummary {
	name: string;
	title?: string;
	trigger?: string;
	task_type?: string;
	origin: string;
	scope: string;
	content_hash?: string;
	/** RFC3339 timestamp. */
	updated_at: string;
}

/** A skill-upsert input. Mirrors `types.AgentConfigSkillInput`. */
export interface AgentConfigSkillInput {
	name: string;
	title?: string;
	description?: string;
	trigger: string;
	task_type?: string;
	tags?: string[];
	steps: string[];
	/** pack | generated. */
	origin: string;
	/** session | project | tenant | global. */
	scope: string;
}

/** `agent_config.skills.list` request — admin-scoped. */
export interface AgentConfigSkillsListRequest {
	identity: IdentityScope;
	agent_id: string;
}

/** `agent_config.skills.list` response. */
export interface AgentConfigSkillsListResponse {
	skills: AgentConfigSkillSummary[];
	protocol_version: string;
}

/** `agent_config.skills.upsert` request — admin-scoped. */
export interface AgentConfigSkillsUpsertRequest {
	identity: IdentityScope;
	agent_id: string;
	skill: AgentConfigSkillInput;
}

/** `agent_config.skills.upsert` response — the recorded revision + skill. */
export interface AgentConfigSkillsUpsertResponse {
	revision: AgentConfigRevisionView;
	skill: AgentConfigSkillSummary;
	protocol_version: string;
}

/** `agent_config.skills.delete` request — admin-scoped. */
export interface AgentConfigSkillsDeleteRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
}

/** `agent_config.skills.delete` response — the recorded revision. */
export interface AgentConfigSkillsDeleteResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

// --- Session-user safe subset (the non-admin lower tier of the
// authorization matrix). A session-scoped caller may set a user prompt layer
// (never the operator base), narrow-only source/tool disable, and ephemeral
// personal skills. The session shapes carry NO base-prompt field and NO
// enable field. ---

/** A session's safe-subset overlay. Mirrors `types.AgentConfigSessionOverlay`.
 * There is NO base-prompt field — base is unwritable by a session caller. */
export interface AgentConfigSessionOverlay {
	user_prompt?: string;
	disabled_servers?: string[];
	disabled_tools?: string[];
	personal_skills?: string[];
}

/** `agent_config.session.set_user_prompt` request — session-safe (non-admin). */
export interface AgentConfigSessionSetUserPromptRequest {
	identity: IdentityScope;
	agent_id: string;
	user_prompt: string;
}

/** `agent_config.session.set_user_prompt` response. */
export interface AgentConfigSessionSetUserPromptResponse {
	overlay: AgentConfigSessionOverlay;
	protocol_version: string;
}

/** `agent_config.session.set_source_disables` request — narrow-only (non-admin). */
export interface AgentConfigSessionSetSourceDisablesRequest {
	identity: IdentityScope;
	agent_id: string;
	disabled_servers?: string[];
	disabled_tools?: string[];
}

/** `agent_config.session.set_source_disables` response. */
export interface AgentConfigSessionSetSourceDisablesResponse {
	overlay: AgentConfigSessionOverlay;
	protocol_version: string;
}

/** `agent_config.session.skills.list` request — session-safe (non-admin). */
export interface AgentConfigSessionSkillsListRequest {
	identity: IdentityScope;
	agent_id: string;
}

/** `agent_config.session.skills.list` response. */
export interface AgentConfigSessionSkillsListResponse {
	skills: AgentConfigSkillSummary[];
	protocol_version: string;
}

/** `agent_config.session.skills.upsert` request — session-safe (non-admin). */
export interface AgentConfigSessionSkillsUpsertRequest {
	identity: IdentityScope;
	agent_id: string;
	skill: AgentConfigSkillInput;
}

/** `agent_config.session.skills.upsert` response. */
export interface AgentConfigSessionSkillsUpsertResponse {
	skill: AgentConfigSkillSummary;
	overlay: AgentConfigSessionOverlay;
	protocol_version: string;
}

/** `agent_config.session.skills.delete` request — session-safe (non-admin). */
export interface AgentConfigSessionSkillsDeleteRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
}

/** `agent_config.session.skills.delete` response. */
export interface AgentConfigSessionSkillsDeleteResponse {
	overlay: AgentConfigSessionOverlay;
	protocol_version: string;
}
