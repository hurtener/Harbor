package types

// agentconfig_llm.go — the wire types for `agent_config.set_llm_provider`:
// the ZERO-URL, zero-secret inference-provider binding and its request /
// response envelopes. Separate from the OAuth-plane
// `set_oauth_provider` descriptor (a distinct credential plane); its own
// reflective no-sink-field decode test lives in agentconfig_llm_test.go.

// AgentConfigLLMProviderDescriptor is the writable inference-provider
// binding — the zero-URL install shape EXACTLY: zero-URL, zero-secret. It carries NO
// URL of any kind, NO env-var name, and NO literal secret. Every
// sink-determining value (the pull endpoint, audience, scope ceiling) lives
// on the boot-declared `inference_broker` it references by non-secret NAME,
// so no admin-writable field determines where the credential is sourced (the
// credential-plane invariant). Because the forbidden fields
// (`credential_url` / `token_url` / `*_env` / any secret) are simply NOT on
// this struct, a `DisallowUnknownFields` decode rejects any of them BY NAME
// before the method runs.
//
// The allowed field set is EXACTLY
// {name, provider, credential_source, inference_broker, model_allow}.
type AgentConfigLLMProviderDescriptor struct {
	// Name is the unique provider-binding name. Required.
	Name string `json:"name"`
	// Provider is the LLM provider the pulled key authenticates (e.g.
	// "openai", "anthropic"). Required.
	Provider string `json:"provider"`
	// CredentialSource is the inference-plane credential-source seam —
	// validated to be exactly "remote" (broker-pull). An empty value is a
	// LOUD reject ("" means the env source, which is config/file-only).
	// Required.
	CredentialSource string `json:"credential_source"`
	// InferenceBroker names a boot-declared inference broker that pins every
	// credential sink. Required; an unknown name fails loud. NON-SECRET.
	InferenceBroker string `json:"inference_broker"`
	// ModelAllow is the optional model-name allowlist the binding restricts
	// the provider to. NON-SECRET. Optional.
	ModelAllow []string `json:"model_allow,omitempty"`
}

// AgentConfigSetLLMProviderRequest is the admin-scoped
// `agent_config.set_llm_provider` request — install (upsert) / rotate a
// ZERO-URL, broker-pull inference provider binding on the owner-tagged
// provider set. Admin-only; server-derived authority (never the body).
type AgentConfigSetLLMProviderRequest struct {
	Identity IdentityScope `json:"identity"`
	AgentID  string        `json:"agent_id"`
	// Provider is the ZERO-URL descriptor to install.
	Provider AgentConfigLLMProviderDescriptor `json:"provider"`
	// No identity/scope field beyond Identity — authority is server-side.
}

// AgentConfigSetLLMProviderResponse is the `agent_config.set_llm_provider`
// response — the installed binding name and whether it took effect on the
// live provider set.
type AgentConfigSetLLMProviderResponse struct {
	// Name is the installed provider-binding name (echoed).
	Name string `json:"name"`
	// Installed reports whether the binding took effect on the live provider
	// set (false only degrades when no live installer is wired on this
	// runtime — the write is then rejected loud, never a silent no-op).
	Installed       bool   `json:"installed"`
	ProtocolVersion string `json:"protocol_version"`
}
