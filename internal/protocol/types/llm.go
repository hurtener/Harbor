package types

// llm.go — the Phase 72g (D-112) wire types for the `llm.posture`
// Protocol method. The method surfaces the runtime's bound LLM provider
// posture so the Console Settings page LLM-Provider Posture card (Phase
// 73m) and any third-party Console implementation render the same
// projection.
//
// Single source of truth (CLAUDE.md §8): these are THE Protocol LLM
// posture wire types. The runtime-side `llm.ConfigSnapshot` is NOT
// re-exported here — the posture handler projects the bound driver's
// shape onto these wire types.
//
// The surface is READ-ONLY and reports the LLM posture honestly. The
// `MockMode` flag (D-089) is `true` iff the runtime booted with the
// dev-only mock escape hatch (`HARBOR_DEV_ALLOW_MOCK=1`). A Console
// implementation that hides the canonical `[DEV-ONLY MOCK LLM — DO NOT
// USE IN PRODUCTION]` banner when `MockMode == true` is a CLAUDE.md §13
// forbidden-practice violation — the wire flag is the structural signal
// the Console must render verbatim.

// LLMPostureRequest is the `llm.posture` request body. The TenantID
// field has the same forward-looking cross-tenant semantics as
// GovernancePostureRequest.TenantID — empty reads the caller's own
// tenant; a non-empty different value requires `auth.ScopeAdmin`.
type LLMPostureRequest struct {
	// TenantID — empty = the caller's own tenant; non-empty + different
	// from the caller's resolved tenant = requires auth.ScopeAdmin.
	TenantID string `json:"tenant_id,omitempty"`
}

// LLMPostureResponse is the `llm.posture` response body — the read-only
// projection of the runtime's bound LLM provider.
type LLMPostureResponse struct {
	// Provider is the LLM provider name (e.g. "bifrost", "mock").
	Provider string `json:"provider"`
	// Model is the bound model identifier (e.g. "openai/gpt-5.3-chat").
	Model string `json:"model"`
	// Region is the provider endpoint region; "" when not applicable
	// (the Console renders an em-dash placeholder for the empty case).
	Region string `json:"region"`
	// MockMode is true iff the runtime booted with HARBOR_DEV_ALLOW_MOCK=1
	// (D-089). The Console renders the canonical
	// `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` banner when this
	// is true; hiding the banner is a §13 forbidden-practice violation.
	MockMode bool `json:"mock_mode"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with — same field every Protocol response carries.
	ProtocolVersion string `json:"protocol_version"`
}
