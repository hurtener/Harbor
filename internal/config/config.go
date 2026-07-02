// Package config defines Harbor's strongly-typed configuration surface.
//
// Configuration is loaded once at boot via Load (or LoadFromBytes for
// tests). After Load returns, the *Config is immutable; subsystems
// hold a *Config reference and read from it concurrently. This is the
// concurrent-reuse contract from.
//
// The struct layout uses one sub-struct per RFC §6 subsystem so that
// future phases can extend their slice without cross-package import
// cycles. Sub-structs whose owning phase has not yet shipped are
// reserved as empty zero-valued types — yaml `omitempty` keeps the
// boot-log readable.
//
// Two struct tag conventions augment the standard yaml tag:
//
//   - `reload:"live"` opts a field into hot-reload. The default is
//     `restart` (whether explicitly tagged or absent). Harbor ships
//     the mechanism; no field opts in yet.
//   - `secret:"true"` marks a field whose value must be redacted by
//     MarshalForLogging. A name-based fallback also redacts fields
//     whose YAML name matches the canonical secret list.
package config

import "time"

// Config is the root configuration. It is immutable after Load.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Identity    IdentityConfig    `yaml:"identity"`
	Telemetry   TelemetryConfig   `yaml:"telemetry"`
	State       StateConfig       `yaml:"state"`
	LLM         LLMConfig         `yaml:"llm"`
	Governance  GovernanceConfig  `yaml:"governance"`
	Distributed DistributedConfig `yaml:"distributed,omitempty"`

	// Reserved slots for future phases — owning phase fills the body.
	Runtime   RuntimeConfig   `yaml:"runtime,omitempty"`   // owned by runtime/* phases
	Memory    MemoryConfig    `yaml:"memory,omitempty"`    // owned by memory phases
	Skills    SkillsConfig    `yaml:"skills,omitempty"`    // owned by skills phases
	Tasks     TasksConfig     `yaml:"tasks,omitempty"`     // owned by tasks phases
	Sessions  SessionsConfig  `yaml:"sessions,omitempty"`  // owned by sessions phases
	Artifacts ArtifactsConfig `yaml:"artifacts,omitempty"` // owned by artifacts phases
	Events    EventsConfig    `yaml:"events,omitempty"`    // owned by events phases
	Audit     AuditConfig     `yaml:"audit,omitempty"`     // owned by the audit subsystem
	Protocol  ProtocolConfig  `yaml:"protocol,omitempty"`  // owned by protocol phases
	CLI       CLIConfig       `yaml:"cli,omitempty"`       // owned by CLI phases
	Tools     ToolsConfig     `yaml:"tools,omitempty"`     // owned by tools subsystem phases (26 / 27 / 28 / 29)
	Planner   PlannerConfig   `yaml:"planner,omitempty"`   // owned by the planner subsystem

	// Multimodal is the attachment-handling policy block.
	// Optional; an omitted block keeps the runtime default
	// (image/* inline, everything else ref).
	Multimodal MultimodalConfig `yaml:"multimodal,omitempty"`

	// Embeddings is the embedding-client block — the model/provider
	// pair Harbor turns text into vectors with, configured separately
	// from the chat `llm` block. Optional; REQUIRED (validated) when
	// any semantic-retrieval mode is enabled (`memory.retrieval` /
	// `skills.retrieval` = `semantic`).
	Embeddings EmbeddingsConfig `yaml:"embeddings,omitempty"`

	PauseResume PauseResumeConfig `yaml:"pauseresume,omitempty"` // owned by the pause/resume subsystem

	// source records the originating filename for error messages.
	// Empty when LoadFromBytes is called without a name. Unexported so
	// it never appears in YAML / logging output.
	source string `yaml:"-"`
}

// ServerConfig is the network surface for the Harbor binary.
//
// AllowedOrigins is the CORS allowlist for the
// multi-process Console+Runtime posture. Each entry is an exact
// origin (`scheme://host[:port]`) the Runtime accepts cross-origin
// requests from. Empty list (the default) = no CORS headers = same-
// origin only (the original posture). The validator rejects `*` (or any
// wildcard shape) unless CORSDevAllowAny is true, per CLAUDE.md
// §7 — never wildcard in production.
//
// CORSDevAllowAny is the explicit, dev-only escape hatch that opens the
// CORS surface to ANY origin. NEVER set in production: a `harbor dev`
// boot with this flag set prints a stderr banner so the posture is
// visibly dev-only, and the validator refuses `*` shapes without it.
// Provided for first-clone Console iteration against a `harbor dev`
// loop where the Console origin (Vite, `:5173`) varies during
// development.
type ServerConfig struct {
	BindAddr            string        `yaml:"bind_addr"`
	ShutdownGracePeriod time.Duration `yaml:"shutdown_grace_period"`
	AllowedOrigins      []string      `yaml:"allowed_origins,omitempty"`
	CORSDevAllowAny     bool          `yaml:"cors_dev_allow_any,omitempty"`
	// DebugAddr optionally enables the pprof debug HTTP listener on a
	// SEPARATE host:port (never the Protocol mux). Empty (the default)
	// disables it entirely. When set it MUST be a loopback address
	// (127.0.0.0/8 or ::1) — the validator rejects any other host so the
	// profiler surface can never be exposed off-box. The listener serves
	// only net/http/pprof handlers and is off by default; it is a
	// dev/diagnostic escape hatch, not a production surface.
	DebugAddr string `yaml:"debug_addr,omitempty"`
}

// IdentityConfig configures JWT validation. Per AGENTS.md §7 the
// algorithm allowlist must contain only asymmetric algorithms.
type IdentityConfig struct {
	JWTAlgorithms []string `yaml:"jwt_algorithms"`
	Issuer        string   `yaml:"issuer"`
	Audience      string   `yaml:"audience"`
	JWKSURL       string   `yaml:"jwks_url,omitempty"`
	JWKSFile      string   `yaml:"jwks_file,omitempty"`
	// JWKSMaxStale bounds how long a cached JWKS key snapshot is honored
	// without a successful refresh. Past this age the validator fails
	// closed (rejects tokens with a distinct staleness reason) rather
	// than serving a possibly-revoked key during a prolonged IdP outage.
	// Zero (the default when omitted) applies the safe built-in ceiling;
	// a negative or below-floor value is a validation error. There is no
	// "disable" path — Harbor's posture is fail-closed; the field tunes
	// the ceiling, it does not remove it. This BOUNDS — it does not make
	// instantaneous — key revocation: pair a tight ceiling with
	// overlapping IdP signing keys and short token TTLs.
	JWKSMaxStale time.Duration `yaml:"jwks_max_stale,omitempty"`
}

// TelemetryConfig configures slog and OpenTelemetry export.
type TelemetryConfig struct {
	LogFormat    string `yaml:"log_format"`
	LogLevel     string `yaml:"log_level"`
	OTelEndpoint string `yaml:"otel_endpoint,omitempty"`
	ServiceName  string `yaml:"service_name"`
}

// StateConfig selects the StateStore driver and its connection.
type StateConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn,omitempty" secret:"true"`
}

// LLMConfig is the default LLM client surface for the runtime
// (the LLM subsystem).
//
// `Driver` selects the §4.4 LLM driver. Harbor ships `"mock"`;
// Harbor registers `"bifrost"`. Empty defaults to `"mock"` so a
// missing configuration value does NOT silently route real LLM
// traffic — operators opt in to bifrost explicitly.
//
// `Provider` / `Model` / `APIKey` / `BaseURL` / `Timeout` are the
// per-bifrost-driver knobs. They are REQUIRED when `Driver != "mock"`
// — `validateLLM` enforces. The mock driver ignores them.
//
// `ContextWindowReserve` is the safety-net token-budget margin
// (default 0.05 / 5%). The safety pass fails with
// `ErrContextWindowExceeded` when the estimated token count is
// within this fraction of a model's configured cap. Range [0.0, 1.0).
//
// `ModelProfiles` carries per-model knobs (context-window cap,
// estimator, JSON-schema mode, default max tokens, reasoning effort,
// cost overrides). The safety net's token-budget guard REQUIRES a
// profile entry for every model the request mentions; missing
// profiles surface at request time as `ErrUnsupportedModel`.
// EmbeddingsConfig is the embedding-client surface — Harbor's
// text→vector capability (`internal/embeddings`), a sibling seam to
// the chat client. The embedding model is its own operator choice:
// chat and embeddings routinely come from different providers, so
// nothing here falls back to the `llm` block.
//
// The whole block is optional. It becomes REQUIRED (enforced by
// `validateEmbeddings`) the moment an embedding-consuming mode is
// enabled — `memory.retrieval: semantic` or `skills.retrieval:
// semantic` — so a semantic mode can never silently degrade to
// non-semantic behaviour (AGENTS.md §13).
//
//   - `Driver` selects the registered embeddings driver. Empty
//     defaults to `"bifrost"` (the production gateway driver).
//   - `Provider` / `Model` — the embedding provider + model (e.g.
//     `openai` / `text-embedding-3-small`). Both required when the
//     block is in use.
//   - `APIKey` — literal value or `env.NAME` reference, matching the
//     `llm.api_key` convention. Required when the block is in use.
//   - `BaseURL` / `Timeout` — optional network knobs.
//   - `Dimensions` — optional reduced output dimension for providers
//     that support it; 0 keeps the model's native dimension.
//
// Restart-required (no `reload:"live"`).
type EmbeddingsConfig struct {
	Driver     string        `yaml:"driver,omitempty"`
	Provider   string        `yaml:"provider,omitempty"`
	Model      string        `yaml:"model,omitempty"`
	APIKey     string        `yaml:"api_key,omitempty" secret:"true"`
	BaseURL    string        `yaml:"base_url,omitempty"`
	Timeout    time.Duration `yaml:"timeout,omitempty"`
	Dimensions int           `yaml:"dimensions,omitempty"`
}

