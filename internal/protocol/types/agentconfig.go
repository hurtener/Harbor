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

// AgentConfigNamedBlock is the wire projection of one named, additive
// prompt block: the NAME a contributor addresses its own contribution by,
// and the BODY that renders.
type AgentConfigNamedBlock struct {
	// Name addresses the block within the section. Unique within the
	// section, drawn from a restricted identifier charset
	// (`[A-Za-z0-9._-]{1,64}`), and NEVER emitted as an XML tag — the
	// attribution is a data-model property, so the prompt's structural
	// taxonomy is never a function of caller input.
	Name string `json:"name"`
	// Body is the block's prompt text. Rendered VERBATIM — see
	// AgentConfigExtraSystemBlocks.
	Body string `json:"body"`
}

// AgentConfigExtraSystemBlocks is the wire projection of an agent's
// ORDERED list of named, operator-authored additive prompt blocks. It
// exists so N independent capability sources can each contribute — and
// later remove — exactly their own text, addressed by name, instead of
// collapsing into one opaque string nobody can safely edit.
//
// # Order is SEMANTIC
//
// The declared array order IS the render order. It is preserved through
// normalisation, it is part of the revision's `content_hash`, and a pure
// re-ordering is therefore a real new revision that the diff reports.
// (Contrast `skills.names` and `oauth_providers`, whose orders are NOT
// semantic and ARE canonicalised by sorting.)
//
// # Rendering and trust
//
// Blocks render VERBATIM — unescaped — inside the `<additional_guidance>`
// section, after the runtime's baked operator guidance and before the
// additive extra-instructions, each preceded by a plain-text `[name]`
// label. They survive a session `system_prompt_override`.
//
// The section is written by ONE verb,
// `agent_config.set_extra_system_blocks`, which sits in the ADMIN tier —
// the same tier that writes `prompt_layers.base`, which is already
// verbatim and strictly more powerful. That authority tier, not an
// escaper, is the boundary.
//
// THE OBLIGATION THIS CREATES: a block MUST NOT carry user-authored or
// model-authored text. Recalled conversation content belongs in the
// UNTRUSTED-framed memory tiers (`start.caller_memory`); user
// instructions belong in `prompt_layers.user`, which IS escaped precisely
// because a claim-free session path can write it.
//
// Selection rule — spine → `prompt_layers.base`; user-authored →
// `prompt_layers.user`; per-capability additive attribution →
// `extra_system_blocks`.
type AgentConfigExtraSystemBlocks struct {
	// Blocks is the ordered list. An ARRAY, never an object keyed by name:
	// an object's key order is not a composition order.
	Blocks []AgentConfigNamedBlock `json:"blocks"`
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
	// merged into the MCP `_meta` on every identity-stamped call. Each key is
	// a `_meta` PATH — a dotted key NESTS, exactly like `injection.meta_key`.
	// Reserved / spec-prefixed keys (at the whole key OR any dot-segment),
	// over-deep paths, and paths colliding with another declared path on the
	// connection are rejected at validation.
	MetaAnnotations map[string]string `json:"meta_annotations,omitempty"`
	// OAuth is an OPTIONAL inline OAuth-provider binding carried over the wire
	// for this connection (a coordinator standing up a new OAuth-fronted MCP
	// server without a pre-installed provider). It is mutually exclusive with
	// OAuthProvider (bind a name that already exists vs. carry the full binding
	// inline). It is accepted ONLY behind the fail-closed
	// `tools.allow_wire_oauth_descriptor` boot opt-in (default off): with the
	// opt-in off an inline descriptor carrying any credential-sink field is
	// rejected, exactly as the name-only binding rejects a sink field today. When
	// opted in, the exchanged token's downstream sink (`allowed_downstream_hosts`)
	// is DERIVED from this connection's own URL — never a wire-supplied list — and
	// the wire `token_url` faces the identical token-exchange SSRF backstop the
	// boot path uses. Set only for the http transport.
	OAuth *AgentConfigOAuthProviderDescriptor `json:"oauth,omitempty"`
	// Injection is an OPTIONAL per-user credential-INJECTION binding for a
	// RECEIVER-STYLE MCP server (one that authenticates by RECEIVING a credential
	// directly on each request rather than PULLING it via RFC 8693) — a
	// coordinator ATTACHING such a server at runtime and wiring per-user
	// credential delivery to it without a boot redeploy. It NAMES a boot-declared
	// `tools.oauth_providers[]` broker (the per-user credential is sourced from it
	// per outbound call via the acting ctx identity — fetched-not-held, per-user,
	// never logged) and declares WHERE the pulled value is placed on the outbound
	// request (a header / an `Authorization: Basic` value / a `_meta` key). Only
	// the pulled value is secret; the mapping (broker name + target key/form) is
	// NON-SECRET. It is mutually exclusive with OAuthProvider / OAuth (one auth
	// mode per connection). It is accepted ONLY behind the fail-closed
	// `tools.allow_wire_injection` boot opt-in (default off, independent of the
	// wire-OAuth opt-in): with the opt-in off a connection carrying any injection
	// field is REJECTED, fail-loud. When opted in, the credential's reachable
	// downstream sink is the host DERIVED from this connection's own URL and
	// validated against the named broker's boot-declared allow-list — never a
	// wire-supplied host list — and every declared target key must be
	// redaction-covered (the audit redactor holds it to `***`). Persisted in the
	// revision (diff / rollback / list parity). Set only for the http transport.
	Injection *AgentConfigMCPCredentialInjectionDescriptor `json:"injection,omitempty"`
	// OAuthDiscoveryAllowedOrigins is the explicit per-connection cross-origin
	// allow-list of public https origins the OAuth-requirement discovery walker
	// may fetch authorization-server metadata from. NON-SECRET (an origin
	// allow-list) — revisioned + diffable + rollback-able, and writable live
	// over `agent_config.set_mcp_discovery_origins`. Empty leaves the
	// authorization-server hop needs-allowance (partial discovery), never a
	// network hole (a granted origin is still refused at dial if it resolves
	// private / loopback). Set only for the http transport.
	OAuthDiscoveryAllowedOrigins []string `json:"oauth_discovery_allowed_origins,omitempty"`
	// ArtifactByteEligible declares that this connection MAY receive
	// artifact BYTES through egress substitution — the runtime resolving
	// an artifact id the model authored and placing the resolved bytes
	// into the outbound tool-call body, so a large document reaches the
	// remote tool without transiting the model's context. NON-SECRET (a
	// boolean declaration).
	//
	// It is the containment boundary for the feature: with it unset, an
	// `artifact_params` mapping is REFUSED at this door and nothing is
	// persisted. It widens the RECIPIENT, never the reachable artifact
	// SET — resolution runs through the same run-scoped resolver, so a
	// call reaches the dispatching run's own (tenant, user, session) and
	// nothing wider.
	//
	// It carries NO fail-closed boot opt-in, unlike the inline `oauth`
	// and `injection` descriptors above, because those govern where a
	// CREDENTIAL is sent (a boot-declared-only plane) and this governs
	// where a user's own CONTENT is sent — inside the co-tenant-admin
	// trust boundary a shared runtime already accepts, whose stated
	// remedy is one runtime per tenant. Every substitution is recorded
	// fail-closed as `mcp.artifact_egressed` (ids, sizes, a digest; never
	// the bytes) before the wire request is issued. Set only for the http
	// transport.
	ArtifactByteEligible bool `json:"artifact_byte_eligible,omitempty"`
	// ArtifactParams maps this server's TOOL names to the parameter names
	// on those tools which carry artifact bytes. NON-SECRET (names only).
	// Requires ArtifactByteEligible on the same connection. Each mapped
	// parameter is validated at attach against the server's OWN discovered
	// inputSchema — declared, and declared string-typed — so Harbor never
	// asserts an argument shape the server did not publish. Refused on the
	// stdio transport. Set only for the http transport.
	ArtifactParams map[string][]string `json:"artifact_params,omitempty"`
}

