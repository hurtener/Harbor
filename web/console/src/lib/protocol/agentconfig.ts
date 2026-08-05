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

/** An agent's MCP-exposure / per-tool policy in a config revision: the
 * exclusion-based paused MCP servers + individually-disabled tools, plus the
 * runtime loading-mode override maps (D-281). Pausing a server excludes its
 * tools from the next run (the live transport stays warm); tools are keyed
 * `<source>_<tool>`. server_loading_modes / tool_loading_modes values are
 * "always" | "deferred"; precedence: tool_loading_modes[name] >
 * server_loading_modes[source] (TOOL-form descriptors only) > the boot
 * config > the driver default. Mirrors `types.AgentConfigToolExposure`. */
export interface AgentConfigToolExposure {
	paused_servers?: string[];
	disabled_tools?: string[];
	server_loading_modes?: Record<string, string>;
	tool_loading_modes?: Record<string, string>;
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

/** One named, additive prompt block: the NAME a contributor addresses its own
 * contribution by (unique, `[A-Za-z0-9._-]{1,64}`, never emitted as an XML
 * tag) and the BODY that renders VERBATIM. Mirrors
 * `types.AgentConfigNamedBlock`. */
export interface AgentConfigNamedBlock {
	name: string;
	body: string;
}

/** An agent's ORDERED list of named, operator-authored additive prompt blocks,
 * so N independent capability sources can each contribute — and later remove —
 * exactly their own text.
 *
 * ORDER IS SEMANTIC: the declared array order IS the render order, it is part
 * of the revision's `content_hash`, and a pure re-ordering is a real new
 * revision the diff reports. (Contrast `skills.names` / `oauth_providers`,
 * whose orders are canonicalised by sorting.)
 *
 * Blocks render VERBATIM — unescaped — inside `<additional_guidance>`, each
 * behind a plain-text `[name]` label, and they survive a session
 * `system_prompt_override`. The section is written only by the ADMIN-tier
 * `agent_config.set_extra_system_blocks`; that authority tier, not an escaper,
 * is the boundary. A block MUST NOT carry user-authored or model-authored
 * text — use `start.caller_memory` or `prompt_layers.user` for that. Mirrors
 * `types.AgentConfigExtraSystemBlocks`. */
export interface AgentConfigExtraSystemBlocks {
	blocks: AgentConfigNamedBlock[];
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
 * (name, transport, stdio argv command or http URL, the non-secret OAuth
 * provider NAME, and non-secret operator `_meta` annotations). Secret auth
 * material is NEVER part of this descriptor. Mirrors
 * `types.AgentConfigMCPConnectionDescriptor`. */
export interface AgentConfigMCPConnectionDescriptor {
	name: string;
	/** "stdio" | "http". */
	transport: string;
	command?: string[];
	url?: string;
	/** Non-secret OAuth provider NAME bound for per-identity southbound bearer
	 * injection (http transport only; empty leaves the static headers). */
	oauth_provider?: string;
	/** Non-secret operator key/values merged verbatim into the MCP `_meta`. */
	meta_annotations?: Record<string, string>;
	/** Non-secret per-connection cross-origin allow-list of public https origins
	 * the OAuth-requirement discovery walker may fetch authorization-server
	 * metadata from. Revisioned + writable live over
	 * `agent_config.set_mcp_discovery_origins` (http transport only). */
	oauth_discovery_allowed_origins?: string[];
	/** Optional inline dev-gated OAuth binding — carries the FULL provider binding
	 * for this connection (mutually exclusive with `oauth_provider`). Accepted only
	 * behind the fail-closed `tools.allow_wire_oauth_descriptor` boot opt-in; the
	 * downstream sink is DERIVED from this connection's url (http transport only). */
	oauth?: AgentConfigOAuthProviderDescriptor;
	/** Optional dev-gated per-user credential-INJECTION mapping for a receiver-style
	 * MCP server — NAMES a boot-declared broker + a target header / basic / `_meta`
	 * key (mutually exclusive with `oauth_provider` / `oauth`). Accepted only behind
	 * the fail-closed `tools.allow_wire_injection` boot opt-in (independent of the
	 * wire-OAuth opt-in); the pulled credential's reachable sink is DERIVED from this
	 * connection's url and every target key must be redaction-covered (http transport
	 * only). */
	injection?: AgentConfigMCPCredentialInjectionDescriptor;
	/** Declares that this connection MAY receive artifact BYTES through egress
	 * substitution — the runtime resolving an artifact id the model authored and
	 * placing the resolved bytes into the outbound tool-call body, so a large
	 * document reaches the remote tool without transiting the model's context.
	 * Non-secret (a boolean). It is the containment boundary for the feature: with
	 * it unset, an `artifact_params` mapping is refused at the door. It widens the
	 * RECIPIENT, never the reachable artifact SET (that stays the dispatching run's
	 * own tenant/user/session). Every substitution is recorded fail-closed as
	 * `mcp.artifact_egressed` — ids, sizes and a digest, never the bytes (http
	 * transport only). */
	artifact_byte_eligible?: boolean;
	/** Maps this server's TOOL names to the parameter names on those tools which
	 * carry artifact bytes. Non-secret (names only). Requires
	 * `artifact_byte_eligible`; each mapped parameter is validated at attach against
	 * the server's own discovered `inputSchema` — declared, and declared
	 * string-typed (http transport only). */
	artifact_params?: Record<string, string[]>;
}

/** One runtime-added connection's per-user credential-INJECTION mapping for a
 * RECEIVER-STYLE MCP server — the NON-SECRET mapping only (NAMES a boot-declared
 * `tools.oauth_providers[]` broker + declares WHERE the pulled value is placed on
 * the outbound request). No secret rides this descriptor; only the broker-pulled
 * value (resolved per-call from the acting identity) is secret. Mirrors
 * `types.AgentConfigMCPCredentialInjectionDescriptor`. */
export interface AgentConfigMCPCredentialInjectionDescriptor {
	/** Boot-declared broker NAME the per-user credential is pulled from. */
	provider: string;
	/** "header" | "basic" | "meta". */
	form: string;
	/** Target request header NAME for form=header (redaction-covered; not
	 * `Authorization`). */
	header?: string;
	/** Username half for form=basic (the pulled credential is the password half). */
	basic_username?: string;
	/** Target `_meta` key PATH for form=meta (dot-separated; redaction-covered leaf;
	 * no reserved segment). */
	meta_key?: string;
}

/** The runtime-added MCP-connection section of the config envelope. Mirrors
 * `types.AgentConfigConnections`. */
export interface AgentConfigConnections {
	servers?: AgentConfigMCPConnectionDescriptor[];
}

/** A Protocol-installed OAuth provider. By default the name-only descriptor
 * (every credential sink pinned at boot on the named credential broker). Behind
 * the fail-closed `tools.allow_wire_oauth_descriptor` boot opt-in it MAY
 * additionally carry the NEW server's OAuth params over the wire (token_url /
 * audience / scopes) while STILL naming a boot-declared credential_broker; with
 * the opt-in off a descriptor carrying token_url or audience is rejected. The
 * runtime's own credential source (pull endpoint + env-var names) never rides the
 * wire. Mirrors `types.AgentConfigOAuthProviderDescriptor`. */
export interface AgentConfigOAuthProviderDescriptor {
	name: string;
	/** Exactly "tokenexchange" (the only installable driver). */
	driver: string;
	/** Exactly "remote" (broker-pull). */
	credential_source: string;
	/** Boot-declared credential broker name — pins the runtime's own credential
	 * custody. Required in BOTH the name-only and the dev-gated wire shape. */
	credential_broker: string;
	/** Requested scope subset (clamped to the broker's boot scope ceiling). */
	scopes?: string[];
	/** Dev-gated wire-carried token endpoint of the NEW server (a credential-sink
	 * field; rejected unless the opt-in is on; dialed through the SSRF backstop). */
	token_url?: string;
	/** Dev-gated wire-carried exchanged-token audience of the NEW server (rejected
	 * unless opted in). */
	audience?: string;
}

/** The Protocol-installed OAuth-provider section of the config envelope. Mirrors
 * `types.AgentConfigOAuthProviders`. */
export interface AgentConfigOAuthProviders {
	providers?: AgentConfigOAuthProviderDescriptor[];
}

/** The writable inference-provider binding — the D-303 zero-URL / zero-secret
 * shape. Carries NO URL and NO env-var name; every sink-determining value lives
 * on the boot-declared `inference_broker` it references by name. A write carrying
 * a `credential_url` / `token_url` / `*_env` / secret field is rejected by name at
 * the wire edge. Mirrors `types.AgentConfigLLMProviderDescriptor`. */
export interface AgentConfigLLMProviderDescriptor {
	name: string;
	/** The LLM provider the pulled key authenticates (e.g. "openai"). */
	provider: string;
	/** Exactly "remote" (broker-pull). */
	credential_source: string;
	/** Boot-declared inference broker name (pins every sink). Non-secret. */
	inference_broker: string;
	/** Optional model-name allowlist. Non-secret. */
	model_allow?: string[];
}

/** An agent's durable run-completion hook — the catalog tool the run
 * transcript is dispatched to at the run loop's terminal boundary, plus an
 * optional dispatch timeout (milliseconds). Mirrors
 * `types.AgentConfigRunCompletionHook`. */
export interface AgentConfigRunCompletionHook {
	tool: string;
	/** Dispatch timeout in milliseconds; 0 inherits the runtime default. */
	timeout_ms?: number;
}

/** The run-lifecycle-hook section of the config envelope. Mirrors
 * `types.AgentConfigHooks`. */
export interface AgentConfigHooks {
	run_completion?: AgentConfigRunCompletionHook;
}

/** The session auto-naming policy section of the config envelope — opt-in,
 * default off. An ABSENT section falls through to the yaml `runtime.naming`
 * fleet default; a PRESENT section is authoritative either way (a bare
 * `{auto: false}` is an explicit opt-out overriding a yaml-on default).
 * `after_turns` / `max_title_len` inherit the runtime defaults
 * (1, 80) when zero; `max_repetitions` is required >= 1 when `repeat_every` > 0
 * (no unlimited value). Mirrors `types.AgentConfigNaming`. */
export interface AgentConfigNaming {
	auto?: boolean;
	after_turns?: number;
	repeat_every?: number;
	max_repetitions?: number;
	max_title_len?: number;
	model?: string;
}

/** Closed MCP descriptor accepted only by D-401 signed capability registration.
 * It intentionally has no generic transport, OAuth, header, injection, stdio,
 * credential, secret, or host-list fields. */
export interface SignedOAuthMCPConnectionDescriptor {
	name: string;
	url: string;
	tool_allowlist?: string[];
	tool_denylist?: string[];
	connect_timeout_ms?: number;
	request_timeout_ms?: number;
	injection?: AgentConfigMCPCredentialInjectionDescriptor;
	artifact_byte_eligible?: boolean;
	artifact_params?: Record<string, string[]>;
}

/** Read-only immutable signed OAuth MCP pair projected from a revision. The
 * authority envelope and raw JTI are intentionally never exposed. */
export interface AgentConfigSignedOAuthMCPPair {
	provider_name: string;
	broker: string;
	audience: string;
	scopes: string[];
	capability_revision: string;
	url_digest: string;
	sink: string;
	connection: SignedOAuthMCPConnectionDescriptor;
	authority_issuer: string;
	authority_key_id: string;
	authority_jti_hash: string;
}

/** An agent-config envelope — every section optional and forward-compatible.
 * Mirrors `types.AgentConfigPayload`. */
export interface AgentConfigPayload {
	prompt_layers?: AgentConfigPromptLayers;
	skills?: AgentConfigSkillsSelection;
	tool_exposure?: AgentConfigToolExposure;
	connections?: AgentConfigConnections;
	oauth_providers?: AgentConfigOAuthProviders;
	llm_params?: AgentConfigLLMParams;
	hooks?: AgentConfigHooks;
	naming?: AgentConfigNaming;
	extra_system_blocks?: AgentConfigExtraSystemBlocks;
	/** Read-only; generic set_revision must not author or clear this field. */
	signed_oauth_mcp_pair?: AgentConfigSignedOAuthMCPPair;
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

/** One loading-mode override delta entry: the override at `key` changed from
 * `from` to `to` (empty = unset). Mirrors `types.AgentConfigLoadingModeChange`. */
export interface AgentConfigLoadingModeChange {
	key: string;
	from?: string;
	to?: string;
}

/** The structured MCP-exposure / per-tool policy set-diff across two
 * revisions. Mirrors `types.AgentConfigToolExposureDiff`. */
export interface AgentConfigToolExposureDiff {
	paused_added?: string[];
	paused_resumed?: string[];
	disabled_added?: string[];
	disabled_enabled?: string[];
	server_loading_changes?: AgentConfigLoadingModeChange[];
	tool_loading_changes?: AgentConfigLoadingModeChange[];
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

/** The structured Protocol-installed OAuth-provider set-diff (by name) across
 * two revisions. Mirrors `types.AgentConfigOAuthProvidersDiff`. */
export interface AgentConfigOAuthProvidersDiff {
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

/** The run-lifecycle-hook per-field delta across two revisions.
 * `section_present_from` / `section_present_to` render the hooks-section
 * PRESENCE ("present" / "" when absent) — a present bare `{}` hooks section
 * is an explicit per-agent no-hook that overrides a yaml fleet hook, and the
 * presence dimension makes exactly that revision visible (absent → "present"
 * with empty tool/timeout). Mirrors `types.AgentConfigHooksDiff`. */
export interface AgentConfigHooksDiff {
	section_present_changed: boolean;
	section_present_from?: string;
	section_present_to?: string;
	run_completion_tool_changed: boolean;
	run_completion_tool_from?: string;
	run_completion_tool_to?: string;
	run_completion_timeout_changed: boolean;
	run_completion_timeout_from?: string;
	run_completion_timeout_to?: string;
}

/** The session auto-naming policy per-field delta across two revisions.
 * `auto_from` / `auto_to` are TRI-STATE display strings — "" (section absent)
 * / "false" / "true" — so a bare `{auto: false}` opt-out revision registers
 * as a change against an absent section. Mirrors
 * `types.AgentConfigNamingDiff`. */
export interface AgentConfigNamingDiff {
	auto_changed: boolean;
	auto_from?: string;
	auto_to?: string;
	after_turns_changed: boolean;
	after_turns_from?: string;
	after_turns_to?: string;
	repeat_every_changed: boolean;
	repeat_every_from?: string;
	repeat_every_to?: string;
	max_repetitions_changed: boolean;
	max_repetitions_from?: string;
	max_repetitions_to?: string;
	max_title_len_changed: boolean;
	max_title_len_from?: string;
	max_title_len_to?: string;
	model_changed: boolean;
	model_from?: string;
	model_to?: string;
}

/** The structured additive-prompt-block delta across two revisions. `added` /
 * `removed` / `changed` are block NAMES (sorted); `reordered` reports the SAME
 * name set in a DIFFERENT order — a real prompt change with no analogue on the
 * sorted sibling sections. Mirrors `types.AgentConfigExtraSystemBlocksDiff`. */
export interface AgentConfigExtraSystemBlocksDiff {
	added?: string[];
	removed?: string[];
	changed?: string[];
	reordered: boolean;
}

/** A server-side revision compare. Mirrors `types.AgentConfigDiff`. */
export interface AgentConfigDiff {
	from_revision_id: string;
	to_revision_id: string;
	skills: AgentConfigSkillsDiff;
	tool_exposure: AgentConfigToolExposureDiff;
	prompt_layers: AgentConfigPromptLayersDiff;
	connections: AgentConfigConnectionsDiff;
	oauth_providers: AgentConfigOAuthProvidersDiff;
	llm_params: AgentConfigLLMParamsDiff;
	hooks: AgentConfigHooksDiff;
	naming: AgentConfigNamingDiff;
	extra_system_blocks: AgentConfigExtraSystemBlocksDiff;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.rollback` response. */
export interface AgentConfigRollbackResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** One bounded, non-secret retirement cleanup entry. */
export interface AgentConfigRetirementCleanupStep {
	class: string;
	resource: string;
	completed: boolean;
}

/** Durable terminal lifecycle state for one agent configuration. */
export interface AgentConfigRetirementStatus {
	operation_id: string;
	retired_at: string;
	prior_revision_id?: string;
	prior_content_hash?: string;
	generation: number;
	cleanup?: AgentConfigRetirementCleanupStep[];
	completed: boolean;
}

/** `agent_config.retire` request — admin control-plane authority. */
export interface AgentConfigRetireRequest {
	identity: IdentityScope;
	agent_id: string;
	operation_id: string;
	expected_content_hash: string;
}

/** `agent_config.retire` response. */
export interface AgentConfigRetireResponse {
	status: AgentConfigRetirementStatus;
	protocol_version: string;
}

/** `agent_config.set_tool_exposure` request — admin-scoped. Replaces ONLY
 * the tool-exposure section (the skills + prompt sections are preserved). */
export interface AgentConfigSetToolExposureRequest {
	identity: IdentityScope;
	agent_id: string;
	tool_exposure: AgentConfigToolExposure;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.set_prompt_layers` response — the recorded revision. */
export interface AgentConfigSetPromptLayersResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.set_extra_system_blocks` request — admin-scoped. Replaces
 * ONLY the additive-prompt-blocks section as a WHOLE-SECTION desired state
 * (every sibling section is preserved); an empty list clears it. There are
 * deliberately no per-block verbs — a block is one element of an ORDERED
 * composition whose order is semantic, so a per-item upsert has no
 * well-defined insertion position. A second contributor composes by
 * read-modify-write: `agent_config.get`, append or drop its own block by NAME,
 * write back with the read revision's `content_hash` as
 * `expected_content_hash`. Names must be unique and match
 * `[A-Za-z0-9._-]{1,64}`; a duplicate or malformed name is refused with
 * `invalid_request` and nothing is persisted. */
export interface AgentConfigSetExtraSystemBlocksRequest {
	identity: IdentityScope;
	agent_id: string;
	extra_system_blocks: AgentConfigExtraSystemBlocks;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * Sending it is how two independent contributors compose safely: the
	 * read-modify-write is only as safe as the token. The refusal is exact
	 * within one Runtime process; it is not a cross-process
	 * compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.set_extra_system_blocks` response — the recorded revision. */
export interface AgentConfigSetExtraSystemBlocksResponse {
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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

/** `agent_config.remove_mcp_connection` request — admin-scoped. Removes a
 * runtime-added MCP server connection by name as a new revision (dropping the
 * descriptor + pruning that server's tool-exposure residue, carrying every
 * sibling section forward). An unknown name and a boot-declared (yaml) name
 * each fail loud with a distinct typed error. The physical teardown happens at
 * the next run's projection boundary. Mirrors
 * `types.AgentConfigRemoveMCPConnectionRequest`. */
export interface AgentConfigRemoveMCPConnectionRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.remove_mcp_connection` response — the recorded removing
 * revision and the removed name. Mirrors
 * `types.AgentConfigRemoveMCPConnectionResponse`. */
export interface AgentConfigRemoveMCPConnectionResponse {
	revision: AgentConfigRevisionView;
	name: string;
	protocol_version: string;
}

/** `agent_config.set_mcp_discovery_origins` request — admin-scoped. FULL-REPLACE
 * writes a runtime-added MCP connection's OAuth-discovery cross-origin
 * allow-list (records a revision AND applies it live). Origins are validated
 * https origins (no path/IP-literal). An unknown name, a boot-declared (yaml)
 * name, and a stdio connection each fail loud with a distinct typed error.
 * Mirrors `types.AgentConfigSetMCPDiscoveryOriginsRequest`. */
export interface AgentConfigSetMCPDiscoveryOriginsRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
	allowed_origins: string[];
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.set_mcp_discovery_origins` response — the recorded revision,
 * the granted / revoked origin deltas, and whether the write applied to the live
 * registry. Mirrors `types.AgentConfigSetMCPDiscoveryOriginsResponse`. */
export interface AgentConfigSetMCPDiscoveryOriginsResponse {
	revision: AgentConfigRevisionView;
	name: string;
	granted?: string[];
	revoked?: string[];
	applied_live: boolean;
	protocol_version: string;
}

/** `agent_config.set_oauth_provider` request — admin-scoped. Install (upsert) a
 * ZERO-URL, broker-pull OAuth provider. The descriptor carries NO URL, NO env-var
 * name, NO secret; a write carrying `token_url` / `auth_url` / `client_id_env` /
 * `client_secret_env` / `remote` is rejected by name at the wire edge. Mirrors
 * `types.AgentConfigSetOAuthProviderRequest`. */
export interface AgentConfigSetOAuthProviderRequest {
	identity: IdentityScope;
	agent_id: string;
	provider: AgentConfigOAuthProviderDescriptor;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.set_oauth_provider` response — the recorded revision and the
 * installed provider name. Mirrors `types.AgentConfigSetOAuthProviderResponse`. */
export interface AgentConfigSetOAuthProviderResponse {
	revision: AgentConfigRevisionView;
	name: string;
	protocol_version: string;
}

/** D-401's sole production creator for an OAuth provider + MCP connection
 * pair. The signed authority envelope is write-only and is never echoed. */
export interface AgentConfigRegisterOAuthMCPCapabilityRequest {
	identity: IdentityScope;
	agent_id: string;
	provider_name: string;
	broker: string;
	audience: string;
	scopes: string[];
	connection: SignedOAuthMCPConnectionDescriptor;
	expected_content_hash?: string;
	authority_envelope: string;
}

export interface AgentConfigRegisterOAuthMCPCapabilityResponse {
	revision: AgentConfigRevisionView;
	provider_name: string;
	connection_name: string;
	protocol_version: string;
}

/** D-401's sole removal verb for a server-owned signed OAuth MCP pair. The
 * mandatory content hash names the exact immutable pair revision whose frozen
 * durable receipt is resumed, so a delayed request cannot remove a replacement. */
export interface AgentConfigRemoveOAuthMCPCapabilityRequest {
	identity: IdentityScope;
	agent_id: string;
	expected_content_hash: string;
}

export interface AgentConfigRemoveOAuthMCPCapabilityResponse {
	revision: AgentConfigRevisionView;
	provider_name: string;
	connection_name: string;
	operation_phase: string;
	protocol_version: string;
}

/** `agent_config.set_llm_provider` request — admin-scoped. Install (upsert) /
 * rotate a ZERO-URL, broker-pull inference provider binding. A separate method
 * from set_oauth_provider (a distinct credential plane); the descriptor carries
 * NO URL, NO env-var name, NO secret. Mirrors
 * `types.AgentConfigSetLLMProviderRequest`. */
export interface AgentConfigSetLLMProviderRequest {
	identity: IdentityScope;
	agent_id: string;
	provider: AgentConfigLLMProviderDescriptor;
}

/** `agent_config.set_llm_provider` response — the installed binding name and
 * whether it took effect on the live provider set. Mirrors
 * `types.AgentConfigSetLLMProviderResponse`. */
export interface AgentConfigSetLLMProviderResponse {
	name: string;
	installed: boolean;
	protocol_version: string;
}

/** `agent_config.remove_oauth_provider` request — admin-scoped. Uninstall a
 * Protocol-installed OAuth provider by name; the live uninstall CLOSES it, so a
 * still-bound connection's next call fails loud. Mirrors
 * `types.AgentConfigRemoveOAuthProviderRequest`. */
export interface AgentConfigRemoveOAuthProviderRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.remove_oauth_provider` response — the recorded revision, the
 * removed provider name, and whether the live uninstall took effect. Mirrors
 * `types.AgentConfigRemoveOAuthProviderResponse`. */
export interface AgentConfigRemoveOAuthProviderResponse {
	revision: AgentConfigRevisionView;
	name: string;
	uninstalled: boolean;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
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

// --- User-scope durable config variant (the middle tier of the
// authorization matrix). A caller carrying the `agent_config:user` scope owns
// a DURABLE, versioned safe-subset config variant keyed under their real
// (tenant, user), with full diff/rollback. The payload is structurally bounded
// (user prompt + narrow-only disables + personal skills) — NO base /
// connections / enable / model field, so a user caller cannot widen. The
// responses REUSE AgentConfigRevisionView / AgentConfigDiff. ---

/** The bounded safe-subset a user-scope revision persists.
 * Mirrors `types.AgentConfigUserPayload`. */
export interface AgentConfigUserPayload {
	user_prompt?: string;
	disabled_servers?: string[];
	disabled_tools?: string[];
	personal_skills?: string[];
}

/** `agent_config.user.get` request — read the caller's own durable variant. */
export interface AgentConfigUserGetRequest {
	identity: IdentityScope;
	agent_id: string;
}

/** `agent_config.user.get` response. */
export interface AgentConfigUserGetResponse {
	revision?: AgentConfigRevisionView;
	set: boolean;
	protocol_version: string;
}

/** `agent_config.user.set_revision` request — write a new revision of the
 * caller's durable variant (user-tier; requires the `agent_config:user` scope). */
export interface AgentConfigUserSetRevisionRequest {
	identity: IdentityScope;
	agent_id: string;
	payload: AgentConfigUserPayload;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.user.set_revision` response. */
export interface AgentConfigUserSetRevisionResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.user.list_revisions` request. */
export interface AgentConfigUserListRevisionsRequest {
	identity: IdentityScope;
	agent_id: string;
	limit?: number;
}

/** `agent_config.user.list_revisions` response. */
export interface AgentConfigUserListRevisionsResponse {
	revisions: AgentConfigRevisionView[];
	protocol_version: string;
}

/** `agent_config.user.diff` request. */
export interface AgentConfigUserDiffRequest {
	identity: IdentityScope;
	agent_id: string;
	from_revision: string;
	to_revision: string;
}

/** `agent_config.user.diff` response. */
export interface AgentConfigUserDiffResponse {
	diff: AgentConfigDiff;
	protocol_version: string;
}

/** `agent_config.user.rollback` request. */
export interface AgentConfigUserRollbackRequest {
	identity: IdentityScope;
	agent_id: string;
	revision_id: string;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.user.rollback` response. */
export interface AgentConfigUserRollbackResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

// --- Durable-per-user skills (CLAIM-FREE). `user` names the durable STORAGE
// scope, not an auth tier: these verbs need only a valid identity (NOT admin,
// NOT the `agent_config:user` scope) because a personal skill cannot widen
// capability. The upsert/delete responses REUSE AgentConfigRevisionView (the
// recorded durable membership revision). ---

/** `agent_config.user.skills.list` request — CLAIM-FREE. */
export interface AgentConfigUserSkillsListRequest {
	identity: IdentityScope;
	agent_id: string;
}

/** `agent_config.user.skills.list` response. */
export interface AgentConfigUserSkillsListResponse {
	skills: AgentConfigSkillSummary[];
	protocol_version: string;
}

/** `agent_config.user.skills.upsert` request — CLAIM-FREE. Upserts a durable
 * personal skill (scope forced to `user` server-side). */
export interface AgentConfigUserSkillsUpsertRequest {
	identity: IdentityScope;
	agent_id: string;
	skill: AgentConfigSkillInput;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.user.skills.upsert` response. */
export interface AgentConfigUserSkillsUpsertResponse {
	skill: AgentConfigSkillSummary;
	revision: AgentConfigRevisionView;
	protocol_version: string;
}

/** `agent_config.user.skills.delete` request — CLAIM-FREE. */
export interface AgentConfigUserSkillsDeleteRequest {
	identity: IdentityScope;
	agent_id: string;
	name: string;
	/**
	 * OPTIONAL expected-revision token. When set, the write requires the
	 * agent's ACTIVE revision to still carry exactly this `content_hash` (as
	 * returned by `agent_config.get`); a moved base is refused with the
	 * `revision_conflict` error code (HTTP 409) and nothing is persisted.
	 * Omitted is the unconditional last-writer-wins write.
	 *
	 * The refusal is exact within one Runtime process; it is not a
	 * cross-process compare-and-swap.
	 */
	expected_content_hash?: string;
}

/** `agent_config.user.skills.delete` response. */
export interface AgentConfigUserSkillsDeleteResponse {
	revision: AgentConfigRevisionView;
	protocol_version: string;
}