// IsZero reports whether the operator left the embeddings block
// entirely unset. Used by the validator (a zero block is fine unless
// a semantic mode demands it) and by the runtime assembly (a zero
// block means no embedder is constructed).
func (e EmbeddingsConfig) IsZero() bool {
	return e == EmbeddingsConfig{}
}

type LLMConfig struct {
	Driver               string                           `yaml:"driver"`
	Provider             string                           `yaml:"provider"`
	Model                string                           `yaml:"model"`
	APIKey               string                           `yaml:"api_key" secret:"true"`
	BaseURL              string                           `yaml:"base_url,omitempty"`
	Timeout              time.Duration                    `yaml:"timeout"`
	ContextWindowReserve float64                          `yaml:"context_window_reserve,omitempty"`
	ModelProfiles        map[string]LLMModelProfileConfig `yaml:"model_profiles,omitempty"`
	// Corrections toggles + per-model-profile-override the
	// provider-correction layer. Omitted = enabled (production
	// default). See `LLMCorrectionsConfig` for the wire shape.
	Corrections LLMCorrectionsConfig `yaml:"corrections,omitempty"`

	// CustomProviders is the registry of operator-declared
	// OpenAI-compatible providers. Each entry adds a new
	// `ModelProvider` to the bifrost account so operators can wire
	// NIM, vLLM, ollama, lm-studio, in-house gateways, or any other
	// OpenAI-compatible endpoint via yaml without per-provider Go
	// code. When `LLMConfig.Provider` matches a custom-provider
	// `Name`, the entry's `BaseURL` / `APIKeyEnvVar` / `Models` /
	// network knobs apply (the legacy single-provider fields
	// `APIKey` / `BaseURL` / `Timeout` are ignored for that case).
	CustomProviders []LLMCustomProviderConfig `yaml:"custom_providers,omitempty"`

	// NetworkDefaults applies to every provider (native + custom)
	// when the per-provider override is absent. Zero-valued fields
	// fall through to bifrost's package-level defaults (a
	// unification of timeout/retry knobs that were previously
	// scattered). Restart-required.
	NetworkDefaults LLMNetworkDefaults `yaml:"network_defaults,omitempty"`
}

// LLMCustomProviderConfig declares one operator-configured
// OpenAI-compatible LLM endpoint. At least one entry is
// required when `LLMConfig.Provider` names a non-native provider.
//
// Fields:
//   - `Name` — the `ModelProvider` identifier the operator picks
//     (e.g. `"nim"`, `"vllm"`). Must be unique across the list AND
//     must not collide with bifrost's native provider names.
//   - `BaseURL` — the OpenAI-compatible endpoint root (e.g.
//     `"https://integrate.api.nvidia.com/v1"`). Required.
//   - `APIKeyEnvVar` — the environment variable name (no `env.`
//     prefix; operator writes `"NVIDIA_API_KEY"`, driver resolves
//     `os.Getenv(...)` at construction time). Missing → fail-closed
//     at `New` (`ErrMissingAPIKey`).
//   - `Models` — the model-name allowlist bifrost forwards to this
//     provider. At least one entry required.
//   - `BaseProviderType` — wire family. V1 accepts only `""`
//     (default to `"openai"`) and `"openai"`. Future phases widen.
//   - `Timeout` / `MaxRetries` / `RetryBackoff*` / `Concurrency` /
//     `BufferSize` — per-provider overrides. Zero-valued → fall back
//     to `LLMConfig.NetworkDefaults`, which itself falls back to
//     bifrost's package-level defaults.
//   - `RequestPathOverrides` — optional `RequestType` → custom URL
//     path map (forwarded to bifrost's `CustomProviderConfig`). Used
//     when an OpenAI-compatible endpoint hosts e.g. `/chat/completions`
//     at the root instead of `/v1/chat/completions`.
type LLMCustomProviderConfig struct {
	Name                 string            `yaml:"name"`
	BaseURL              string            `yaml:"base_url"`
	APIKeyEnvVar         string            `yaml:"api_key_env_var"`
	Models               []string          `yaml:"models"`
	BaseProviderType     string            `yaml:"base_provider_type,omitempty"`
	Timeout              time.Duration     `yaml:"timeout,omitempty"`
	MaxRetries           int               `yaml:"max_retries,omitempty"`
	RetryBackoffInitial  time.Duration     `yaml:"retry_backoff_initial,omitempty"`
	RetryBackoffMax      time.Duration     `yaml:"retry_backoff_max,omitempty"`
	Concurrency          int               `yaml:"concurrency,omitempty"`
	BufferSize           int               `yaml:"buffer_size,omitempty"`
	RequestPathOverrides map[string]string `yaml:"request_path_overrides,omitempty"`
}

// LLMNetworkDefaults are the operator-tunable defaults that apply to
// every provider (native + custom) when the per-provider override is
// absent. Zero-valued fields fall through to bifrost's
// package-level defaults so an operator who omits the block sees
// today's behaviour unchanged.
type LLMNetworkDefaults struct {
	Timeout             time.Duration `yaml:"timeout,omitempty"`
	MaxRetries          int           `yaml:"max_retries,omitempty"`
	RetryBackoffInitial time.Duration `yaml:"retry_backoff_initial,omitempty"`
	RetryBackoffMax     time.Duration `yaml:"retry_backoff_max,omitempty"`
	Concurrency         int           `yaml:"concurrency,omitempty"`
	BufferSize          int           `yaml:"buffer_size,omitempty"`
}

// LLMCorrectionsConfig is the top-level toggle for the
// per-provider correction layer. The layer is enabled by default;
// operators set `enabled: false` only for testing scenarios that
// need to exercise the safety pass in isolation.
//
// `Enabled` is `*bool` so the loader can distinguish "operator didn't
// set the field" (nil → defaults to true) from "operator explicitly
// disabled" (&false). Restart-required.
type LLMCorrectionsConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// LLMModelProfileConfig is one entry in `LLMConfig.ModelProfiles`.
// Keyed by canonical model name (e.g. `"anthropic/claude-sonnet-4"`,
// `"google/gemini-3.1-flash-lite"`). Harbor ships the shape;
// later LLM phases consume the fields.
//
//   - `ContextWindowTokens` is the model's hard input-token cap.
//     REQUIRED (> 0); the safety net's token-budget guard uses it.
//   - `TokenEstimator` selects the estimator algorithm. "" / "chars_div_4"
//     default chars/4 + per-message overhead. Later phases may
//     register tiktoken-equivalent estimators by name.
//   - `JSONSchemaMode` — reads ("native" / "tools" /
//     "prompted"). Harbor stores opaque.
//   - `DefaultMaxTokens` — the identity-tier override
//     target. nil → use the runtime/governance default.
//   - `ReasoningEffort` — request-level default. Empty string →
//     "use provider default."
//   - `CostOverrides` — fallback per-1M-token rates when the
//     provider doesn't include cost in its response. Harbor
//     reads when accumulating identity-scoped cost.
type LLMModelProfileConfig struct {
	ContextWindowTokens int                     `yaml:"context_window_tokens"`
	TokenEstimator      string                  `yaml:"token_estimator,omitempty"`
	JSONSchemaMode      string                  `yaml:"json_schema_mode,omitempty"`
	DefaultMaxTokens    *int                    `yaml:"default_max_tokens,omitempty"`
	ReasoningEffort     string                  `yaml:"reasoning_effort,omitempty"`
	CostOverrides       *LLMCostOverridesConfig `yaml:"cost_overrides,omitempty"`
	// Corrections — per-model overrides for the correction
	// layer. nil → use the per-provider defaults baked into
	// `internal/llm/corrections.defaultProfileFor`. See
	// `LLMCorrectionsProfileConfig` for the wire shape.
	Corrections *LLMCorrectionsProfileConfig `yaml:"corrections,omitempty"`
	// MaxRetries — caps the validator-driven retry loop the
	// `internal/llm/retry` wrapper runs against this model. Zero (the
	// default) maps to `llm.DefaultMaxRetries` (1). Negative is
	// rejected by `validateLLM`.
	MaxRetries int `yaml:"max_retries,omitempty"`
}

// LLMCorrectionsProfileConfig is one per-model profile override for
// the correction layer. Each field is the operator-facing
// string form of the equivalent `internal/llm` enum
// (`MessageOrderingPolicy`, `SchemaSanitizationMode`, `ReasoningRouting`,
// `ResponseFormatProfile`). Empty string → use the per-provider
// default keyed by model-name prefix.
//
// Valid enum values:
//
//   - `message_ordering`: "" / "system_first_strict"
//   - `schema_mode`: "" / "openai_strict" / "permissive"
//   - `reasoning_effort_routing`: "" / "thinking_model"
//   - `response_format_shape`: "" / "json_only" / "anthropic"
//   - `usage_backfill_enabled`: bool
type LLMCorrectionsProfileConfig struct {
	MessageOrdering        string `yaml:"message_ordering,omitempty"`
	SchemaMode             string `yaml:"schema_mode,omitempty"`
	ReasoningEffortRouting string `yaml:"reasoning_effort_routing,omitempty"`
	ResponseFormatShape    string `yaml:"response_format_shape,omitempty"`
	UsageBackfillEnabled   bool   `yaml:"usage_backfill_enabled,omitempty"`
}

// LLMCostOverridesConfig is a per-model cost-table override (used
// when the provider response doesn't include cost). Harbor reads.
type LLMCostOverridesConfig struct {
	InputPer1M     float64 `yaml:"input_per_1m_tokens"`
	OutputPer1M    float64 `yaml:"output_per_1m_tokens"`
	ReasoningPer1M float64 `yaml:"reasoning_per_1m_tokens,omitempty"`
	Currency       string  `yaml:"currency,omitempty"`
}

