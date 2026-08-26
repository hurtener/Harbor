package types

// llm.go — the wire types for the `llm.posture`
// Protocol method. The method surfaces the runtime's bound LLM provider
// posture so the Console Settings page LLM-Provider Posture card (
// ) and any third-party Console implementation render the same
// projection.
//
// Single source of truth (CLAUDE.md §8): these are THE Protocol LLM
// posture wire types. The runtime-side `llm.ConfigSnapshot` is NOT
// re-exported here — the posture handler projects the bound driver's
// shape onto these wire types.
//
// The surface is READ-ONLY and reports the LLM posture honestly. The
// `MockMode` flag is `true` iff the runtime booted with the
// dev-only mock escape hatch (`HARBOR_DEV_ALLOW_MOCK=1`). A Console
// implementation that hides the canonical `[DEV-ONLY MOCK LLM — DO NOT
// USE IN PRODUCTION]` banner when `MockMode == true` is a CLAUDE.md §13
// forbidden-practice violation — the wire flag is the structural signal
// the Console must render verbatim.

// `llm.posture` takes the shared posture request envelope,
// RuntimeInfoRequest — not a type of its own, and the cross-tenant
// selector is `identity.tenant`. An `LLMPostureRequest` carrying a
// `tenant_id` field used to be declared here and was never decoded; it
// was removed rather than implemented, for the reasons recorded on the
// `governance.posture` sibling in governance.go.

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
	// MockMode is true iff the runtime booted with HARBOR_DEV_ALLOW_MOCK=1.
	// The Console renders the canonical
	// `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` banner when this
	// is true; hiding the banner is a §13 forbidden-practice violation.
	MockMode bool `json:"mock_mode"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// with — same field every Protocol response carries.
	ProtocolVersion string `json:"protocol_version"`
}

// LLMProviderOperationResponse is the bounded, content-free result of a
// runtime-origin provider catalog operation. Provider credentials, endpoint
// values, and raw provider response bodies never cross this wire boundary.
type LLMProviderOperationResponse struct {
	Operation     string                       `json:"operation"`
	RuntimeOrigin bool                         `json:"runtime_origin"`
	ProviderID    string                       `json:"provider_id,omitempty"`
	Descriptors   []LLMProviderDescriptor      `json:"descriptors,omitempty"`
	Validation    *LLMProviderValidation       `json:"validation,omitempty"`
	Discovery     *LLMProviderDiscovery        `json:"discovery,omitempty"`
	Route         *LLMProviderRouteObservation `json:"route,omitempty"`
}

// LLMProviderRouteObservation echoes only exact generations and readiness.
type LLMProviderRouteObservation struct {
	RouteID                      string `json:"route_id"`
	RouteGeneration              uint64 `json:"route_generation"`
	ProviderConnectionID         string `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64 `json:"provider_connection_generation"`
	CredentialAssetGeneration    uint64 `json:"credential_asset_generation"`
	Ready                        bool   `json:"ready"`
}

type LLMProviderDescriptor struct {
	ID               string                       `json:"id"`
	Kind             string                       `json:"kind"`
	CredentialModes  []string                     `json:"credential_modes"`
	CredentialFields []LLMProviderCredentialField `json:"credential_fields"`
	CustomEndpoint   string                       `json:"custom_endpoint"`
	Validation       LLMProviderOperation         `json:"validation"`
	Discovery        LLMProviderOperation         `json:"discovery"`
}

// LLMProviderCredentialField describes a non-secret input shape. Secret
// values and endpoint values never cross this wire type.
type LLMProviderCredentialField struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Secret   bool   `json:"secret"`
}

type LLMProviderOperation struct {
	State         string `json:"state"`
	RuntimeOrigin bool   `json:"runtime_origin"`
	Bounded       bool   `json:"bounded"`
}

type LLMProviderValidation struct {
	ProviderID string             `json:"provider_id"`
	Outcome    LLMProviderOutcome `json:"outcome"`
}

type LLMProviderDiscovery struct {
	ProviderID string             `json:"provider_id"`
	Outcome    LLMProviderOutcome `json:"outcome"`
	Models     []LLMProviderModel `json:"models,omitempty"`
	Pages      int                `json:"pages"`
	ModelCount int                `json:"model_count"`
}

type LLMProviderOutcome struct {
	State         string `json:"state"`
	Code          string `json:"code"`
	Message       string `json:"message"`
	ObservedAt    string `json:"observed_at,omitempty"`
	RuntimeOrigin bool   `json:"runtime_origin"`
	Partial       bool   `json:"partial"`
	Stale         bool   `json:"stale"`
}

type LLMProviderModel struct {
	ID           string                       `json:"id"`
	Source       string                       `json:"source"`
	Deprecated   bool                         `json:"deprecated"`
	Capabilities LLMProviderModelCapabilities `json:"capabilities"`
}

// The capability shapes mirror the provider-neutral runtime descriptor while
// remaining content-free and bounded for Protocol consumers.
type LLMProviderModelCapabilities struct {
	Context          LLMProviderNumericCapability   `json:"context"`
	MaxInputTokens   LLMProviderNumericCapability   `json:"max_input_tokens"`
	MaxOutputTokens  LLMProviderNumericCapability   `json:"max_output_tokens"`
	InputModalities  LLMProviderSetCapability       `json:"input_modalities"`
	OutputModalities LLMProviderSetCapability       `json:"output_modalities"`
	Tools            string                         `json:"tools"`
	Vision           string                         `json:"vision"`
	Reasoning        LLMProviderReasoningCapability `json:"reasoning"`
	Pricing          LLMProviderPricingCapability   `json:"pricing"`
}

type LLMProviderNumericCapability struct {
	State string `json:"state"`
	Value int    `json:"value,omitempty"`
}

type LLMProviderSetCapability struct {
	State  string   `json:"state"`
	Values []string `json:"values,omitempty"`
}

type LLMProviderReasoningCapability struct {
	State  string   `json:"state"`
	Levels []string `json:"levels,omitempty"`
}

type LLMProviderPricingCapability struct {
	State  string `json:"state"`
	Source string `json:"source,omitempty"`
}