// AgentConfigMCPCredentialInjectionDescriptor is the wire projection of one
// runtime-added connection's per-user credential-INJECTION mapping for a
// RECEIVER-STYLE MCP server. It is the NON-SECRET mapping ONLY: it NAMES a
// boot-declared `tools.oauth_providers[]` broker (the per-user credential is
// pulled from it per outbound call via the acting ctx identity) and declares
// WHERE the pulled value is placed on the outbound request. No secret material
// rides this descriptor — only the broker-pulled value (resolved per-call) is
// secret. It mirrors the boot `tools.mcp_servers[].injection` shape so the
// runtime-add path and the boot path share one injection engine. Accepted only
// behind the fail-closed `tools.allow_wire_injection` boot opt-in; the pulled
// credential's reachable sink is derived from the connection URL (never a
// wire-supplied host list) and every declared target key must be
// redaction-covered.
type AgentConfigMCPCredentialInjectionDescriptor struct {
	// Provider NAMES the declared `tools.oauth_providers[]` broker the per-user
	// credential is pulled from (the SAME registry `oauth_provider` resolves
	// against). NON-SECRET (a name). Required; an unknown name fails at attach.
	Provider string `json:"provider"`
	// Form selects the injection form: "header" / "basic" / "meta". Required.
	Form string `json:"form"`
	// Header is the target request header NAME for Form=="header" (e.g.
	// `x-vendor-api-key`). Required for that form; must be a redaction-covered
	// credential key and must not be `Authorization` (use Form=="basic").
	Header string `json:"header,omitempty"`
	// BasicUsername is the username half for Form=="basic"; the pulled credential
	// becomes the password half (`Authorization: Basic base64(user ":" value)`).
	// Optional (an empty username is the common API-key-as-basic shape). NON-SECRET.
	BasicUsername string `json:"basic_username,omitempty"`
	// MetaKey is the target `_meta` key PATH for Form=="meta", dot-separated for
	// nesting (e.g. `vendor.api_key`). Required for that form; no segment may be a
	// reserved `_meta` key and the leaf must be a redaction-covered credential key.
	MetaKey string `json:"meta_key,omitempty"`
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
// Protocol-installed OAuth provider. By DEFAULT it is the ZERO-URL,
// name-only descriptor: it carries NO URL of any kind, NO env-var name, and NO
// literal secret, and every credential-sink-determining value (token endpoint,
// credential-pull endpoint, allowed downstream hosts, audience, scope ceiling)
// is pinned at boot on the NAMED credential broker the descriptor references by
// non-secret name.
//
// # Dev-gated wire binding
//
// Behind the fail-closed, boot-only `tools.allow_wire_oauth_descriptor` opt-in
// (config flag OR the `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` boot env; default
// off, all of production) the descriptor MAY additionally carry the NEW server's
// OAuth parameters over the wire — TokenURL, Audience, Scopes — while still
// NAMING a boot-declared CredentialBroker, so a coordinator can stand up a new
// OAuth-fronted MCP server at runtime. With the opt-in OFF, a descriptor carrying
// TokenURL or Audience is REJECTED (fail-loud, naming the field + the opt-in
// key) — the name-only posture is byte-for-byte unchanged. The relaxation stays
// honest even when opted in:
//
//   - The runtime's OWN credential custody stays 100% boot-declared: the
//     credential source (the coordinator pull endpoint + the service-token env
//     name + the org client credential) lives on the NAMED CredentialBroker, a
//     `tools.oauth_credential_brokers[]` entry — NO credential-source URL and NO
//     env-var name ever rides the wire. Only the NEW server's public token
//     endpoint (TokenURL) is wire-carried.
//   - `allowed_downstream_hosts` is NEVER a wire field — it is DERIVED from the
//     connected server's own URL — so an exchanged token can only reach the
//     endpoint the connection dials.
//   - The wire TokenURL is dialed through the identical token-exchange SSRF
//     backstop the boot path uses (refuse resolved private / link-local / ULA /
//     unspecified, no proxy, every redirect refused).
//
// The forbidden fields that would determine a credential sink WITHOUT the
// derived/boot-declared bounds (a raw `allowed_downstream_hosts` list,
// `client_id_env`, `client_secret_env`, `auth_url`, `remote`) are simply NOT on
// this struct, so a `DisallowUnknownFields` decode rejects any of them BY NAME.
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
	// CredentialBroker names a boot-declared credential broker that pins the
	// runtime's OWN credential custody (the coordinator pull endpoint, the
	// service-token env name, the org client credential). Required in BOTH the
	// name-only and the dev-gated wire shape — the wire descriptor still names a
	// boot broker so no credential-source URL or secret ever rides the wire.
	// NON-SECRET (a name).
	CredentialBroker string `json:"credential_broker"`
	// Scopes is the requested OAuth scope subset. NON-SECRET; clamped to the
	// broker's boot scope ceiling at build time. Optional.
	Scopes []string `json:"scopes,omitempty"`
	// TokenURL is the NEW server's RFC-8693 token-exchange endpoint. A wire-carried
	// field: accepted ONLY behind the `tools.allow_wire_oauth_descriptor` opt-in,
	// and dialed through the token-exchange SSRF backstop (private / link-local /
	// ULA / unspecified refused post-DNS, every redirect refused, no proxy).
	// Rejected when the opt-in is off. Optional.
	TokenURL string `json:"token_url,omitempty"`
	// Audience is the NEW server's exchanged-token audience. A wire-carried field:
	// accepted ONLY behind the opt-in; rejected when off. Optional.
	Audience string `json:"audience,omitempty"`
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
	// ExtraSystemBlocks, when non-nil, pins the agent's ORDERED list of
	// named additive prompt blocks. Absent contributes nothing and leaves
	// the composed system prompt byte-identical.
	ExtraSystemBlocks *AgentConfigExtraSystemBlocks `json:"extra_system_blocks,omitempty"`
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

// AgentConfigExtraSystemBlocksDiff is the wire projection of the
// structured delta of the additive prompt blocks across two revisions.
type AgentConfigExtraSystemBlocksDiff struct {
	// Added / Removed / Changed are block NAMES (sorted): present only in
	// the to-revision, present only in the from-revision, and present in
	// both with a different body. Bodies are never carried here — the
	// revision payload is where the text lives.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Changed []string `json:"changed,omitempty"`
	// Reordered reports the SAME name set in a DIFFERENT order. Order is
	// render order, so a pure re-ordering is a real prompt change; it has
	// no analogue on the sorted sibling sections.
	Reordered bool `json:"reordered"`
}

// AgentConfigDiff is the wire projection of a server-side revision
// compare — the structured skills + tool-exposure + connection set-diffs,
// the prompt-layer text delta, the per-agent LLM-parameter delta, the
// run-lifecycle-hook delta, the session auto-naming policy delta, and the
// additive prompt-block delta.
type AgentConfigDiff struct {
	FromRevisionID    string                           `json:"from_revision_id"`
	ToRevisionID      string                           `json:"to_revision_id"`
	Skills            AgentConfigSkillsDiff            `json:"skills"`
	ToolExposure      AgentConfigToolExposureDiff      `json:"tool_exposure"`
	PromptLayers      AgentConfigPromptLayersDiff      `json:"prompt_layers"`
	Connections       AgentConfigConnectionsDiff       `json:"connections"`
	OAuthProviders    AgentConfigOAuthProvidersDiff    `json:"oauth_providers"`
	LLMParams         AgentConfigLLMParamsDiff         `json:"llm_params"`
	Hooks             AgentConfigHooksDiff             `json:"hooks"`
	Naming            AgentConfigNamingDiff            `json:"naming"`
	ExtraSystemBlocks AgentConfigExtraSystemBlocksDiff `json:"extra_system_blocks"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

// AgentConfigSetPromptLayersResponse is the `agent_config.set_prompt_layers`
// response — the recorded config revision (or, on an idempotent re-set, the
// existing active revision).
type AgentConfigSetPromptLayersResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigSetExtraSystemBlocksRequest is the admin-scoped
// `agent_config.set_extra_system_blocks` request — set the agent's ORDERED
// list of named additive prompt blocks as a desired-state REPLACE of the
// blocks section (every sibling section of the active revision is
// preserved). The edit records a new config revision and applies to the
// agent's NEXT run.
//
// # It is a WHOLE-SECTION replace, deliberately
//
// There are no per-block upsert / delete verbs. A block is one element of
// an ORDERED composition whose order is semantic, so a per-item upsert has
// no well-defined insertion position. A second contributor composes by
// read-modify-write: `agent_config.get`, append or drop its own block by
// NAME, write back — sending the read revision's `content_hash` as
// ExpectedContentHash so a concurrent sibling's write is refused rather
// than silently reverted. When the read answered `set: false` (the agent has
// no config yet) there is no hash to echo: send the reserved first-write
// token "-", which succeeds only while no revision exists. Omitting the
// token there would be an unconditional write, and two contributors
// composing onto a fresh agent would silently revert each other — the one
// case the protocol could not express before the sentinel.
//
// # Trust
//
// The bodies render VERBATIM in an operator-trusted position. This verb is
// admin-gated, and that is the whole boundary — see
// AgentConfigExtraSystemBlocks for the argument and the obligation it
// creates (a block must never carry user-authored or model-authored text).
type AgentConfigSetExtraSystemBlocksRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// ExtraSystemBlocks is the FULL desired state of the section. An empty
	// list clears it. Names must be unique and match `[A-Za-z0-9._-]{1,64}`;
	// a duplicate or malformed name is refused with `invalid_request`,
	// naming the offender, and NOTHING is persisted.
	ExtraSystemBlocks AgentConfigExtraSystemBlocks `json:"extra_system_blocks"`
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write every sibling door also performs.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is the
	// token to send when the read answered `set: false` and there is no hash
	// to echo — without it the base case of the composition protocol has no
	// expressible form and two first contributors silently revert each other.
	// A real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// Sending it is how two independent contributors compose safely: the
	// read-modify-write is only as safe as the token. The refusal is exact
	// within one Runtime process; it is not a cross-process
	// compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

// AgentConfigSetExtraSystemBlocksResponse is the
// `agent_config.set_extra_system_blocks` response — the recorded config
// revision (or, on an idempotent re-set, the existing active revision).
type AgentConfigSetExtraSystemBlocksResponse struct {
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	//
	// This is the ONE door where the token can be evaluated only AFTER a
	// live side effect: whether the server answers is the input to what gets
	// written, so the dial / handshake / register runs first. A conflict
	// therefore COMPENSATES rather than merely refusing — the just-attached
	// server is detached, an inline wire-OAuth provider installed for this
	// binding is uninstalled, and a terminal `mcp.connection.failed`
	// lifecycle event is emitted carrying the conflict as its reason. The
	// "NOTHING is persisted" guarantee above is therefore about the world,
	// not just the revision spine: a refused add leaves no live server that
	// no revision names (which `remove_mcp_connection` could never remove)
	// and never leaves the lifecycle parked on the transient `pending`.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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

// --- Durable-per-user skills (CLAIM-FREE) ---
//
// These wire types back the `agent_config.user.skills.*` verbs — the durable
// analogue of the ephemeral session-skills family. `user` names the storage
// SCOPE (durable, keyed (tenant, user)); it is NOT an auth tier: the verbs are
// CLAIM-FREE (a valid identity is enough, no admin and no
// `auth.ScopeAgentConfigUser`) because a personal skill cannot widen
// capability — the capability filter is default-deny and the injection-time
// redactor scrubs any tool a skill names that is outside the run's allowed
// set. The upsert/delete responses REUSE the canonical AgentConfigRevisionView
// (the recorded user-scope membership revision) so the durable rung inherits
// the same diff/rollback trail as the rest of the user config variant.

// AgentConfigUserSkillsListRequest is the `agent_config.user.skills.list`
// request — lists the caller's durable user-scope personal skills (metadata
// only) under their real (tenant, user).
type AgentConfigUserSkillsListRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
}

// AgentConfigUserSkillsListResponse is the `agent_config.user.skills.list`
// response.
type AgentConfigUserSkillsListResponse struct {
	Skills          []AgentConfigSkillSummary `json:"skills"`
	ProtocolVersion string                    `json:"protocol_version"`
}

// AgentConfigUserSkillsUpsertRequest is the `agent_config.user.skills.upsert`
// request — upserts a DURABLE personal skill at user scope (persists across
// ALL of the caller's conversations). The skill scope is forced to
// `skills.ScopeUser` server-side; the body has no scope field.
type AgentConfigUserSkillsUpsertRequest struct {
	Identity IdentityScope         `json:"identity"`
	AgentID  string                `json:"agent_id"`
	Skill    AgentConfigSkillInput `json:"skill"`
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

// AgentConfigUserSkillsUpsertResponse is the `agent_config.user.skills.upsert`
// response — the upserted skill summary plus the recorded durable user-scope
// membership revision.
type AgentConfigUserSkillsUpsertResponse struct {
	Skill           AgentConfigSkillSummary `json:"skill"`
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}

// AgentConfigUserSkillsDeleteRequest is the `agent_config.user.skills.delete`
// request.
type AgentConfigUserSkillsDeleteRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	Name     string        `json:"name"`
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

// AgentConfigUserSkillsDeleteResponse is the `agent_config.user.skills.delete`
// response — the recorded durable user-scope membership revision after the
// change.
type AgentConfigUserSkillsDeleteResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
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
	// PersonalSkills names the user's durable personal skill membership for
	// the variant. It COMPOSES with the `agent_config.user.skills.*` verbs:
	// those verbs write the skill BODIES to the SkillStore at user scope AND
	// incrementally mutate this same membership list; `set_revision` sets it
	// declaratively. The run-start skills projection reads this membership to
	// keep durable user skills visible even when the admin pins a skills
	// membership.
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
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
	// ExpectedContentHash is the OPTIONAL expected-revision token. When
	// non-empty, the write requires the agent's ACTIVE revision to still
	// carry exactly this content hash (as returned by `agent_config.get`)
	// at write time; a moved base is refused with the `revision_conflict`
	// error code (HTTP 409) and NOTHING is persisted. Empty (the default,
	// and every request that omits the field) is the unconditional
	// last-writer-wins write this door has always performed.
	//
	// The reserved value "-" is the FIRST-WRITE token: it requires the agent
	// to have NO active revision, and is refused once one exists. It is what
	// makes the read-modify-write composition protocol expressible at its
	// base case — a caller whose `agent_config.get` answered `set: false`
	// has no hash to echo, so before the sentinel its only expressible token
	// was the empty one, i.e. it could not opt out of last-writer-wins on
	// the one write where two contributors are most likely to collide. A
	// real content hash is 64 lowercase hex characters, so "-" can never
	// collide with one.
	//
	// The refusal is exact within one Runtime process; it is not a
	// cross-process compare-and-swap.
	ExpectedContentHash string `json:"expected_content_hash,omitempty"`
}

// AgentConfigUserRollbackResponse is the `agent_config.user.rollback`
// response — the revision the active pointer now points to.
type AgentConfigUserRollbackResponse struct {
	Revision        AgentConfigRevisionView `json:"revision"`
	ProtocolVersion string                  `json:"protocol_version"`
}