// GovernanceConfig holds the V1 governance policy surface — per-tier
// cost ceilings, rate limits, and MaxTokens caps, declared through
// `IdentityTiers`. Hot-reload is not yet wired;
// every field is restart-required.
//
// **Enforcement is wired**: a populated
// `IdentityTiers` map is composed into the enforcement Subsystem
// (MaxTokens → rate limit → cost ceiling) by the production assembly
// (`assemble.Assemble` → `governance.SetFactory` → the `llm.Open`
// wrapper chain) — requests ARE throttled, budgeted, and token-capped,
// and the same map also drives the read-only `governance.posture`
// Protocol surface.
//
// **Latent default:** an empty `IdentityTiers`
// map + empty `DefaultTier` keeps the surface fully latent — no
// enforcement, no wrapper in the LLM chain.
//
// **Removed (governance-config consolidation):** the
// pre-Phase-36a single-knob fields `default_max_tokens`,
// `cost_ceiling_usd`, and `rate_limit_tps` are no longer accepted on
// `GovernanceConfig`. They were validated-but-ignored stubs: the
// loader took them, the enforcement engine never consumed them, and an
// operator setting `cost_ceiling_usd: 100` in YAML saw silent no-op
// behaviour. The loader now emits a structured
// `config.deprecated_field` slog warning when any of those keys
// appears in YAML, drops the value, and proceeds. Operators migrating
// from a pre-Phase-36a config build a `default` tier with the
// equivalent values under `IdentityTiers`.
type GovernanceConfig struct {
	RepairAttempts int `yaml:"repair_attempts"`

	// DefaultTier is the tier name applied to an identity that does
	// not match a custom resolver mapping. Empty = no default tier =
	// no enforcement for unmatched identities (latent default).
	DefaultTier string `yaml:"default_tier,omitempty"`

	// IdentityTiers maps tier name to its policy bundle. Empty is the
	// latent default (no enforcement); populated tiers are
	// enforced at the LLM edge AND projected on the read-only posture
	// surface (see the type godoc above). Each entry's fields are
	// independently declared — `budget_ceiling_usd` for cost ceilings,
	// plus rate-limit + MaxTokens fields.
	IdentityTiers map[string]GovernanceTierConfig `yaml:"identity_tiers,omitempty"`
}

// GovernanceTierConfig is one tier's policy bundle.
// Each field is independently opt-in: set the cost field only to enforce
// the cost ceiling, leave the rest zero-valued for latent behaviour.
//
// `BudgetCeilingUSD` — Per-identity cost ceiling. PreCall
// blocks when the (identity, tier) accumulator total ≥ this. 0 = no
// ceiling.
//
// `RateLimit` — Per-(identity, model) token bucket. Zero-
// valued (Capacity == 0) = no rate limit.
//
// `MaxTokens` — Per-call cap. Requests whose `MaxTokens`
// exceed this fail loudly with `ErrMaxTokensExceeded`. 0 = no cap.
type GovernanceTierConfig struct {
	BudgetCeilingUSD float64                   `yaml:"budget_ceiling_usd,omitempty"`
	RateLimit        GovernanceRateLimitConfig `yaml:"rate_limit,omitempty"`
	MaxTokens        int                       `yaml:"max_tokens,omitempty"`
}

// GovernanceRateLimitConfig is the token-bucket shape.
// `Capacity` is the bucket ceiling (max reservable tokens).
// `RefillTokens` are added every `RefillInterval`. A zero `Capacity`
// disables the rate limit entirely.
type GovernanceRateLimitConfig struct {
	Capacity       int           `yaml:"capacity,omitempty"`
	RefillTokens   int           `yaml:"refill_tokens,omitempty"`
	RefillInterval time.Duration `yaml:"refill_interval,omitempty"`
}

// Reserved sub-structs. Each owning phase will populate fields and
// validators. Until then the struct is zero-valued and the loader
// passes them through unchanged.

// RuntimeConfig is owned by runtime/* phases (engine, streaming,
// cancellation, backpressure).
type RuntimeConfig struct{}

// MemoryConfig is owned by the memory subsystem phases.
//
// `Driver` selects a `memory.MemoryStore` driver. Harbor ships
// only `"inmem"`; Harbor adds `"sqlite"` and `"postgres"`. Default
// `inmem`.
//
// `DSN` is required when `Driver` is `"sqlite"` or `"postgres"`.
// The format mirrors the StateStore + ArtifactStore drivers (bare
// file path or `file:` URI for SQLite; libpq-compatible connection
// string for Postgres). `secret:"true"` redacts the value in
// audit-redacted logs.
//
// `Strategy` selects the memory shape: `"none"`, or
// `"truncation"` / `"rolling_summary"`. Default `none`.
// `memory.Open` rejects strategies the configured driver does not
// implement with `ErrStrategyNotImplemented`.
//
// `BudgetTokens` is the truncation / rolling-summary budget cap
// (token estimate). Zero means "no budget" — appending is
// unbounded.
//
// `RecoveryBacklogMax` is the bounded queue size for the
// `rolling_summary` strategy's recovery loop. Default 16
// (applied by the loader when the section is omitted). Overflow
// drops oldest and emits `memory.recovery_dropped` on the bus.
// Ignored by the `none` and `truncation` strategies.
//
// `RecentTurns` is the number of most-recent conversation turns the
// `rolling_summary` strategy keeps verbatim before older turns spill
// into the rolling summary. Zero selects the strategy default
// (`strategy.FullZoneTurns`, currently 4). Ignored by the `none`
// strategy; the `truncation` strategy keeps every turn that fits the
// budget so it does not consult this knob.
//
// Restart-required (no `reload:"live"`).
type MemoryConfig struct {
	Driver             string `yaml:"driver"`
	DSN                string `yaml:"dsn,omitempty" secret:"true"`
	Strategy           string `yaml:"strategy,omitempty"`
	BudgetTokens       int    `yaml:"budget_tokens,omitempty"`
	RecoveryBacklogMax int    `yaml:"recovery_backlog_max,omitempty"`
	// RecentTurns is the verbatim recent-window size for the
	// `rolling_summary` strategy. 0 → strategy default (FullZoneTurns).
	RecentTurns int `yaml:"recent_turns,omitempty"`

	// Summarizer tunes the `rolling_summary` compaction LLM: a
	// switchable model and an append-only prompt extension. Ignored by
	// the `none` and `truncation` strategies (which run no summariser).
	Summarizer MemorySummarizerConfig `yaml:"summarizer,omitempty"`

	// Retrieval is the opt-in retrieval mode. Empty (the default)
	// keeps the strategy-shaped retrieval unchanged; `"semantic"`
	// additionally embeds turns and serves similarity search
	// (`MemoryStore.SearchTurns`), COMPOSING with the configured
	// strategy — it never replaces `rolling_summary`. Requires the
	// `embeddings` block (validated; no stub fallback).
	Retrieval string `yaml:"retrieval,omitempty"`
	// RetrievalTopK caps how many scored turns a semantic
	// `SearchTurns` returns when the caller passes no limit. 0 uses
	// the memory subsystem default (5). Ignored unless
	// `retrieval: semantic`.
	RetrievalTopK int `yaml:"retrieval_top_k,omitempty"`
	// RetrievalMinScore is the cosine-similarity floor for semantic
	// recall: a scored turn must meet or exceed this value to be
	// injected into the prompt. Valid range [-1, 1]. Default 0.0
	// (turns with negative similarity, i.e. anti-correlated with the
	// query, are filtered out while marginally-similar turns are
	// admitted). Ignored unless `retrieval: semantic`.
	RetrievalMinScore float64 `yaml:"retrieval_min_score,omitempty"`
}

// MemorySummarizerConfig tunes the `rolling_summary` strategy's
// compaction LLM. Both fields are optional and apply only when
// `memory.strategy: rolling_summary`.
//
// `Model`, when set, pins the model the compaction summariser requests
// (routed through the summariser's model override) so operators can run
// compaction on a cheaper/faster model independent of the planner's
// model. Empty selects the main LLM's default model — today's behavior.
// A model with no matching `model_profiles` entry fails at runtime the
// same way any unsupported model does; it is not rejected at load time.
//
// `Prompt`, when set, is APPENDED to the baseline summariser system
// prompt behind an explicit "extend, do not override" separator — it
// never replaces the baseline role framing or conciseness guarantees.
// Empty leaves the baseline prompt exactly as shipped (no behavior
// change).
type MemorySummarizerConfig struct {
	Model  string `yaml:"model,omitempty"`  // "" → main LLM default model
	Prompt string `yaml:"prompt,omitempty"` // "" → baseline only; else appended to the baseline summariser prompt
}

// SkillsConfig is owned by the skills subsystem phases.
//
// `Driver` selects a `skills.SkillStore` driver. Harbor ships the
// `"localdb"` driver only; a later phase adds `"portico"`.
// Default `localdb`.
//
// `DSN` is required when `Driver` is `"localdb"`. The format mirrors
// the StateStore + MemoryStore drivers (bare file path or `file:`
// URI for SQLite; `:memory:` honoured for tests). `secret:"true"`
// redacts the value in audit-redacted logs.
//
// `Directory` shapes the Phase-39 virtual directory the run loop
// injects as the per-turn `<skills_context>` prompt block (
// ). All fields optional; restart-required.
type SkillsConfig struct {
	Driver    string                `yaml:"driver,omitempty"`
	DSN       string                `yaml:"dsn,omitempty" secret:"true"`
	Directory SkillsDirectoryConfig `yaml:"directory,omitempty"`

	// Retrieval is the opt-in retrieval mode for `Search` /
	// `skill_search`. Empty (the default) keeps the token-savvy
	// FTS5 → regex → exact ladder; `"semantic"` ranks by embedding
	// similarity over the identity-scoped catalog instead (result
	// path `"semantic"`). Requires the `embeddings` block (validated;
	// no stub fallback). Capability filtering, redaction, and the
	// budgeter apply unchanged on top.
	Retrieval string `yaml:"retrieval,omitempty"`
}

