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
// / per-tool policy in a config revision: the exclusion-based pause/disable
// sets (paused MCP servers, individually-disabled tools) plus the runtime
// loading-mode override maps. Pausing a server excludes its tools
// from the next run's projection (the live transport stays warm); disabling
// a tool excludes that one tool. Tools are keyed `<source>_<tool>`; a
// server's tools share the source id.
//
// ServerLoadingModes / ToolLoadingModes values are the closed set
// "always" | "deferred" — `agent_config.set_tool_exposure` rejects any
// other value with `invalid_request` (400) BEFORE recording a revision.
// Precedence: ToolLoadingModes[name] > ServerLoadingModes[source]
// (TOOL-form descriptors only) > the boot config > the driver default.
type AgentConfigToolExposure struct {
	// PausedServers names the MCP source ids excluded from the next run's
	// projection (resume is a flag flip, not a re-dial).
	PausedServers []string `json:"paused_servers,omitempty"`
	// DisabledTools names the individually-disabled tools (`<source>_<tool>`).
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// ServerLoadingModes overrides the loading mode for a server's TOOL-form
	// descriptors, keyed by MCP source id.
	ServerLoadingModes map[string]string `json:"server_loading_modes,omitempty"`
	// ToolLoadingModes overrides the loading mode for one exact catalog
	// name, unconditionally.
	ToolLoadingModes map[string]string `json:"tool_loading_modes,omitempty"`
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

// AgentConfigLLMParams is the wire projection of an agent's per-agent
// LLM-parameter section in a config revision: the sampling defaults pinned
// for the agent (model / temperature / max-tokens / reasoning-effort).
// Every field is pointer-optional — an unset field falls through to the
// tenant-wide baseline, then the config default. Sampling parameters only;
// additive system-prompt text lives in AgentConfigPromptLayers.
type AgentConfigLLMParams struct {
	// Model, when set, is the model the agent's next run requests
	// (overriding the tenant-wide baseline / config default).
	Model *string `json:"model,omitempty"`
	// Temperature, when set, is the sampling temperature for the next run.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens, when set, is the per-message output-token ceiling.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// ReasoningEffort, when set, is the reasoning-effort hint
	// ("off" | "low" | "medium" | "high").
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// AgentConfigMCPConnectionDescriptor is the wire projection of one
// runtime-added MCP server connection — the NON-SECRET descriptor only
// (name, transport, stdio argv command or http URL, the non-secret OAuth
// provider NAME, and the non-secret operator `_meta` annotations). Secret
// auth material (bearer headers, OAuth tokens, credentials, the minted
// downstream token) is NEVER part of this descriptor: it flows through the
// live attach + the tool-side OAuth / pause-resume path and is never
// persisted in a revision, diff, or event. The oauth_provider field is a
// NAME, not a secret — it selects a config-declared acquisition strategy.
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
	// OAuthProvider names a declared OAuth provider to bind for per-identity
	// southbound bearer injection. NON-SECRET (a provider name). Set only for
	// the http transport; empty leaves the connection on its static headers.
	OAuthProvider string `json:"oauth_provider,omitempty"`
	// MetaAnnotations is a static, NON-SECRET set of operator key/values
	// merged verbatim into the MCP `_meta` on every identity-stamped call.
	// Reserved / spec-prefixed keys are rejected at attach.
	MetaAnnotations map[string]string `json:"meta_annotations,omitempty"`
	// OAuthDiscoveryAllowedOrigins is the explicit per-connection cross-origin
	// allow-list of public https origins the OAuth-requirement discovery walker
	// may fetch authorization-server metadata from. NON-SECRET (an origin
	// allow-list) — revisioned + diffable + rollback-able, and writable live
	// over `agent_config.set_mcp_discovery_origins`. Empty leaves the
	// authorization-server hop needs-allowance (partial discovery), never a
	// network hole (a granted origin is still refused at dial if it resolves
	// private / loopback). Set only for the http transport.
	OAuthDiscoveryAllowedOrigins []string `json:"oauth_discovery_allowed_origins,omitempty"`
}

// AgentConfigConnections is the wire projection of the runtime-added
// MCP-connection section of the config envelope — the set of NON-SECRET
// connection descriptors recorded in a revision (part of the agent's
// versioned desired state for diff / rollback).
type AgentConfigConnections struct {
	// Servers is the set of runtime-added MCP connection descriptors.
	Servers []AgentConfigMCPConnectionDescriptor `json:"servers,omitempty"`
}

// AgentConfigOAuthProviderDescriptor is the wire projection of one
// Protocol-installed OAuth provider — the ZERO-URL descriptor only. It carries
// NO URL of any kind, NO env-var name, and NO literal secret: the
// credential-plane invariant pins every credential-sink-determining
// value (token endpoint, credential-pull endpoint, allowed downstream hosts,
// audience, scope ceiling) at boot on the NAMED credential broker the
// descriptor references by non-secret name. Because the forbidden fields
// (`token_url`, `auth_url`, `client_id_env`, `client_secret_env`, `remote`) are
// simply NOT on this struct, a `DisallowUnknownFields` decode rejects any of
// them BY NAME (`json: unknown field "client_secret_env"`) — a loud reject with
// no decoy fields (the field cannot exist).
type AgentConfigOAuthProviderDescriptor struct {
	// Name is the unique provider name (a connection's oauth_provider binding
	// references it). Required.
	Name string `json:"name"`
	// Driver is the OAuth flow driver — validated to be exactly "tokenexchange"
	// (the only writable driver; the non-interactive PULL exchange). Required.
	Driver string `json:"driver"`
	// CredentialSource is the credential-source seam — validated to be exactly
	// "remote" (broker-pull). An empty value is a LOUD reject. Required.
	CredentialSource string `json:"credential_source"`
	// CredentialBroker names a boot-declared credential broker that pins every
	// credential sink. Required; an unknown name fails loud. NON-SECRET.
	CredentialBroker string `json:"credential_broker"`
	// Scopes is the requested OAuth scope subset. NON-SECRET; clamped to the
	// broker's boot scope ceiling at build time. Optional.
	Scopes []string `json:"scopes,omitempty"`
}

// AgentConfigOAuthProviders is the wire projection of the Protocol-installed
// OAuth-provider section of the config envelope — the set of ZERO-URL provider
// descriptors recorded in a revision (part of the agent's versioned desired
// state for diff / rollback).
type AgentConfigOAuthProviders struct {
	// Providers is the set of installed provider descriptors.
	Providers []AgentConfigOAuthProviderDescriptor `json:"providers,omitempty"`
}

// AgentConfigOAuthProvidersDiff is the wire projection of the structured
// installed-provider set-diff (by name) across two revisions.
type AgentConfigOAuthProvidersDiff struct {
	// Added are the provider names present in the to-revision but not the
	// from-revision.
	Added []string `json:"added,omitempty"`
	// Removed are the provider names present in the from-revision but not the
	// to-revision.
	Removed []string `json:"removed,omitempty"`
}

// AgentConfigRunCompletionHook is the wire projection of an agent's durable
// run-completion hook in a config revision: the catalog tool the run
// transcript is dispatched to at the run loop's terminal boundary, plus an
// optional dispatch timeout (milliseconds). Mirrors the static
// `runtime.hooks.run_completion` yaml; resolution at run start is
// agent-config over yaml over no hook.
type AgentConfigRunCompletionHook struct {
	// Tool is the catalog tool name the transcript is dispatched to.
	Tool string `json:"tool"`
	// TimeoutMS is the dispatch timeout in milliseconds. Zero inherits the
	// runtime default at run start.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// AgentConfigHooks is the wire projection of the run-lifecycle-hook section
// of the config envelope. Declared as its own section so a set REPLACES only
// this section, preserving the sibling sections.
type AgentConfigHooks struct {
	// RunCompletion, when non-nil, pins the agent's run-completion hook.
	RunCompletion *AgentConfigRunCompletionHook `json:"run_completion,omitempty"`
}

// AgentConfigNaming is the wire projection of the session auto-naming policy
// section of the config envelope. Declared as its own section so a set
// REPLACES only this section, preserving the siblings. Opt-in, default off: an
// ABSENT section means "no per-agent policy" (the yaml `runtime.naming` fleet
// default, then off, resolves at run start). A PRESENT section is
// authoritative either way — `auto: true` enables, and a bare `{auto: false}`
// is an explicit per-agent OPT-OUT that overrides a yaml-on fleet default
// (section presence is the signal; a present section is preserved verbatim
// through normalization, never dropped as inert).
//
// `after_turns` and `max_title_len` resolve to their runtime defaults (1, 80)
// when zero; `max_repetitions` is REQUIRED ≥ 1 whenever `repeat_every` > 0
// (`agent_config.set_revision` rejects a repeating policy with no cap —
// `invalid_request` 400 — so unbounded periodic re-naming is unrepresentable);
// a set `model` is validated against the configured ModelProfiles.
type AgentConfigNaming struct {
	// Auto enables session auto-naming for the agent (false in a present
	// section = an explicit off that overrides the yaml fleet default).
	Auto bool `json:"auto,omitempty"`
	// AfterTurns is the completed-run count at which the first auto-name
	// fires; 0 inherits the runtime default (1).
	AfterTurns int `json:"after_turns,omitempty"`
	// RepeatEvery, when > 0, re-names every N completed turns after the
	// first; 0 names once only.
	RepeatEvery int `json:"repeat_every,omitempty"`
	// MaxRepetitions caps the TOTAL auto-namings (including the first);
	// required ≥ 1 when RepeatEvery > 0. Ignored when RepeatEvery == 0.
	MaxRepetitions int `json:"max_repetitions,omitempty"`
	// MaxTitleLen bounds the auto title in runes; 0 inherits the runtime
	// default (80). A set value must be in [8, 200].
	MaxTitleLen int `json:"max_title_len,omitempty"`
	// Model, when set, is the model the auto-naming call requests (empty =
	// the run's effective model).
	Model string `json:"model,omitempty"`
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
	// OAuthProviders, when non-nil, pins the agent's Protocol-installed
	// (zero-URL) OAuth provider descriptors for the revision.
	OAuthProviders *AgentConfigOAuthProviders `json:"oauth_providers,omitempty"`
	// LLMParams, when non-nil, pins the agent's per-agent LLM-parameter
	// section (model / temperature / max-tokens / reasoning-effort) for the
	// revision.
	LLMParams *AgentConfigLLMParams `json:"llm_params,omitempty"`
	// Hooks, when non-nil, pins the agent's run-lifecycle-hook section (the
	// run-completion hook) for the revision.
	Hooks *AgentConfigHooks `json:"hooks,omitempty"`
	// Naming, when non-nil, pins the agent's session auto-naming policy
	// section for the revision.
	Naming *AgentConfigNaming `json:"naming,omitempty"`
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
	// ServerLoadingChanges / ToolLoadingChanges are the structured deltas of
	// the per-server / per-tool loading-mode override maps.
	ServerLoadingChanges []AgentConfigLoadingModeChange `json:"server_loading_changes,omitempty"`
	ToolLoadingChanges   []AgentConfigLoadingModeChange `json:"tool_loading_changes,omitempty"`
}

// AgentConfigLoadingModeChange is the wire projection of one loading-mode
// override delta entry: the override at Key (a catalog name for a
// ToolLoadingChanges entry, an MCP source id for a ServerLoadingChanges
// entry) changed from From to To. An empty From/To means the override was
// absent (unset) on that side of the compare. Mirrors `agentcfg.LoadingModeChange`.
type AgentConfigLoadingModeChange struct {
	Key  string `json:"key"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
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

// AgentConfigLLMParamsDiff is the wire projection of the per-agent
// LLM-parameter per-field delta across two revisions. Each dimension
// reports whether it changed plus its from / to display values (an unset
// dimension is the empty string).
type AgentConfigLLMParamsDiff struct {
	ModelChanged bool   `json:"model_changed"`
	ModelFrom    string `json:"model_from,omitempty"`
	ModelTo      string `json:"model_to,omitempty"`

	TemperatureChanged bool   `json:"temperature_changed"`
	TemperatureFrom    string `json:"temperature_from,omitempty"`
	TemperatureTo      string `json:"temperature_to,omitempty"`

	MaxTokensChanged bool   `json:"max_tokens_changed"`
	MaxTokensFrom    string `json:"max_tokens_from,omitempty"`
	MaxTokensTo      string `json:"max_tokens_to,omitempty"`

	ReasoningEffortChanged bool   `json:"reasoning_effort_changed"`
	ReasoningEffortFrom    string `json:"reasoning_effort_from,omitempty"`
	ReasoningEffortTo      string `json:"reasoning_effort_to,omitempty"`
}

// AgentConfigHooksDiff is the wire projection of the run-lifecycle-hook
// per-field delta across two revisions. Each dimension reports whether it
// changed plus its from / to display values (an unset hook is the empty
// string).
//
// `section_present_from` / `section_present_to` render the hooks-section
// PRESENCE ("present" when the section exists — with or without a
// run-completion tool — "" when absent), because presence is semantic: a
// present bare `{}` hooks section is an explicit per-agent no-hook that
// overrides a yaml fleet hook, and the diff must show exactly that revision
// (absent → "present" with empty tool/timeout) rather than rendering it as
// no change. Mirrors the naming diff's tri-state `auto` precedent. Additive
// fields — the Protocol version holds.
type AgentConfigHooksDiff struct {
	SectionPresentChanged bool   `json:"section_present_changed"`
	SectionPresentFrom    string `json:"section_present_from,omitempty"`
	SectionPresentTo      string `json:"section_present_to,omitempty"`

	RunCompletionToolChanged bool   `json:"run_completion_tool_changed"`
	RunCompletionToolFrom    string `json:"run_completion_tool_from,omitempty"`
	RunCompletionToolTo      string `json:"run_completion_tool_to,omitempty"`

	RunCompletionTimeoutChanged bool   `json:"run_completion_timeout_changed"`
	RunCompletionTimeoutFrom    string `json:"run_completion_timeout_from,omitempty"`
	RunCompletionTimeoutTo      string `json:"run_completion_timeout_to,omitempty"`
}

// AgentConfigNamingDiff is the wire projection of the session auto-naming
// policy per-field delta across two revisions. Each dimension reports whether
// it changed plus its from / to display values (an ABSENT section renders
// every dimension as the empty string).
//
// `auto_from` / `auto_to` are TRI-STATE display strings — "" (section
// absent) / "false" / "true" — because section presence is semantic: a
// present bare `{auto: false}` section is an explicit per-agent opt-out that
// overrides a yaml-on fleet default, and the diff must show exactly that
// revision (absent → "false") rather than rendering it as no change.
type AgentConfigNamingDiff struct {
	AutoChanged bool   `json:"auto_changed"`
	AutoFrom    string `json:"auto_from,omitempty"`
	AutoTo      string `json:"auto_to,omitempty"`

	AfterTurnsChanged bool   `json:"after_turns_changed"`
	AfterTurnsFrom    string `json:"after_turns_from,omitempty"`
	AfterTurnsTo      string `json:"after_turns_to,omitempty"`

	RepeatEveryChanged bool   `json:"repeat_every_changed"`
	RepeatEveryFrom    string `json:"repeat_every_from,omitempty"`
	RepeatEveryTo      string `json:"repeat_every_to,omitempty"`

	MaxRepetitionsChanged bool   `json:"max_repetitions_changed"`
	MaxRepetitionsFrom    string `json:"max_repetitions_from,omitempty"`
	MaxRepetitionsTo      string `json:"max_repetitions_to,omitempty"`

	MaxTitleLenChanged bool   `json:"max_title_len_changed"`
	MaxTitleLenFrom    string `json:"max_title_len_from,omitempty"`
	MaxTitleLenTo      string `json:"max_title_len_to,omitempty"`

	ModelChanged bool   `json:"model_changed"`
	ModelFrom    string `json:"model_from,omitempty"`
	ModelTo      string `json:"model_to,omitempty"`
}

// AgentConfigDiff is the wire projection of a server-side revision
// compare — the structured skills + tool-exposure + connection set-diffs,
// the prompt-layer text delta, the per-agent LLM-parameter delta, the
// run-lifecycle-hook delta, and the session auto-naming policy delta.
type AgentConfigDiff struct {
	FromRevisionID string                        `json:"from_revision_id"`
	ToRevisionID   string                        `json:"to_revision_id"`
	Skills         AgentConfigSkillsDiff         `json:"skills"`
	ToolExposure   AgentConfigToolExposureDiff   `json:"tool_exposure"`
	PromptLayers   AgentConfigPromptLayersDiff   `json:"prompt_layers"`
	Connections    AgentConfigConnectionsDiff    `json:"connections"`
	OAuthProviders AgentConfigOAuthProvidersDiff `json:"oauth_providers"`
	LLMParams      AgentConfigLLMParamsDiff      `json:"llm_params"`
	Hooks          AgentConfigHooksDiff          `json:"hooks"`
	Naming         AgentConfigNamingDiff         `json:"naming"`
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

// AgentConfigSetLLMParamsRequest is the admin-scoped
// `agent_config.set_llm_params` request — set the agent's per-agent
// LLM-parameter section (model / temperature / max-tokens / reasoning-effort)
// as a desired-state REPLACE of the LLM-params section (the prompt-layer +
// skills + tool-exposure + connection sections of the active revision are
// preserved). A set Model is validated against the configured ModelProfiles
// at set time (an unknown model is rejected). The edit records a new config
// revision and applies to the agent's NEXT run.
type AgentConfigSetLLMParamsRequest struct {
	Identity  IdentityScope        `json:"identity"`
	AgentID   string               `json:"agent_id"`
	LLMParams AgentConfigLLMParams `json:"llm_params"`
}

// AgentConfigSetLLMParamsResponse is the `agent_config.set_llm_params`
// response — the recorded config revision (or, on an idempotent re-set, the
// existing active revision).
type AgentConfigSetLLMParamsResponse struct {
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

// AgentConfigRemoveMCPConnectionRequest is the admin-scoped
// `agent_config.remove_mcp_connection` request — remove a runtime-added MCP
// server connection by name. The verb governs REVISIONED state only: it
// records a new revision that drops the named descriptor AND prunes that
// server's tool-exposure residue (its paused/disabled/loading entries),
// carrying every sibling section forward. An unknown name and a boot-declared
// (yaml) name each fail loud with a distinct typed error (a boot-declared
// server is edited in yaml + restart, never through this verb). The verb
// itself never tears anything down — the physical teardown (deregister tools
// + close transport) happens at the next run-start reconcile; an in-flight
// run whose next step calls the detached server fails loudly (typed
// not-found / closed transport), never a hang or a silent success.
type AgentConfigRemoveMCPConnectionRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Name is the runtime-added MCP source id to remove.
	Name string `json:"name"`
}

// AgentConfigRemoveMCPConnectionResponse is the
// `agent_config.remove_mcp_connection` response — the recorded removing
// revision and the name that was removed.
type AgentConfigRemoveMCPConnectionResponse struct {
	// Revision is the recorded config revision whose connections section
	// dropped the named descriptor (with the server's tool-exposure residue
	// pruned in the same atomic revision).
	Revision AgentConfigRevisionView `json:"revision"`
	// Name is the removed MCP source id (echoed for the caller's convenience).
	Name            string `json:"name"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigSetMCPDiscoveryOriginsRequest is the admin-scoped
// `agent_config.set_mcp_discovery_origins` request — a FULL-REPLACE write of a
// runtime-added MCP connection's OAuth-discovery cross-origin allow-list. The
// verb records a new revision carrying every sibling section forward AND applies
// the allow-list to the live MCP registry so the very next discovery uses it
// (and a revoke prunes the recorded requirement's now-unallowed
// authorization-server entries). Origins are the SHARED origin validator's
// shape (https scheme://host[:port], no path/query/fragment, no IP literal). An
// unknown connection, a boot-declared (yaml) name, and a stdio-transport
// connection each fail loud with a distinct typed error. Admin-only;
// server-derived authority (never the body). Governs revisioned state only — a
// boot-declared connection's allowance is edited in yaml + restart.
type AgentConfigSetMCPDiscoveryOriginsRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Name is the runtime-added MCP source id whose allow-list is replaced.
	Name string `json:"name"`
	// AllowedOrigins is the FULL replacement allow-list (empty clears it).
	AllowedOrigins []string `json:"allowed_origins"`
}

// AgentConfigSetMCPDiscoveryOriginsResponse is the
// `agent_config.set_mcp_discovery_origins` response — the recorded revision, the
// computed granted / revoked deltas (against the prior live allow-list), and
// whether the write took effect on the live registry.
type AgentConfigSetMCPDiscoveryOriginsResponse struct {
	// Revision is the recorded config revision carrying the new allow-list.
	Revision AgentConfigRevisionView `json:"revision"`
	// Name is the connection whose allow-list was replaced (echoed).
	Name string `json:"name"`
	// Granted is the set of origins newly present versus the prior live set.
	Granted []string `json:"granted,omitempty"`
	// Revoked is the set of origins dropped versus the prior live set.
	Revoked []string `json:"revoked,omitempty"`
	// AppliedLive reports whether the allow-list was applied to the live MCP
	// registry (true on every served success; false only degrades to the
	// revisioned write when no live registry applier is wired on this runtime).
	AppliedLive     bool   `json:"applied_live"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigSetOAuthProviderRequest is the admin-scoped
// `agent_config.set_oauth_provider` request — install (upsert) a ZERO-URL,
// broker-pull OAuth provider onto the agent-config revision spine. The
// descriptor carries `{name, driver:"tokenexchange", credential_source:"remote",
// credential_broker, scopes?}` — NO URL, NO env-var name, NO secret. The runtime
// records a new revision (carrying every sibling section forward) AND installs
// the provider live into the owner-tagged provider set so the next MCP attach
// bound to it injects the exchanged bearer. Every credential sink (the token
// endpoint, the credential-pull endpoint, the allowed downstream hosts, the
// audience, and the scope ceiling) is pinned at boot on the named broker; a
// write carrying a URL or env-var name is rejected BY NAME
// (DisallowUnknownFields). Admin-only; server-derived authority (never the body).
type AgentConfigSetOAuthProviderRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Provider is the ZERO-URL descriptor to install.
	Provider AgentConfigOAuthProviderDescriptor `json:"provider"`
}

// AgentConfigSetOAuthProviderResponse is the `agent_config.set_oauth_provider`
// response — the recorded revision and the installed provider name.
type AgentConfigSetOAuthProviderResponse struct {
	// Revision is the recorded config revision carrying the installed provider.
	Revision AgentConfigRevisionView `json:"revision"`
	// Name is the installed provider name (echoed).
	Name            string `json:"name"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigRemoveOAuthProviderRequest is the admin-scoped
// `agent_config.remove_oauth_provider` request — uninstall a Protocol-installed
// OAuth provider by name. The runtime records a new revision dropping the named
// descriptor (carrying every sibling forward) AND uninstalls the provider live,
// which CLOSES it — so a still-bound connection's next call fails LOUD rather
// than degrading to an unauthenticated dial. Deliberately breaking; the break is
// confined to the owning owner by the owner-scoped run-start reconcile. An
// unknown name and a boot-declared name each fail loud with a distinct typed
// error. Admin-only; server-derived authority (never the body).
type AgentConfigRemoveOAuthProviderRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Name is the installed provider name to uninstall.
	Name string `json:"name"`
}

// AgentConfigRemoveOAuthProviderResponse is the
// `agent_config.remove_oauth_provider` response — the recorded revision, the
// removed provider name, and whether the live uninstall took effect.
type AgentConfigRemoveOAuthProviderResponse struct {
	// Revision is the recorded config revision whose oauth-providers section
	// dropped the named descriptor.
	Revision AgentConfigRevisionView `json:"revision"`
	// Name is the removed provider name (echoed).
	Name string `json:"name"`
	// Uninstalled reports whether the provider was uninstalled live (true on
	// every served success; false only degrades to the revisioned write when no
	// live provider installer is wired on this runtime).
	Uninstalled     bool   `json:"uninstalled"`
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

// --- Session-user safe subset (the non-admin lower tier of the
// authorization matrix) ---
//
// These wire types back the NON-admin session-safe `agent_config.session.*`
// verbs: a session-scoped end user may set a user prompt layer (never the
// operator base), NARROW (never widen) source/tool enablement within the
// admin-allowed set, and manage ephemeral personal skills. The session
// shapes carry NO base-prompt field and NO enable field — the data model
// itself bounds the safe subset (a session caller physically cannot widen a
// capability or edit the operator base). Authority derives from the verified
// ctx scope at the wire handler, never the request body.

// AgentConfigSessionOverlay is the wire projection of a session's
// safe-subset overlay: a user prompt layer that composes ABOVE the operator
// base, a narrow-only source/tool disable set, and the names of the
// session's ephemeral personal skills. There is intentionally NO base-prompt
// field — a session caller cannot write the operator base.
type AgentConfigSessionOverlay struct {
	UserPrompt      string   `json:"user_prompt,omitempty"`
	DisabledServers []string `json:"disabled_servers,omitempty"`
	DisabledTools   []string `json:"disabled_tools,omitempty"`
	PersonalSkills  []string `json:"personal_skills,omitempty"`
}

// AgentConfigSessionSetUserPromptRequest is the session-safe
// `agent_config.session.set_user_prompt` request. It sets ONLY the user
// prompt layer (the shape has no base field — base is unwritable by a
// session caller).
type AgentConfigSessionSetUserPromptRequest struct {
	Identity   IdentityScope `json:"identity"`
	AgentID    string        `json:"agent_id"`
	UserPrompt string        `json:"user_prompt"`
}

// AgentConfigSessionSetUserPromptResponse is the
// `agent_config.session.set_user_prompt` response — the resulting overlay.
type AgentConfigSessionSetUserPromptResponse struct {
	Overlay         AgentConfigSessionOverlay `json:"overlay"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigSessionSetSourceDisablesRequest is the session-safe
// `agent_config.session.set_source_disables` request. It names the
// servers/tools the session wants DISABLED — narrow-only. There is no enable
// field; at projection the disable set is unioned into the admin exclusion
// set, so it can only narrow the admin-allowed exposure, never widen it.
type AgentConfigSessionSetSourceDisablesRequest struct {
	Identity        IdentityScope `json:"identity"`
	AgentID         string        `json:"agent_id"`
	DisabledServers []string      `json:"disabled_servers,omitempty"`
	DisabledTools   []string      `json:"disabled_tools,omitempty"`
}

// AgentConfigSessionSetSourceDisablesResponse is the
// `agent_config.session.set_source_disables` response — the resulting
// overlay.
type AgentConfigSessionSetSourceDisablesResponse struct {
	Overlay         AgentConfigSessionOverlay `json:"overlay"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigSessionSkillsListRequest is the session-safe
// `agent_config.session.skills.list` request — lists the session's skills
// (metadata only) under the caller's real triple.
type AgentConfigSessionSkillsListRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
}

// AgentConfigSessionSkillsListResponse is the
// `agent_config.session.skills.list` response.
type AgentConfigSessionSkillsListResponse struct {
	Skills          []AgentConfigSkillSummary `json:"skills"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigSessionSkillsUpsertRequest is the session-safe
// `agent_config.session.skills.upsert` request — upserts an ephemeral
// personal skill under the caller's real triple. The skill never promotes
// past the session.
type AgentConfigSessionSkillsUpsertRequest struct {
	Identity IdentityScope         `json:"identity"`
	AgentID  string                `json:"agent_id"`
	Skill    AgentConfigSkillInput `json:"skill"`
}

// AgentConfigSessionSkillsUpsertResponse is the
// `agent_config.session.skills.upsert` response — the upserted skill summary
// and the resulting overlay.
type AgentConfigSessionSkillsUpsertResponse struct {
	Skill           AgentConfigSkillSummary   `json:"skill"`
	Overlay         AgentConfigSessionOverlay `json:"overlay"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigSessionSkillsDeleteRequest is the session-safe
// `agent_config.session.skills.delete` request.
type AgentConfigSessionSkillsDeleteRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	Name     string        `json:"name"`
}

// AgentConfigSessionSkillsDeleteResponse is the
// `agent_config.session.skills.delete` response — the resulting overlay.
type AgentConfigSessionSkillsDeleteResponse struct {
	Overlay         AgentConfigSessionOverlay `json:"overlay"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// --- User-scope durable config variant (the middle tier of the
// authorization matrix) ---
//
// These wire types back the NON-admin `agent_config.user.*` verb family: a
// caller carrying the `auth.ScopeAgentConfigUser` claim owns a DURABLE,
// versioned safe-subset config variant keyed under their REAL (tenant, user),
// with full diff/rollback. The set verb's input is structurally bounded to
// the safe subset (user prompt + narrow-only disables + personal-skill
// names) — there is NO base / connections / enable / model field, so a user
// caller physically cannot widen a capability or edit the operator base. The
// responses REUSE the canonical AgentConfigRevisionView / AgentConfigDiff
// (payload sections the user tier never sets stay nil), giving literal
// diff/rollback parity with the admin registry surface. Authority derives
// from the verified ctx scope at the wire handler, never the request body.

// AgentConfigUserPayload is the bounded safe-subset a user-scope revision
// persists — the ONE durable user write surface for the agent-config band. It
// mirrors the session overlay's field set (user prompt + narrow-only disables
// + personal skills) and has NO base / connections / enable / model field.
// The two PROJECTION-ONLY sibling phases consume its fields: the prompt
// projection reads UserPrompt; the tool-exposure projection reads
// DisabledServers / DisabledTools.
type AgentConfigUserPayload struct {
	// UserPrompt is the user instruction layer that composes ABOVE the
	// operator base (the prompt projection consumes this).
	UserPrompt string `json:"user_prompt,omitempty"`
	// DisabledServers names the MCP servers the user narrows out (the
	// tool-exposure projection consumes this); narrow-only — there is no
	// enable path.
	DisabledServers []string `json:"disabled_servers,omitempty"`
	// DisabledTools names the individual tools the user narrows out (the
	// tool-exposure projection consumes this).
	DisabledTools []string `json:"disabled_tools,omitempty"`
	// PersonalSkills names the user's personal skill membership for the
	// variant.
	PersonalSkills []string `json:"personal_skills,omitempty"`
}

// AgentConfigUserGetRequest is the `agent_config.user.get` request — read the
// caller's own durable variant's active revision.
type AgentConfigUserGetRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
}

// AgentConfigUserGetResponse is the `agent_config.user.get` response.
type AgentConfigUserGetResponse struct {
	// Revision is the active revision; nil when the caller has no durable
	// variant (Set is false).
	Revision *AgentConfigRevisionView `json:"revision,omitempty"`
	// Set reports whether an active user revision exists.
	Set             bool   `json:"set"`
	ProtocolVersion string `json:"protocol_version"`
}

// AgentConfigUserSetRevisionRequest is the `agent_config.user.set_revision`
// request — write a new revision of the caller's durable variant from the
// bounded safe-subset payload.
type AgentConfigUserSetRevisionRequest struct {
	Identity IdentityScope          `json:"identity"`
	AgentID  string                 `json:"agent_id"`
	Payload  AgentConfigUserPayload `json:"payload"`
}

// AgentConfigUserSetRevisionResponse is the `agent_config.user.set_revision`
// response — the new (or, on an idempotent re-set, existing) revision.
type AgentConfigUserSetRevisionResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigUserListRevisionsRequest is the
// `agent_config.user.list_revisions` request — the caller's own variant
// revision chain, newest-first.
type AgentConfigUserListRevisionsRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Limit caps the returned chain length. 0 = no cap.
	Limit int `json:"limit,omitempty"`
}

// AgentConfigUserListRevisionsResponse is the
// `agent_config.user.list_revisions` response.
type AgentConfigUserListRevisionsResponse struct {
	Revisions       []AgentConfigRevisionView `json:"revisions"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigUserDiffRequest is the `agent_config.user.diff` request —
// compare two existing revisions of the caller's own variant.
type AgentConfigUserDiffRequest struct {
	Identity     IdentityScope `json:"identity"`
	AgentID      string        `json:"agent_id"`
	FromRevision string        `json:"from_revision"`
	ToRevision   string        `json:"to_revision"`
}

// AgentConfigUserDiffResponse is the `agent_config.user.diff` response.
type AgentConfigUserDiffResponse struct {
	Diff            AgentConfigDiff `json:"diff"`
	ProtocolVersion string          `json:"protocol_version"`
}

// AgentConfigUserRollbackRequest is the `agent_config.user.rollback`
// request — repoint the caller's own variant active pointer to an existing
// revision.
type AgentConfigUserRollbackRequest struct {
	Identity   IdentityScope `json:"identity"`
	AgentID    string        `json:"agent_id"`
	RevisionID string        `json:"revision_id"`
}

// AgentConfigUserRollbackResponse is the `agent_config.user.rollback`
// response — the revision the active pointer now points to.
type AgentConfigUserRollbackResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}