// SkillsDirectoryConfig configures the skills virtual directory
// the run loop consumes as the `<skills_context>`
// producer. The injected block is a bounded,
// stable, pinned-then-recent browse window — identity-scoped,
// capability-filtered, redacted; per-query relevance retrieval stays
// the LLM's job via the `skill_search` meta-tool.
//
//   - `Pinned` anchors the named skills at the top of every view, in
//     declaration order. Pinning is an ORDERING preference — pinned
//     skills are never exempt from the capability filter.
//   - `MaxEntries` caps the view length. 0 (the default) falls back
//     to `planner.skills_context_max`'s resolved value (default 5)
//     so the pre-111d injection-budget knob keeps its meaning;
//     explicit values must sit in [1, 200].
//   - `Selection` orders the unpinned remainder:
//     `pinned_then_recent` (default — UpdatedAt DESC).
//     `pinned_then_top` (UseCount DESC) is a canonical library-level
//     Selection but is REJECTED by the operator validator until a
//     production usage-bump path lands — no shipped path increments
//     UseCount, so the ordering would silently degrade to
//     alphabetical.
type SkillsDirectoryConfig struct {
	Pinned     []string `yaml:"pinned,omitempty"`
	MaxEntries int      `yaml:"max_entries,omitempty"`
	Selection  string   `yaml:"selection,omitempty"`
}

// TasksConfig configures the TaskRegistry driver and the
// backgroundtasks-config knobs. `Driver` selects the registered
// driver name; Harbor ships only `"inprocess"`.
//
// `RetainTurnTimeout` is the maximum time the runtime engine will
// block a foreground turn waiting for retain-turn groups to resolve.
// Defaults to 5 minutes (RFC §6.8); zero or negative values are
// rejected by validation. Consumed by the engine wiring scheduled
// for the runtime↔tasks integration; validated today so an
// operator's deployment is rejected for an invalid value even
// before the consumer lands.
//
// `ContinuationHopLimit` caps the number of background-continuation
// hops a planner runtime may take before requiring user
// confirmation. Defaults to 8 (RFC §6.8); zero or negative values
// are rejected by validation. Consumed by the planner concretes
// (the planner runtime); same "validate today, consume later" pattern as
// `RetainTurnTimeout`.
//
// Restart-required (no `reload:"live"` tag).
type TasksConfig struct {
	Driver               string        `yaml:"driver"`
	RetainTurnTimeout    time.Duration `yaml:"retain_turn_timeout"`
	ContinuationHopLimit int           `yaml:"continuation_hop_limit"`
}

// DistributedConfig configures Harbor's distributed contracts (the
// MessageBus + RemoteTransport seams). `BusDriver` selects the
// MessageBus driver (`loopback` default, or `durable`); `RemoteDriver`
// selects the RemoteTransport driver (`loopback` default, or `a2a`).
// Restart-required (no `reload:"live"`).
type DistributedConfig struct {
	BusDriver    string `yaml:"bus_driver"`
	RemoteDriver string `yaml:"remote_driver"`
	// BusPollInterval tunes how often the durable bus driver scans the
	// shared StateStore for envelopes published by other instances (or
	// left by a crash) to project onto the local event bus. Optional;
	// the durable driver applies a built-in default when unset (<= 0).
	// Ignored by the loopback driver.
	BusPollInterval time.Duration `yaml:"bus_poll_interval,omitempty"`
}

// SessionsConfig configures the SessionRegistry's GC sweeper. Defaults
// match RFC §6.9: idle TTL 24h, hard cap 30 days, sweep every 15 min.
// Fields are not hot-reloadable in V1 (changing GC cadence at runtime
// would race with the sweeper goroutine).
type SessionsConfig struct {
	IdleTTL       time.Duration `yaml:"idle_ttl"`
	HardCap       time.Duration `yaml:"hard_cap"`
	SweepInterval time.Duration `yaml:"sweep_interval"`
}

// PauseResumeConfig configures the pause lifecycle (RFC §3.3 + §6.3).
//
// `MaxParkDuration` is the ceiling on how long a pause may stay parked
// before the runtime's pause sweeper resumes it with the typed
// `timeout` Decision (`pause.resumed`) and the waiting run
// terminates as a constraints-conflict. Zero (the default) means
// pauses never expire and the sweeper is not started — the pre-111c
// behaviour. Negative values are rejected by validation.
//
// `SweepInterval` is the sweeper's scan cadence. Default 1m; 0 means
// the default applies (the block is off-by-default, so a hand-built
// Config without it stays valid); negative values are rejected. When
// expiry is enabled it must not exceed `MaxParkDuration` (a pause
// must not overstay its deadline by more than one sweep).
//
// Fields are not hot-reloadable in V1 (changing the sweep cadence at
// runtime would race with the sweeper goroutine — same posture as
// SessionsConfig).
type PauseResumeConfig struct {
	MaxParkDuration time.Duration `yaml:"max_park_duration"`
	SweepInterval   time.Duration `yaml:"sweep_interval"`
}

// ArtifactsConfig configures the ArtifactStore driver, the
// filesystem-driver root path, the SQL-driver connection string, and
// the heavy-output threshold above which the runtime mandatorily
// routes payloads through the store.
//
// `Driver` selects an artifacts driver. V1 ships five drivers:
// `inmem` (the floor; per-process lifetime, no persistence), `fs`
// (single-binary production target), `sqlite` (SQLite-
// backed, durable across restart), `postgres` (
// Postgres-backed, durable across restart, multi-replica safe), and
// `s3` (S3-compatible object-store-backed, durable;
// presigned-URL `GetRef` via the optional `Presigner` capability).
// Default `inmem`.
//
// `FSRoot` is required when `Driver == "fs"`; it is the root
// directory under which `<root>/<tenant>/<user>/<session>/<task>/
// <namespace>/<id>` blobs land. The directory is created
// (`os.MkdirAll`) at driver `New` time.
//
// `DSN` is required when `Driver` is `"sqlite"` or `"postgres"`.
// Format:
//   - SQLite: a bare file path (e.g. `/var/lib/harbor/artifacts.sqlite`)
//     or the `:memory:` sentinel (degenerate dev case).
//   - Postgres: a standard URL form
//     (`postgres://user:pass@host:5432/db?sslmode=disable`) or pgx
//     key-value form.
//
// `HeavyOutputThresholdBytes` is the byte size at which the runtime
// mandatorily routes a payload through the ArtifactStore. Default
// 32 KB (RFC §6.10). Per-tool overrides land at via
// the tool catalog; the field is the runtime-wide default. Consumed
// by the tool dispatcher and the LLM-edge catch-all
// validated today so an operator's deployment is
// rejected for an invalid value even before the consumers land.
//
// S3* fields configure the S3-style driver (AWS S3 / MinIO /
// Cloudflare R2 / any S3-compat backend). `S3Bucket` is required when
// `Driver == "s3"`. `S3Region` defaults to "us-east-1" when unset.
// `S3Endpoint` is the base URL for non-AWS backends (MinIO / R2);
// leave empty to use AWS's default endpoint resolution. `S3Prefix` is
// an optional path prefix that lets multiple Harbor deployments share
// one bucket. `S3AccessKeyID` and `S3SecretAccessKey` are optional —
// when both are empty the SDK's default credential chain is used
// (AWS_*, IRSA, instance metadata, etc.). `S3UsePathStyle` defaults
// to false (AWS native); flip on for MinIO / older R2 endpoints.
type ArtifactsConfig struct {
	Driver                    string `yaml:"driver"`
	FSRoot                    string `yaml:"fs_root,omitempty"`
	DSN                       string `yaml:"dsn,omitempty" secret:"true"`
	HeavyOutputThresholdBytes int    `yaml:"heavy_output_threshold_bytes,omitempty"`
	S3Bucket                  string `yaml:"s3_bucket,omitempty"`
	S3Endpoint                string `yaml:"s3_endpoint,omitempty"`
	S3Region                  string `yaml:"s3_region,omitempty"`
	S3Prefix                  string `yaml:"s3_prefix,omitempty"`
	S3AccessKeyID             string `yaml:"s3_access_key_id,omitempty" secret:"true"`
	S3SecretAccessKey         string `yaml:"s3_secret_access_key,omitempty" secret:"true"`
	S3UsePathStyle            bool   `yaml:"s3_use_path_style,omitempty"`
}

// EventsConfig configures the event bus driver and its in-process
// limits. filled the previously-reserved slot with the
// inmem driver's defaults; Harbor adds ReplayBufferSize for the
// in-memory ring buffer that backs Replayer. Harbor registers the
// `durable` driver and adds the two optional StateStore-selection
// fields below without changing the existing field shape.
//
// ReplayBufferSize=0 disables replay entirely on the inmem driver
// (Replay returns ErrReplayUnavailable immediately). The default
// applied by Load is 10000. On the `durable` driver ReplayBufferSize
// sizes the best-effort fallback ring used ONLY when no StateStore is
// configured.
//
// StateDriver / StateDSN select the StateStore the `durable` driver
// persists events through. They are OPTIONAL and ignored
// by every other driver: when Driver=="durable" and StateDriver is
// empty, the durable driver auto-degrades to a best-effort in-memory
// ring buffer and emits a loud runtime.warning — replay is
// then NOT durable across restarts. StateDSN is required whenever
// StateDriver is a non-inmem driver (sqlite / postgres), mirroring
// StateConfig's driver/DSN pairing.
type EventsConfig struct {
	Driver                   string        `yaml:"driver"`
	MaxSubscribersPerSession int           `yaml:"max_subscribers_per_session"`
	SubscriberBufferSize     int           `yaml:"subscriber_buffer_size"`
	IdleTimeout              time.Duration `yaml:"idle_timeout"`
	DropWindow               time.Duration `yaml:"drop_window"`
	ReplayBufferSize         int           `yaml:"replay_buffer_size"`
	StateDriver              string        `yaml:"state_driver,omitempty"`
	StateDSN                 string        `yaml:"state_dsn,omitempty" secret:"true"`
}

// AuditConfig is owned by the audit subsystem.
type AuditConfig struct{}

// ToolsConfig is owned by the tools subsystem.
// The block is optional — operators who don't attach external tool
// sources omit it entirely.
//
// `HTTPManifests` lists paths to UTCP-style YAML manifests for
// the HTTP driver. The boot-path loader is NOT yet wired —
// no production path calls `LoadManifest` / `RegisterManifest` —
// so `Validate` REJECTS a non-empty list rather than letting a
// populated knob silently do nothing (§13; SDK friction audit,
// docs/notes/sdk-friction-audit.md §1). An empty list is valid and
// is what the shipped examples carry. HTTP tools are registered
// programmatically via the driver until the loader lands.
//
// `MCPServers` lists the MCP southbound attachments. Each
// entry boots a `*mcp.Provider` whose discovered tools / resources
// / prompts are merged into the runtime catalog.
//
// `A2APeers` lists the A2A peers. The wire driver
// (`internal/distributed/drivers/a2a`) reads this slice at
// construction.
//
// `Entries` lists per-tool catalog wiring declarations: operators
// attach approval policies and / or OAuth bindings to a tool name
// without writing Go wiring code. The catalog
// builder reads this list at boot and auto-wraps each named tool's
// descriptor with the declared middleware. An entry whose Name does
// not resolve to a registered tool fails the catalog build loud
// (§13 "fail loudly at boot"); an entry that names an unknown
// approval policy or OAuth provider also fails loud.
//
// Restart-required (no `reload:"live"` tag): adding / removing tool
// providers at runtime is a later Protocol-surface concern.
//
// `OAuthProviders` closes the deferred "OAuth provider construction"
// gap (issue #116). Each entry declares one named OAuth provider
// resolved at boot through the `internal/tools/auth` driver registry
// (§4.4 seam). The V1 default driver is `oauth2` (generic OAuth2/PKCE
// Authorization Code flow). When any `tools.entries[].oauth.provider`
// references a name, that name MUST appear in `OAuthProviders` —
// validateTools enforces.
//
// `OAuthTokenKEKEnv` names the env var holding the 32-byte hex-encoded
// key-encryption key (KEK) the OAuth token store consumes for
// AES-256-GCM encryption at rest (§7). Required whenever
// `OAuthProviders` is non-empty; the dev-stack reads the env at boot
// and fails closed when the env value is empty or wrong-length.
type ToolsConfig struct {
	HTTPManifests    []string                  `yaml:"http_manifests,omitempty"`
	MCPServers       []MCPServerConfig         `yaml:"mcp_servers,omitempty"`
	A2APeers         []A2APeerConfig           `yaml:"a2a_peers,omitempty"`
	Entries          []ToolEntryConfig         `yaml:"entries,omitempty"`
	OAuthProviders   []ToolOAuthProviderConfig `yaml:"oauth_providers,omitempty"`
	OAuthTokenKEKEnv string                    `yaml:"oauth_token_kek_env,omitempty"`

	// BuiltIn lists opt-in tools shipped in the Harbor binary that the
	// runtime should register against the catalog at boot. V1.1 ships
	// two names: `clock.now` (current UTC time) and `text.echo` (echo
	// input verbatim). The names are mirrored from
	// `internal/tools/builtin.KnownNames()` via `KnownBuiltInTools()`
	// below; a typo fails validation pre-boot rather than at runtime.
	// Empty list = no built-ins registered. Restart-required (no
	// `reload:"live"` tag).
	BuiltIn []string `yaml:"built_in,omitempty"`

	// Custom lists operator-declared custom tools whose Go shell is
	// generated by `harbor scaffold`. Each entry
	// names the tool, gives a one-line description, and declares its
	// input + output shape as a flat map of `field: type` (V1.1
	// supports `string`, `integer`, `number`, `boolean`, `[]string`).
	// The scaffolder materialises one `tools/<name>.go` stub + matching
	// `tools/<name>_test.go` per entry; the rendered Go contains the
	// typed Input/Output structs and a `Handle` stub the operator fills
	// in. The runtime does NOT auto-discover these tools — the operator
	// imports the generated `tools/` package + calls `RegisterTools`
	// from the agent's bootstrap. Restart-required (no `reload:"live"`).
	Custom []CustomToolConfig `yaml:"custom,omitempty"`

	// GrantedScopes is the operator-declared list of authorization
	// scopes the dev binary's planner-facing catalog view treats as
	// granted. The runtime catalog projects only tools whose
	// `AuthScopes` are entirely contained in this set; tools that
	// require a missing scope are invisible to the planner.
	//
	// the field closes the runloop's
	// `nil /* TODO */` hard-code that left the catalog
	// unfiltered. Empty list = no scopes granted (the existing latent
	// default — tools that declare AuthScopes are invisible). The
	// scopes are operator-defined per their tool sources; the
	// validator only asserts that each entry is a non-empty string,
	// with no allowlist.
	//
	// Restart-required (no `reload:"live"` tag — scope changes
	// trigger a re-evaluation of every catalog descriptor).
	GrantedScopes []string `yaml:"granted_scopes,omitempty"`

	// SearchCacheDSN is the SQLite DSN backing the tool
	// SearchCache (FTS5 + regex fallback). Empty means the dev binary
	// uses an in-memory cache (the V1 default — discovery state lives
	// for the process lifetime). Operators can point at an on-disk
	// path (`file:./harbor-tools.db?_pragma=...`) to persist the
	// cache across reboots. Restart-required.
	SearchCacheDSN string `yaml:"search_cache_dsn,omitempty"`

	// MCPAppHost configures the deployment-wide MCP App
	// (`io.modelcontextprotocol/ui`) host capability the runtime advertises
	// to every MCP server during the initialize handshake. The host's
	// renderable display modes do not vary per server, so this is a single
	// deployment-level block, not a per-server field. Optional; nil resolves
	// to the inline-only baseline (the mode the Console renders today).
	// Restart-required.
	MCPAppHost *MCPAppHostConfig `yaml:"mcp_app_host,omitempty"`

	// MCPAddConnection gates the admin-driven runtime add of a NEW MCP
	// server connection over the Protocol
	// (`agent_config.add_mcp_connection`). Adding a stdio server spawns an
	// operator-supplied command — an RCE surface — so it is fail-closed: a
	// stdio add is permitted ONLY when its command argv[0] is listed in
	// StdioAllowlist. An empty / nil block rejects every stdio add (the
	// secure default); http adds are admin-scoped but not gated here.
	// Optional. Restart-required.
	MCPAddConnection *MCPAddConnectionConfig `yaml:"mcp_add_connection,omitempty"`
}

// MCPAddConnectionConfig declares the operator allowlist for the
// admin-driven runtime add of a NEW MCP server connection. Adding a stdio
// MCP server runs an operator-supplied command (an RCE surface); the add
// path (`agent_config.add_mcp_connection`) permits a stdio add ONLY when the
// command's argv[0] is exactly present in StdioAllowlist (fail-closed —
// argv-form only, never a shell). An empty / nil allowlist rejects every
// stdio add. http adds do not consult this gate. Restart-required.
type MCPAddConnectionConfig struct {
	// StdioAllowlist is the exact set of permitted stdio command binaries /
	// paths (matched on argv[0]). Empty rejects every stdio add.
	StdioAllowlist []string `yaml:"stdio_allowlist,omitempty"`
}

// MCPAppHostConfig declares the MCP App display modes this Harbor host can
// render. The runtime advertises these in the `io.modelcontextprotocol/ui`
// client capability during every MCP initialize handshake so a server can
// tailor the app references it returns to what the host actually renders.
//
// `DisplayModes` is the closed set `inline` / `fullscreen` / `pip` (the
// ext-apps `McpUiDisplayMode` values). Empty or nil resolves to the
// inline-only baseline. Restart-required.
type MCPAppHostConfig struct {
	DisplayModes []string `yaml:"display_modes,omitempty"`
}

// MCPAppHostDisplayModes resolves the MCP App display modes the host
// advertises. A nil MCPAppHost block, or one with no display modes, yields
// the inline-only baseline — the mode the Console renders out of the box.
// The boot loader calls this once and threads the result into every MCP
// provider's host-capability advertisement.
func (t ToolsConfig) MCPAppHostDisplayModes() []string {
	if t.MCPAppHost == nil || len(t.MCPAppHost.DisplayModes) == 0 {
		return []string{"inline"}
	}
	return append([]string(nil), t.MCPAppHost.DisplayModes...)
}

// CustomToolConfig declares one operator-defined custom tool whose Go
// shell is generated by `harbor scaffold`. Each
// entry yields one `tools/<name>.go` stub + matching test file in the
// scaffolded project.
//
// Fields:
//   - `Name` — the catalog tool name (also drives the generated file
//     name + Go function name). Required. Unique within `Custom`; no
//     collision with `BuiltIn`. Operator-friendly form: lowercase +
//     `.` separators are allowed (e.g. `weather.lookup`).
//   - `Description` — one-line summary, surfaced in the generated
//     Go comment + the planner-facing tool catalog. Required.
//   - `Input` — flat map of field name → type. Generates the typed
//     Go input struct. Type allowlist (V1.1): `string` / `integer` /
//     `number` / `boolean` / `[]string`. Required (may be empty when
//     the tool takes no arguments).
//   - `Output` — flat map of field name → type. Same allowlist as
//     `Input`. Required (may be empty when the tool returns nothing
//     observable beyond success).
//
// Restart-required.
type CustomToolConfig struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Input       map[string]string `yaml:"input,omitempty"`
	Output      map[string]string `yaml:"output,omitempty"`
}

// ToolOAuthProviderConfig declares one operator-configured OAuth
// provider (closes issue #116 and the deferred construction
// gap). Each entry resolves to a self-registered driver in
// `internal/tools/auth/drivers/<name>/` via the §4.4 seam pattern. The
// constructed `auth.OAuthProvider` is keyed by `Name` in the catalog
// builder's `Deps.OAuthProviders` map; `tools.entries[].oauth.provider`
// references the same `Name`.
//
// The V1 default driver is `oauth2` — generic OAuth2/PKCE Authorization
// Code flow. Future flow types (device-code, client-credentials,
// per-vendor extensions) add a new driver under
// `internal/tools/auth/drivers/<name>/` without changing this shape.
//
// Credentials enter via env-var indirection (§7 rule 2 — never
// hardcoded, never logged). `ClientIDEnv` / `ClientSecretEnv` name the
// env vars; the driver resolves `os.Getenv` at construction and fails
// closed when either is empty.
//
// Layout in YAML:
//
//	tools:
//	  oauth_token_kek_env: HARBOR_OAUTH_TOKEN_KEK
//	  oauth_providers:
//	    - name: github
//	      driver: oauth2
//	      client_id_env: GITHUB_OAUTH_CLIENT_ID
//	      client_secret_env: GITHUB_OAUTH_CLIENT_SECRET
//	      scopes: ["repo", "read:user"]
//	      auth_url: https://github.com/login/oauth/authorize
//	      token_url: https://github.com/login/oauth/access_token
//	      redirect_url: https://example.com/oauth/callback
//
// Fields:
//   - `Name` — operator-facing identifier (must be unique across the
//     slice; referenced by `tools.entries[].oauth.provider`).
//   - `Driver` — names a self-registered driver under
//     `internal/tools/auth/drivers/<name>/`. Required. Unknown driver
//     names fail validation with the registered-driver list in the
//     error message.
//   - `ClientIDEnv` / `ClientSecretEnv` — env-var names the driver
//     reads at construction. Both required.
//   - `Scopes` — requested OAuth scopes. Optional.
//   - `AuthURL` / `TokenURL` — authorization-server endpoints. Used by
//     the generic `oauth2` driver; driver-specific drivers may ignore.
//   - `RedirectURL` — the redirect URI the operator hosts (the Harbor
//     Protocol callback handler). Required for the `oauth2` driver.
//   - `Extra` — driver-specific extras map. Reserved for future
//     drivers' per-flow knobs (e.g. device-code's verification URI,
//     vendor-specific tenant ID). Unused by the V1 `oauth2` driver.
//
// Restart-required.
type ToolOAuthProviderConfig struct {
	Name            string            `yaml:"name"`
	Driver          string            `yaml:"driver"`
	ClientIDEnv     string            `yaml:"client_id_env"`
	ClientSecretEnv string            `yaml:"client_secret_env"`
	Scopes          []string          `yaml:"scopes,omitempty"`
	AuthURL         string            `yaml:"auth_url,omitempty"`
	TokenURL        string            `yaml:"token_url,omitempty"`
	RedirectURL     string            `yaml:"redirect_url,omitempty"`
	Extra           map[string]string `yaml:"extra,omitempty"`
}

// ToolEntryConfig is one per-tool catalog wiring declaration.
// The shape is intentionally small: the catalog builder
// looks up the registered tool by `Name`, then applies whichever of
// `Approval` and / or `OAuth` are populated.
//
// Layout in YAML:
//
//	tools:
//	  entries:
//	    - name: delete_doc
//	      approval:
//	        policy: deny-all
//	        reason: "deletion requires human review"
//	    - name: github.repo.read
//	      oauth:
//	        provider: github
//	        binding_scope: user
//	    - name: write_to_prod
//	      approval:
//	        policy: tagged
//	        require_tags: ["sensitive", "write:prod"]
//	      oauth:
//	        provider: prod-api
//	        binding_scope: agent
//
// An empty `Approval` AND `OAuth` block is rejected at validation
// time (an entry with no middleware to apply is a configuration
// typo).
//
// Restart-required.
type ToolEntryConfig struct {
	// Name is the catalog tool name the entry applies to. Required.
	// The catalog builder fails closed when no tool registered with
	// this name resolves at boot.
	Name string `yaml:"name"`
	// LoadingMode controls when this tool appears in the planner's
	// prompt-time catalog. "" or "always" = every turn (default).
	// "deferred" = hidden by default; the LLM discovers via meta-tools.
	LoadingMode string `yaml:"loading_mode,omitempty"`
	// Approval declares an approval-gate wiring for this tool. Omit
	// for tools that need no gating. When present, `Approval.Policy`
	// MUST be one of the canonical policy names; an unknown value
	// fails closed.
	Approval *ToolApprovalConfig `yaml:"approval,omitempty"`
	// OAuth declares an OAuth binding for this tool. Omit for tools
	// that need no OAuth. When present, `OAuth.Provider` MUST name a
	// configured OAuth source and `OAuth.BindingScope` MUST be one of
	// "user" / "agent".
	OAuth *ToolOAuthConfig `yaml:"oauth,omitempty"`
}

// ToolApprovalConfig declares an approval-gate wiring for one tool.
type ToolApprovalConfig struct {
	// Policy names which bundled approval policy to apply. The
	// catalog builder maps this name onto a concrete
	// `approval.ApprovalPolicy` instance. Allowed values:
	//   - "deny-all"      → `approval.AlwaysDenyPolicy`
	//   - "approve-all"   → `approval.AlwaysApprovePolicy` (dev only)
	//   - "tagged"        → `approval.TaggedPolicy` (consults
	//                       `RequireTags` below)
	// An unknown policy value fails the catalog build with a wrapped
	// error naming the offending value.
	Policy string `yaml:"policy"`
	// Reason is the operator-facing classification carried on
	// `tool.approval_requested`. Optional — the bundled policies
	// supply a sensible default.
	Reason string `yaml:"reason,omitempty"`
	// RequireTags is consulted by the `tagged` policy. An entry whose
	// `Policy: tagged` AND `RequireTags: []` is rejected — the
	// tagged policy with no tags is a no-op (configuration smell).
	RequireTags []string `yaml:"require_tags,omitempty"`
}

// ToolOAuthConfig declares an OAuth binding for one tool. It
// /.
type ToolOAuthConfig struct {
	// Provider names the OAuth source the tool binds to. The catalog
	// builder consults the configured OAuth registry; a name that
	// resolves to no source fails the catalog build loud.
	Provider string `yaml:"provider"`
	// BindingScope is "user" or "agent". An
	// invalid value fails the catalog build with a wrapped error
	// naming the offending value.
	BindingScope string `yaml:"binding_scope"`
}

// MCPServerConfig is one MCP southbound attachment. `Name` is the
// source-id prefix (must be unique across servers); `TransportMode`
// selects the wire transport (`auto` / `sse` / `streamable_http` /
// `stdio`); `URL` is required for HTTP-flavoured transports;
// `Command` is required for stdio (argv form ONLY — see
// `internal/tools/drivers/mcp/transport_stdio.go` for the §7
// security rule). `Headers` are operator-supplied auth headers
// (treated as secrets for redaction). `KeepAlive` is the
// session-ping interval; zero disables.
//
// Restart-required.
type MCPServerConfig struct {
	Name          string            `yaml:"name"`
	TransportMode string            `yaml:"transport_mode"`
	URL           string            `yaml:"url,omitempty"`
	Command       []string          `yaml:"command,omitempty"`
	Headers       map[string]string `yaml:"headers,omitempty" secret:"true"`
	KeepAlive     time.Duration     `yaml:"keep_alive,omitempty"`
	// Policy is the per-server default tool reliability policy.
	// Optional; nil preserves today's behaviour exactly
	// (every tool inherits `tools.DefaultPolicy()` — 30 s per-attempt
	// deadline / 4 total attempts). When set, it projects to the
	// `mcpdrv.Config.DefaultPolicy` applied to every tool this server
	// registers. Per-field zero values fall through to
	// `tools.DefaultPolicy()` (see `ToolPolicyConfig`). Restart-required.
	Policy *ToolPolicyConfig `yaml:"policy,omitempty"`
	// ToolPolicies are per-tool overrides keyed by the MCP tool's name
	// (the server-side name, NOT the `<source>_<tool>` Harbor-facing
	// name). A tool named here uses the override instead of `Policy`
	// (or the package default). Optional; empty preserves today's
	// behaviour. Restart-required.
	ToolPolicies map[string]ToolPolicyConfig `yaml:"tool_policies,omitempty"`
	// OAuthProvider names a declared `tools.oauth_providers[]` entry to
	// bind for per-identity southbound OAuth: every identity-stamped
	// per-call RPC to this server resolves a fresh bearer for the calling
	// identity and injects `Authorization: Bearer <tok>`. The value is a
	// NON-SECRET provider NAME — it selects a config-declared acquisition
	// strategy; the secret stays env-indirected on the provider entry.
	// Optional; empty leaves the connection on its static `headers`.
	// Validation rejects an unknown name, a binding on a stdio transport
	// (no HTTP request to inject into), and a static `Authorization` header
	// alongside a binding (one auth mode per connection). Restart-required.
	OAuthProvider string `yaml:"oauth_provider,omitempty"`
	// MetaAnnotations is a static, NON-SECRET set of operator-declared
	// key/values merged verbatim into the MCP `_meta` map on every
	// identity-stamped per-call RPC — the deployment's own attribution
	// vocabulary, passed to the server as-is. Reserved keys (`tenant`,
	// `user`, `session`, `agent_id`, `traceparent`, `tracestate`, and any
	// `io.modelcontextprotocol/`-prefixed key) and empty keys are rejected
	// at validation. Optional. Restart-required.
	MetaAnnotations map[string]string `yaml:"meta_annotations,omitempty"`
}

// ToolPolicyConfig is the operator-facing YAML projection of
// `tools.ToolPolicy`. It is the single config→policy
// translation surface (CLAUDE.md §13): the projection helper in
// `policy_projection.go` is the only code that maps these fields onto
// a `tools.ToolPolicy`. There is no second policy definition.
//
// Field semantics (all optional; a zero/omitted field falls through
// to `tools.DefaultPolicy()` per-field via `tools.ToolPolicy`'s own
// zero-value resolution):
//
//   - MaxAttempts is the TOTAL number of attempts including the first
//     (operators think in total attempts, not retries). It projects to
//     `tools.ToolPolicy.MaxRetries = MaxAttempts - 1`. `1` means a
//     single attempt with no retry; `0`/omitted falls through to the
//     default 4 total attempts (3 retries).
//   - TimeoutMS is the per-attempt deadline in milliseconds. `0`/
//     omitted falls through to the default 30000.
//   - RetryOn is the retryable error-class allowlist. Each value must
//     be a known `tools.ErrorClass` string (`transient` / `timeout` /
//     `5xx` / `permanent`); validation rejects unknown classes. Empty/
//     omitted falls through to the default `[transient timeout 5xx]`.
//   - BackoffBaseMS / BackoffMaxMS are the backoff base / cap in
//     milliseconds; `0`/omitted fall through to the defaults
//     (100 ms / 30 s). BackoffMult is the per-retry multiplier;
//     `0`/omitted falls through to the default 2.
type ToolPolicyConfig struct {
	MaxAttempts   int      `yaml:"max_attempts,omitempty"`
	TimeoutMS     int      `yaml:"timeout_ms,omitempty"`
	RetryOn       []string `yaml:"retry_on,omitempty"`
	BackoffBaseMS int      `yaml:"backoff_base_ms,omitempty"`
	BackoffMult   float64  `yaml:"backoff_mult,omitempty"`
	BackoffMaxMS  int      `yaml:"backoff_max_ms,omitempty"`
}

// A2APeerConfig declares an A2A peer the southbound driver may connect
// to. URL is required; the driver rejects HTTP schemes unless the host
// is loopback or `AllowInsecureLoopback` is true (AGENTS.md §7).
//
// `TrustTier` is an operator-set integer in [1, 5] (1 = third-party,
// 5 = first-party). The route-scoring registry uses this to rank
// peers when more than one declares the same capability.
//
// `LatencyTierMS` is an operator hint at the peer's expected p50
// latency in milliseconds. Smaller values rank higher (latency is
// the tie-breaker among similarly-trusted peers).
//
// `AllowInsecureLoopback` opts a loopback HTTP peer into the driver.
// The flag is name-checked against loopback only — a non-loopback
// HTTP host is still rejected regardless. Restart-required.
//
// `AgentCardTTL` overrides the driver-level AgentCard cache TTL.
// Zero falls back to the driver default (10 minutes).
type A2APeerConfig struct {
	URL                   string        `yaml:"url"`
	TrustTier             int           `yaml:"trust_tier"`
	LatencyTierMS         int           `yaml:"latency_tier_ms"`
	AllowInsecureLoopback bool          `yaml:"allow_insecure_loopback,omitempty"`
	AgentCardTTL          time.Duration `yaml:"agent_card_ttl,omitempty"`
}

// ProtocolConfig is owned by the protocol-server phases.
//
// `MaxRequestBytes` is the upper bound on an
// `artifacts.put` upload body. A body above this is rejected with the
// canonical `CodeRequestTooLarge` / HTTP 413 — fail loud, never silently
// truncate. Optional; a zero value resolves to `DefaultMaxRequestBytes`
// (4 MiB) at load time. Operators with a larger upload need adjust this
// key and the Console's browser-side size hint accordingly.
type ProtocolConfig struct {
	MaxRequestBytes int `yaml:"max_request_bytes,omitempty"`
}

// DefaultMaxRequestBytes is the `ProtocolConfig.MaxRequestBytes` value
// applied when the operator configured none — 4 MiB, the spec's
// "moderate-size upload" posture for the artifacts.put
// pipeline.
const DefaultMaxRequestBytes = 4 << 20

// ResolvedMaxRequestBytes returns the configured `MaxRequestBytes`,
// substituting `DefaultMaxRequestBytes` when the field is zero or
// negative. The protocol-server boot path calls this so a fresh config
// produces a working upload bound with zero ceremony.
func (c ProtocolConfig) ResolvedMaxRequestBytes() int {
	if c.MaxRequestBytes <= 0 {
		return DefaultMaxRequestBytes
	}
	return c.MaxRequestBytes
}

// CLIConfig is owned by the CLI phases.
//
// `DevHotReload` configures the `harbor dev` hot-reload
// watcher: fsnotify-driven graceful-drain restart when a watched file
// changes. The block is opt-out (`Enabled: true` is the default applied
// by the loader). Restart-required at the harbor-dev level (a change to
// the block takes effect on the next `harbor dev` boot).
type CLIConfig struct {
	DevHotReload DevHotReloadConfig `yaml:"dev_hot_reload,omitempty"`
}

// DevHotReloadConfig configures the `harbor dev`
// hot-reload watcher. The block is opt-out — the loader populates
// defaults that match the §13 "test stubs as production defaults"
// amendment for dev seams: on by default, fail-loud at boot when the
// configured watch root is unreadable, never silently disabled.
//
// Fields:
//
//   - `Enabled` — `*bool` so the loader can distinguish "operator didn't
//     set the field" (nil → defaults to true) from "operator explicitly
//     disabled" (&false). The CLI flag `harbor dev --no-hot-reload` is
//     the operator-facing escape hatch — it overrides this value at
//     boot. Default: true.
//   - `Policy` — retain-in-flight policy on a triggered restart:
//     `"drain"` (wait for in-flight RunLoops up to `DrainTimeout` before
//     forcing close; the default), `"cancel"` (immediately cancel
//     in-flight; no drain), or `"disabled"` (no hot-reload — equivalent
//     to `Enabled: false`). Unknown values fail validation.
//   - `DrainTimeout` — bounds the `drain` policy's wait for in-flight
//     RunLoops. Zero / negative rejected. Default: 5s.
//   - `WatchRoots` — list of paths (absolute or relative to the working
//     directory) the fsnotify watcher monitors. The dev cmd unions this
//     with the loaded config file's directory so a config edit also
//     triggers a reload. Default: `[".harbor/agents"]` — the
//     project-local draft-save directory.
//
// Restart-required (no `reload:"live"` tag): the watcher itself is the
// reload mechanism; reconfiguring the watcher at runtime would race the
// watcher's own goroutine.
type DevHotReloadConfig struct {
	Enabled      *bool         `yaml:"enabled,omitempty"`
	Policy       string        `yaml:"policy,omitempty"`
	DrainTimeout time.Duration `yaml:"drain_timeout,omitempty"`
	WatchRoots   []string      `yaml:"watch_roots,omitempty"`
}

// Canonical retain-in-flight policy values for DevHotReloadConfig.
// Centralised so cmd/harbor and the validator reference one spelling.
const (
	// DevHotReloadPolicyDrain waits for in-flight RunLoops to drain up
	// to DrainTimeout before forcing a stack restart. The default.
	DevHotReloadPolicyDrain = "drain"
	// DevHotReloadPolicyCancel cancels in-flight RunLoops immediately
	// on a triggered restart — no drain wait.
	DevHotReloadPolicyCancel = "cancel"
	// DevHotReloadPolicyDisabled is equivalent to Enabled: false; the
	// watcher is not started. Listed as a canonical value so an operator
	// can disable hot-reload via a single explicit field rather than
	// flipping the *bool.
	DevHotReloadPolicyDisabled = "disabled"
)

// PlannerConfig selects the planner concrete the runtime constructs at
// boot. The §4.4 seam pattern applied to the planner — closes the
// "future phases will read cfg.Planner" note and CLAUDE.md §1.3's
// swappable-planner property gap (closes issue #126). Mirrors
// the `ToolOAuthProviderConfig` structurally — same shape, same
// validator pattern, same registry pattern (`internal/planner` driver
// registry; `cmd/harbor/main.go` blank-imports each driver).
//
// `Driver` selects a self-registered planner driver in
// `internal/planner/<name>/`. V1 ships only the `react` driver (the
// reference LLM-driven ReAct planner —). Empty
// defaults to `react` so a missing configuration value boots with the
// V1 reference planner unchanged; operators opt into alternates
// explicitly when later phases land them (Plan-Execute, Workflow,
// Graph, Deterministic, Supervisor, MultiAgent, HumanApproval per
// RFC §6.2).
//
// `MaxSteps` overrides the driver-side circuit-breaker step cap. Zero
// (the default) means "use the driver's internal default" — e.g. the
// react driver's `react.DefaultMaxSteps` (12). The validator rejects
// negative values pre-boot.
//
// `ExtraGuidance` is operator-supplied domain-specific guidance
// injected into the rendered ReAct system prompt's
// `<additional_guidance>` section (RFC §6.2). Empty (the
// default) omits the section entirely. The string is rendered
// verbatim — the operator owns its content hygiene. It flows to the
// react driver's `WithSystemPromptExtra` Option at construction;
// other drivers ignore it. The validator imposes no rule beyond
// "string" — operator copy is operator copy.
//
// `ReasoningReplay` controls whether the ReAct planner re-injects a
// prior step's captured provider-side reasoning trace into the next
// turn's prompt. The enum has two values:
// `never` (the default for ALL models — a prior step's captured
// reasoning is never re-injected) and `text` (prepend the captured
// trace as a text block above the prior `{tool, args}` action JSON).
// Empty defaults to `never`; the validator rejects any other value
// loudly pre-boot. There is no `provider_native` mode in V1.
//
// `MaxToolExamplesPerTool` caps how many curated examples the ReAct
// prompt's `<available_tools>` section renders per tool.
// Examples are ranked `minimal` > `common` > `edge-case` >
// untagged; the renderer keeps the top N. Zero (the default) resolves
// to 3 inside the react driver. The validator rejects negative values
// loudly pre-boot. Other drivers ignore it.
//
// `ParallelToolCalls` toggles native parallel tool-call emission.
// Pointer-bool so an omitted key (nil) resolves
// to `true` — the React planner emits a native `CallParallel` when the
// LLM returns N>1 tool-calls in one response, and the dev `ToolExecutor`
// dispatches the branches concurrently. An explicit `false` selects the
// serialization fallback (one `CallTool` per step via
// `RunContext.PendingToolCalls`). It flows to the react driver's
// `WithParallelToolCalls` Option only when non-nil; other drivers ignore
// it. No validator rule beyond "bool" — both states are correct.
//
// `SkillsContextMax` caps how many skill bodies the run loop fetches
// from `skills.SkillStore.Search` and hands the planner via
// `RunContext.SkillsContext`. Zero (the default)
// resolves to `DefaultSkillsContextMax` (5) via
// `SkillsContextMaxResolved()` — the one source of the default since
// the config consolidation; the validator rejects negatives loudly pre-boot.
// Consumed by the per-task run loop (cmd + devstack + headless
// assemblies); library consumers that build their own RunContext
// resolve through the same method.
//
// `PlanningHints` is the operator-supplied per-run planning steering
// the dev run loop projects onto `RunContext.PlanningHints`.
// Renders into the ReAct prompt's `<planning_constraints>`
// via 83c. V1.1 ships only `Constraints` and `PreferredTools`; the
// richer Go-struct fields (`ParallelGroups`, `DisallowTools`,
// `Budget`) remain reachable via a custom planner Option, not via
// `harbor.yaml`. The block is omitted entirely when empty.
//
// `TokenBudget` is the trajectory-compression threshold.
// When > 0 the per-task run loop projects it onto
// `RunSpec.Base.Budget.TokenBudget` and the runtime assembly
// constructs the trajectory compression runner (the LLM-backed
// `TrajectorySummariser` over the configured LLM client); the
// steering RunLoop then invokes `MaybeCompress` at each step
// boundary, compacting an over-budget trajectory into
// `Trajectory.Summary` (one compression per run at V1.1.x). Zero (the
// default) disables compression entirely — today's behaviour. The
// validator rejects negative values loudly pre-boot. Requires a
// configured `llm` block when non-zero (the summariser needs a real
// client; fail-loud at assembly).
//
// `Extra` is the per-driver opaque extras map. Reserved for future
// drivers' per-flow knobs (e.g. a deterministic planner's scripted
// step sequence, a supervisor planner's sub-agent list). The V1 `react`
// driver ignores it.
//
// Restart-required (no `reload:"live"` tag): swapping a planner at
// runtime would race with in-flight RunLoop goroutines holding the old
// concrete.
type PlannerConfig struct {
	Driver                 string                  `yaml:"driver,omitempty"`
	MaxSteps               int                     `yaml:"max_steps,omitempty"`
	ExtraGuidance          string                  `yaml:"extra_guidance,omitempty"`
	ReasoningReplay        string                  `yaml:"reasoning_replay,omitempty"`
	MaxToolExamplesPerTool int                     `yaml:"max_tool_examples_per_tool,omitempty"`
	ParallelToolCalls      *bool                   `yaml:"parallel_tool_calls,omitempty"`
	SkillsContextMax       int                     `yaml:"skills_context_max,omitempty"`
	AbsoluteMaxSpawnDepth  int                     `yaml:"absolute_max_spawn_depth,omitempty"`
	TokenBudget            int                     `yaml:"token_budget,omitempty"`
	PlanningHints          PlannerPlanningHintsCfg `yaml:"planning_hints,omitempty"`
	Extra                  map[string]string       `yaml:"extra,omitempty"`
}

// ParallelToolCallsEnabled resolves the optional `parallel_tool_calls`
// knob. Nil (unset) resolves to `true` — native
// parallel tool-call emission is the default. A non-nil value is
// honoured verbatim. The dev stack reads this when no driver-boundary
// passthrough is wanted; the `*bool` itself flows through the planner
// boundary so the react factory can distinguish "unset" from an
// explicit `false`.
func (p PlannerConfig) ParallelToolCallsEnabled() bool {
	return p.ParallelToolCalls == nil || *p.ParallelToolCalls
}

// SpawnDepthCap resolves the optional `absolute_max_spawn_depth` knob.
// A non-positive value (unset or zero) resolves to
// `DefaultSpawnDepthCap`: a SpawnTask whose child would exceed this
// ParentTaskID-chain depth is rejected loudly so a background sub-agent
// cannot recurse without bound. The cap bounds depth, not breadth.
func (p PlannerConfig) SpawnDepthCap() int {
	if p.AbsoluteMaxSpawnDepth <= 0 {
		return DefaultSpawnDepthCap
	}
	return p.AbsoluteMaxSpawnDepth
}

// DefaultSpawnDepthCap is the ONE source of the spawn-depth default
// (deduped from the former unexported mirror pair
// `config.defaultSpawnDepthCap` / `cmd/harbor::defaultMaxSpawnDepth`).
// The tool executor's constructor clamp references this constant; no
// other literal copy of the value is allowed.
const DefaultSpawnDepthCap = 4

// DefaultHeavyOutputThresholdBytes is the ONE source of the
// heavy-output threshold default (32 KiB; RFC §6.10): the byte
// size at which the runtime promotes heavy content to artifact-backed
// stubs. `Defaults()` seeds
// `ArtifactsConfig.HeavyOutputThresholdBytes` from it; the dispatch
// executor's safety floor (`internal/runtime/dispatch`), the LLM-edge
// snapshot default (`llm.DefaultHeavyOutputThreshold`), and the search
// preview bound (`search.HeavyPreviewThreshold`) all reference this
// constant (the `DefaultSpawnDepthCap` precedent — checkpoint audit
// follow-up). No other literal copy of the value is allowed.
const DefaultHeavyOutputThresholdBytes = 32 * 1024

// SkillsContextMaxResolved resolves the optional `skills_context_max`
// knob. A non-positive value (unset or zero)
// resolves to `DefaultSkillsContextMax`. re-homed
// the zero→default resolution here from run-loop literals (the
// cmd/devstack pair each carried its own `= 5` constant — the
// duplicate-default drift mechanism).
func (p PlannerConfig) SkillsContextMaxResolved() int {
	if p.SkillsContextMax <= 0 {
		return DefaultSkillsContextMax
	}
	return p.SkillsContextMax
}

// DefaultSkillsContextMax is the ONE source of the skills-context cap
// default: how many skill bodies the run loop fetches from
// `skills.SkillStore.Search` and hands the planner via
// `RunContext.SkillsContext` when `skills_context_max` is unset.
const DefaultSkillsContextMax = 5

// MultimodalConfig is the YAML carrier of the per-agent attachment
// disposition policy. It is a thin adapter over
// the planner-homed policy core: `internal/planner` decodes the block
// into `planner.DispositionPolicy` via `DispositionPolicyFromConfig`;
// a Go consumer embedding the runtime headless constructs the policy
// value directly and never touches this block.
//
// `Disposition` maps a MIME key to a disposition value:
//
//	multimodal:
//	  disposition:
//	    "application/pdf": "tool:pdf.extract"  # force a catalog tool
//	    "image/*": inline                       # family wildcard
//	    "*": ref                                # agent-wide default
//
// Keys: an exact IANA media type (`application/pdf`), a family
// wildcard (`image/*`), or the literal `*` (the agent-wide default —
// decoded onto `DispositionPolicy.Default`). Values: `ref` (the
// runtime default for non-image MIMEs — `ArtifactStub` + `Fetch.Tool`
// hint), `inline` (DataURL inline; `image/*` only at V1.1),
// `provider_native` (opt-in provider-side understanding — the LLM
// driver uploads the attachment to the provider's file surface and
// the model sees it via an opaque `file_id`; a provider without
// support for the modality degrades to `ref` with a logged notice),
// or `tool:<name>` (force the named catalog tool via `Fetch.Tool`).
//
// Precedence: a per-attachment Protocol `disposition` hint > this
// block > the runtime default (`image/*` → inline, everything else →
// ref — byte-for-byte the pre-84b behaviour). Restart-required (no
// `reload:"live"` tag).
type MultimodalConfig struct {
	Disposition map[string]string `yaml:"disposition,omitempty"`
}

// PlannerPlanningHintsCfg is the YAML-facing subset of the planner's
// `PlanningHints`. V1.1 ships two fields; the
// richer Go-struct fields stay reachable through a custom planner
// Option, not the YAML surface. An empty struct projects to nil on
// `RunContext.PlanningHints`, so the `<planning_constraints>` prompt
// section is omitted.
type PlannerPlanningHintsCfg struct {
	// Constraints is free-form text rendered verbatim into the
	// `<planning_constraints>` section. Voice/tone rules, hard
	// negative-space guidance ("never call X without prior approval"),
	// or domain-specific constraints. Empty omits the line.
	Constraints string `yaml:"constraints,omitempty"`
	// PreferredTools lists tool names the planner should prefer when
	// multiple satisfy the same goal. Empty omits the line.
	PreferredTools []string `yaml:"preferred_tools,omitempty"`
}

// IsZero reports whether every field on the YAML config is empty —
// the dev run loop reads this to decide whether to project onto
// `RunContext.PlanningHints` (nil) or to allocate a populated struct.
func (p PlannerPlanningHintsCfg) IsZero() bool {
	return p.Constraints == "" && len(p.PreferredTools) == 0
}
