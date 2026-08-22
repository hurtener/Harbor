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

import (
	"encoding/json"
	"time"

	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
)

// Config is the root configuration. It is immutable after Load.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Identity    IdentityConfig    `yaml:"identity"`
	Telemetry   TelemetryConfig   `yaml:"telemetry"`
	Postgres    PostgresConfig    `yaml:"postgres,omitempty"`
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

	// VirtualAgents is the boot YAML declaration of the configured
	// top-level agent's optional virtual-agent profiles
	// (`virtual_agents:`). Absent (the zero value) means no profiles —
	// omission is byte-compatible with a runtime that predates the
	// feature. Optional; validated at boot.
	VirtualAgents VirtualAgentsConfig `yaml:"virtual_agents,omitempty"`

	// Observability is the observability-rollup block (HA-65): the
	// durable, indexed rollup projection backing
	// `observability.query` and the projection-backed session
	// counters. Optional; an absent block leaves the rollup surface
	// unwired and the route at 501 (the partial-build convention).
	Observability ObservabilityConfig `yaml:"observability,omitempty"`

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

// PostgresConfig is the runtime-wide connection budget shared by every
// Harbor-owned PostgreSQL projection. It applies across equal-DSN shared
// pools and distinct-DSN compatibility pools alike. Zero fields select the
// conservative Basic-4GB fleet defaults: 3 open, 1 idle, 5m lifetime, and
// 30s idle lifetime. Restart-required.
type PostgresConfig struct {
	MaxOpenConns           int           `yaml:"max_open_conns,omitempty"`
	MaxIdleConns           int           `yaml:"max_idle_conns,omitempty"`
	ConnMaxLifetime        time.Duration `yaml:"conn_max_lifetime,omitempty"`
	ConnMaxIdleTime        time.Duration `yaml:"conn_max_idle_time,omitempty"`
	MigrationMaxConcurrent int           `yaml:"migration_max_concurrent,omitempty"`
}

// StateConfig selects the StateStore driver and its connection.
type StateConfig struct {
	Driver        string          `yaml:"driver"`
	DSN           string          `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode sqlmigrate.Mode `yaml:"migration_mode,omitempty"`
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

	// CredentialSource selects WHERE the PRIMARY provider's API key is
	// sourced from — the inference-plane credential-source seam:
	//
	//   - "" / "local" (default) — the key resolves from `api_key` (an
	//     `env.NAME` reference or a literal) once at boot, exactly as
	//     today. The process environment is the custodian.
	//   - "remote" — the runtime PULLS the key from the coordinator's
	//     boot-declared inference broker named by `inference_broker`, at
	//     connect + refresh (never on the per-call hot path). The
	//     coordinator remains sole custodian; nothing is persisted.
	//
	// Brokered XOR local, validated LOUD at boot: `remote` REQUIRES a
	// non-empty `inference_broker` (resolving to a declared broker) and
	// REJECTS a set `api_key` (both set is a config error); a local source
	// REJECTS a set `inference_broker` (neither a broker nor a key is also
	// an error — a provider must declare exactly one source). Restart-
	// required; NOT a Protocol surface.
	CredentialSource string `yaml:"credential_source,omitempty"`

	// InferenceBroker names the boot-declared `inference_brokers[]` entry
	// the primary provider's key is pulled from when `credential_source`
	// is "remote". Referenced by NON-SECRET name — the pull endpoint /
	// audience / scope ceiling live on the named broker, never here and
	// never on the wire (the credential-plane invariant). Required
	// when `credential_source: remote`, rejected otherwise.
	InferenceBroker string `yaml:"inference_broker,omitempty"`

	// InferenceBrokers is the boot-declared list of NAMED inference-plane
	// credential brokers — the pinned credential SINK for a runtime's LLM
	// provider key. The credential-plane invariant (no admin-writable field
	// determines a credential sink) keeps every sink-determining value here,
	// referenced by non-secret name from `credential_source: remote` (and
	// from the `agent_config.set_llm_provider` Protocol write's zero-URL
	// descriptor). Config/file-only, restart-required — NOT a Protocol
	// surface.
	InferenceBrokers []InferenceBrokerConfig `yaml:"inference_brokers,omitempty"`
}

// InferenceBrokerConfig declares one NAMED, boot-declared inference-plane
// credential broker — the pinned credential SINK for a runtime's LLM
// provider key, the inference-plane analogue of
// [ToolOAuthCredentialBrokerConfig]. The credential-plane invariant (no
// admin-writable field determines a credential sink) keeps every
// sink-determining value HERE, never on a wire-writable descriptor. It
// pins the pull endpoint / audience / scope ceiling by non-secret NAME;
// no URL or secret ever crosses the wire.
//
// Config/file-only, restart-required — NOT a Protocol surface.
//
// Layout in YAML:
//
//	llm:
//	  inference_brokers:
//	    - name: openai-broker
//	      credential_url: https://coordinator.example.com/runtimes/self/provider-key
//	      auth_token_env: HARBOR_COORDINATOR_TOKEN
//	      audience: openai        # optional
//	      scope_ceiling: [chat]   # optional
//	      cache_ttl: 5m           # optional
//	      timeout: 10s            # optional
type InferenceBrokerConfig struct {
	// Name is the operator-facing broker identifier (unique within the
	// slice; referenced by non-secret name from `credential_source: remote`
	// and from a Protocol-installed provider descriptor). Required.
	Name string `yaml:"name"`
	// CredentialURL is the boot-pinned coordinator credential-pull endpoint
	// the runtime GETs the provider API key from. Required; must be https
	// (or a loopback host for the dev / fixture case). The pull carries the
	// runtime's own service bearer token (AuthTokenEnv), so TLS is
	// mandatory off loopback (§7). Boot-pinned: never wire-writable.
	CredentialURL string `yaml:"credential_url"`
	// AuthTokenEnv names the env var holding the runtime's OWN broker
	// credential — the `Authorization: Bearer` the runtime presents to
	// CredentialURL (§7 rule 2 — never hardcoded). Required non-empty. Read
	// lazily at fetch time so a rotated token is picked up without restart.
	AuthTokenEnv string `yaml:"auth_token_env"`
	// Audience is the boot-pinned audience ceiling for the pulled provider
	// key. Optional — an LLM provider key is not always audience-shaped;
	// when present it is enforced as the credential-plane sink pin, when absent the
	// broker's own binding governs. Boot-pinned, never wire-writable.
	Audience string `yaml:"audience,omitempty"`
	// ScopeCeiling is the boot-pinned scope ceiling for the pulled provider
	// key. Optional (see Audience). Boot-pinned, never wire-writable.
	ScopeCeiling []string `yaml:"scope_ceiling,omitempty"`
	// CacheTTL caps the in-memory serve horizon for a brokered provider
	// key. Optional; zero = driver default.
	CacheTTL time.Duration `yaml:"cache_ttl,omitempty"`
	// Timeout bounds a single credential pull. Optional; zero = driver
	// default.
	Timeout time.Duration `yaml:"timeout,omitempty"`
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
// pre-tiered single-knob fields `default_max_tokens`,
// `cost_ceiling_usd`, and `rate_limit_tps` are no longer accepted on
// `GovernanceConfig`. They were validated-but-ignored stubs: the
// loader took them, the enforcement engine never consumed them, and an
// operator setting `cost_ceiling_usd: 100` in YAML saw silent no-op
// behaviour. The loader now emits a structured
// `config.deprecated_field` slog warning when any of those keys
// appears in YAML, drops the value, and proceeds. Operators migrating
// from a pre-tiered config build a `default` tier with the
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
// cancellation, backpressure). Its first populated body is the run
// lifecycle-hook block.
type RuntimeConfig struct {
	// Hooks configures the runtime's run-lifecycle hooks. The only hook
	// point today is the run-completion hook (`runtime.hooks.run_completion`).
	Hooks RuntimeHooksConfig `yaml:"hooks,omitempty"`
	// Naming is the fleet-default session auto-naming policy
	// (`runtime.naming`). Opt-in, default off: with the block absent (or
	// `auto: false`) the runtime does no auto-naming. A per-agent
	// agent-config `naming` section overrides this default at run start.
	Naming RuntimeNamingConfig `yaml:"naming,omitempty"`
}

// RuntimeHooksConfig is the runtime's run-lifecycle hook block.
type RuntimeHooksConfig struct {
	// RunCompletion configures the run-completion hook: at the run loop's
	// terminal boundary the runtime dispatches the run's transcript to the
	// named catalog tool. Empty `Tool` disables the static hook (the
	// versioned agent-config `hooks` section can still enable it per agent).
	RunCompletion RunCompletionHookConfig `yaml:"run_completion,omitempty"`
}

// RunCompletionHookConfig is the static run-completion hook configuration.
// The durable, versioned equivalent is the agent-config `hooks` section
// (agentcfg.HooksSection); resolution at run start is agent-config over this
// yaml over no hook.
type RunCompletionHookConfig struct {
	// Tool is the catalog tool name the transcript is dispatched to. Empty
	// disables the static hook. The tool need not be exposed to the planner —
	// the executor resolves it against the full catalog.
	Tool string `yaml:"tool,omitempty"`
	// Timeout bounds the detached hook dispatch. Non-positive falls back to
	// the runtime default (10s) at run start.
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// RuntimeNamingConfig is the static fleet-default session auto-naming policy
// (`runtime.naming`). The durable, versioned per-agent equivalent is the
// agent-config `naming` section (agentcfg.NamingSection); resolution at run
// start is agent-config over this yaml over off. Opt-in, default off: with
// `Auto` false the runtime writes no counters, makes no naming LLM calls, and
// emits no naming events.
type RuntimeNamingConfig struct {
	// Auto enables session auto-naming fleet-wide (false = off).
	Auto bool `yaml:"auto,omitempty"`
	// AfterTurns is the completed-run count at which the first auto-name
	// fires. Zero defaults to 1 at run start; negative is rejected.
	AfterTurns int `yaml:"after_turns,omitempty"`
	// RepeatEvery, when > 0, re-names every N completed turns after the
	// first; 0 names once only. Negative is rejected.
	RepeatEvery int `yaml:"repeat_every,omitempty"`
	// MaxRepetitions caps the TOTAL auto-namings (including the first);
	// required >= 1 when RepeatEvery > 0 (no unlimited value exists).
	MaxRepetitions int `yaml:"max_repetitions,omitempty"`
	// MaxTitleLen bounds the auto title in runes. Zero defaults to 80 at run
	// start; a set value must be in [8, 200].
	MaxTitleLen int `yaml:"max_title_len,omitempty"`
	// Model, when set, is the model the auto-naming call requests (empty =
	// the run's effective model). Validated against ModelProfiles at boot.
	Model string `yaml:"model,omitempty"`
	// ReasoningMode controls reasoning only for the naming call: empty / off
	// explicitly disables reasoning; provider_default omits provider controls
	// without inheriting the model profile's planner default.
	ReasoningMode string `yaml:"reasoning_mode,omitempty"`
}

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
	Driver             string          `yaml:"driver"`
	DSN                string          `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode      sqlmigrate.Mode `yaml:"migration_mode,omitempty"`
	Strategy           string          `yaml:"strategy,omitempty"`
	BudgetTokens       int             `yaml:"budget_tokens,omitempty"`
	RecoveryBacklogMax int             `yaml:"recovery_backlog_max,omitempty"`
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
// `Directory` shapes the skills virtual directory the run loop
// injects as the per-turn `<skills_context>` prompt block (
// ). All fields optional; restart-required.
type SkillsConfig struct {
	Driver        string                `yaml:"driver,omitempty"`
	DSN           string                `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode sqlmigrate.Mode       `yaml:"migration_mode,omitempty"`
	Directory     SkillsDirectoryConfig `yaml:"directory,omitempty"`

	// Retrieval is the opt-in retrieval mode for `Search` /
	// `skill_search`. Empty (the default) keeps the token-savvy
	// FTS5 → regex → exact ladder; `"semantic"` ranks by embedding
	// similarity over the identity-scoped catalog instead (result
	// path `"semantic"`). Requires the `embeddings` block (validated;
	// no stub fallback). Capability filtering, redaction, and the
	// budgeter apply unchanged on top.
	Retrieval string `yaml:"retrieval,omitempty"`

	// SessionPersonalCutover is the static, restart-required operator
	// declaration that controls durable session-personal-skill migration.
	// An omitted declaration leaves a tenant in read-only dual-read mode.
	SessionPersonalCutover SessionPersonalCutoverConfig `yaml:"session_personal_cutover,omitempty"`

	// BootAgentPacks is the boot YAML declaration of the operator-managed
	// per-agent skill pack FILE sources the runtime's boot resolver
	// composes for the boot/default agent (see BootAgentPackConfig for the
	// entry shape + bounds). Absent (the zero value) means no boot pack
	// source — omission is byte-compatible with a runtime that predates
	// the feature. Optional; when non-empty it REQUIRES the skills driver
	// + DSN contract (validated — the composite resolver needs the
	// configured skill store to compose against). Restart-required.
	BootAgentPacks []BootAgentPackConfig `yaml:"boot_agent_packs,omitempty"`
}

// BootAgentPackConfig declares ONE boot-time, operator-managed per-agent
// skill pack source: a filesystem directory of package-directory skills
// the runtime's boot resolver composes into the boot agent's run view.
// It is the config-side, file-source declaration of the per-agent skill
// pack surface; the durable revision-versioned packs remain the
// protocol-managed mechanism — this block is the boot-time file source
// the resolver consumes, addressed by (tenant_id, agent_id) at
// configuration-selection time. agent_id never becomes an isolation
// principal: the run still starts from the caller's verified identity
// triple and its signed reach to the effective agent.
//
// Layout in YAML:
//
//	skills:
//	  boot_agent_packs:
//	    - tenant_id: acme
//	      agent_id: harbor-dev-agent
//	      directory: /etc/harbor/skills
//	      include: [workbench-foundation]
//
// Fields:
//   - `tenant_id` / `agent_id` — the (tenant, agent) pair the pack is
//     declared for. Required; each pair must be unique across the list.
//     `agent_id` MUST equal the runtime-resolved boot/default agent id —
//     enforced by [Config.ValidateBootAgentPacksForAgent], which the
//     runtime calls with the authoritative resolved value. The config
//     package does not know that value and never hard-codes it.
//   - `directory` — the skills directory on disk. May be ABSOLUTE (the
//     authoritative `/etc/harbor/skills` deployment shape) or RELATIVE; a
//     relative directory resolves against the loaded config file's
//     directory at Load time (the `tools.http_manifests` provenance
//     pattern), NEVER the process CWD. A config loaded without a file
//     source (LoadFromBytes / a hand-built *Config) keeps the relative
//     value unresolved so the later boot loader can fail loud rather
//     than silently falling back to CWD. Surrounding whitespace is
//     rejected outright, and the rune bound applies to the stored/raw
//     value — never a trimmed copy.
//   - `include` — the exact list of package-directory names under
//     `directory` to compose. Each entry is EXACTLY ONE relative
//     package-directory name: non-empty, single-segment, no `.` / `..`,
//     no `/` or `\` separator, no absolute / drive / URI form. Required
//     (≥ 1); unique within the entry both raw and after case-normalised
//     comparison (the resolver matches package-directory names
//     case-insensitively, mirroring skills.CanonicalPackName).
//
// Bounds (deterministic, exported for the loader + the boot resolver):
// at most [MaxBootAgentPacks] declarations; at most
// [MaxBootAgentPackIncludes] includes per declaration; at most
// [MaxBootAgentPackAggregateIncludes] includes in aggregate; each
// identity-ish per-field string bounded by [MaxBootAgentPackFieldRunes]
// and the directory by [MaxBootAgentPackDirectoryRunes]. Restart-required.
type BootAgentPackConfig struct {
	TenantID  string   `yaml:"tenant_id"`
	AgentID   string   `yaml:"agent_id"`
	Directory string   `yaml:"directory"`
	Include   []string `yaml:"include"`
}

// Boot agent pack bounds. Exported so the loader (this package) and the
// future boot resolver (the skills subsystem) share ONE deterministic,
// closed contract — a declaration cannot be accepted at config load and
// refused at boot because the two sides disagree on a ceiling.
const (
	// MaxBootAgentPacks bounds the number of declarations in
	// `skills.boot_agent_packs`.
	MaxBootAgentPacks = 64
	// MaxBootAgentPackIncludes bounds the `include` entries on ONE
	// declaration.
	MaxBootAgentPackIncludes = 64
	// MaxBootAgentPackFieldRunes bounds each identity-ish per-declaration
	// string field (tenant_id, agent_id, one include name) in runes.
	MaxBootAgentPackFieldRunes = 256
	// MaxBootAgentPackDirectoryRunes bounds the `directory` field in
	// runes. Larger than the identity fields because a real filesystem
	// path legitimately runs longer than a token.
	MaxBootAgentPackDirectoryRunes = 4096
	// MaxBootAgentPackAggregateIncludes bounds the TOTAL include count
	// across every declaration — the boot resolver enumerates all of
	// them into one composed view, so the aggregate is what bounds the
	// composed snapshot.
	MaxBootAgentPackAggregateIncludes = 256
)

// SessionPersonalCutoverConfig contains the finite tenant declarations the
// runtime may migrate. It is intentionally static configuration, not a
// runtime writer-discovery or membership mechanism.
type SessionPersonalCutoverConfig struct {
	Tenants []SessionPersonalCutoverTenant `yaml:"tenants,omitempty"`
}

// SessionPersonalCutoverTenant attests that a tenant's legacy session-skill
// writers have drained for one migration epoch.
type SessionPersonalCutoverTenant struct {
	TenantID             string `yaml:"tenant_id"`
	Epoch                string `yaml:"epoch"`
	RosterDigest         string `yaml:"roster_digest"`
	LegacyWritersDrained bool   `yaml:"legacy_writers_drained"`
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

	// Turns configures the durable conversation-turn projection store
	// (HA-64) — the indexed read model backing
	// `sessions.turns.list` / `sessions.turns.get`. Optional; an
	// absent block (Driver empty) leaves the projection unwired and
	// the turn routes at 501 (the partial-build convention). Drivers:
	// `inmem` (dev/embedded, per-process), `sqlite`
	// (`modernc.org/sqlite`, CGo-free, durable), `postgres`
	// (pgx-backed, durable, multi-replica). Restart-required.
	Turns TurnsConfig `yaml:"turns,omitempty"`
}

// TurnsConfig configures the HA-64 durable conversation-turn projection
// store. The shape mirrors the other driver-selecting blocks
// (state/memory/artifacts): a `driver` name plus the connection
// `dsn`. `Retention` bounds the newest turn rows retained per session
// (<= 0 applies the projection's documented default).
type TurnsConfig struct {
	Driver        string          `yaml:"driver,omitempty"`
	DSN           string          `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode sqlmigrate.Mode `yaml:"migration_mode,omitempty"`
	Retention     int             `yaml:"retention,omitempty"`
}

// ObservabilityConfig owns the observability-rollup projection block
// (HA-65). Optional; the zero value leaves the rollup surface unwired.
type ObservabilityConfig struct {
	// Rollups configures the durable rollup store. Drivers:
	// `inmem` (dev/embedded reference), `sqlite`, `postgres`.
	// Optional; an empty Driver leaves the projection unwired.
	Rollups RollupsConfig `yaml:"rollups,omitempty"`
}

// RollupsConfig configures the HA-65 observability rollup store — the
// same driver + dsn shape as the other driver-selecting blocks.
type RollupsConfig struct {
	Driver        string          `yaml:"driver,omitempty"`
	DSN           string          `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode sqlmigrate.Mode `yaml:"migration_mode,omitempty"`
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
// mandatorily routes a payload bound for a model's CONTEXT WINDOW
// through the ArtifactStore. Default 128 KiB (RFC §6.5, §6.10). The
// routing is a runtime-wide invariant, not a per-tool opt-in; there
// are no per-tool overrides. Consumed by the tool dispatcher
// (promote-to-stub), the LLM-edge catch-all (leak detection +
// auto-materialization), the trajectory-compaction payload budget, and
// the `tools.content_stats` report that echoes the resolved value.
//
// It does NOT govern Console-facing Protocol replies. `pause.list`,
// `memory.get` / `memory.list`, the flow catalog and the
// `mcp.servers.read_resource` / `mcp.apps.call_tool` /
// `mcp.apps.tool_context` reads select inline-versus-reference at the
// pinned `DefaultConsoleInlinePayloadBytes` (32 KiB) instead, because
// those payloads never enter a model's context and the selection is
// Protocol-visible. Raising this field does not widen them.
//
// `FetchDefaultMaxBytes` and `FetchHardMaxBytes` are the operator's
// half of the read-back bound. `HeavyOutputThresholdBytes` governs what
// GOES OUT to the store; these two govern what COMES BACK from it, and
// the pair is tunable together rather than one being operator policy and
// the other a compile-time constant. `FetchDefaultMaxBytes` (default
// 64 KiB) is the window applied when a caller names no bound;
// `FetchHardMaxBytes` (default 1 MiB) is the ceiling a caller's own
// bound is clamped to. Both are optional: zero selects the built-in
// default, so an existing configuration is unchanged by their arrival.
// Validation rejects a negative value and rejects a resolved default
// above the resolved hard ceiling.
//
// A request above the effective ceiling is SERVED AT THE CEILING and
// says so through the response's `truncated` / `total_size_bytes` /
// `returned_bytes` fields — never refused and never silently shortened.
//
// The guarantee is bounded and stated as such: the ceiling bounds ONE
// read. It is not a budget over repeated reads; aggregate consumption
// stays the governance layer's concern (cost ceilings and rate limits).
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
	Driver                    string          `yaml:"driver"`
	FSRoot                    string          `yaml:"fs_root,omitempty"`
	DSN                       string          `yaml:"dsn,omitempty" secret:"true"`
	MigrationMode             sqlmigrate.Mode `yaml:"migration_mode,omitempty"`
	HeavyOutputThresholdBytes int             `yaml:"heavy_output_threshold_bytes,omitempty"`
	FetchDefaultMaxBytes      int             `yaml:"fetch_default_max_bytes,omitempty"`
	FetchHardMaxBytes         int             `yaml:"fetch_hard_max_bytes,omitempty"`
	S3Bucket                  string          `yaml:"s3_bucket,omitempty"`
	S3Endpoint                string          `yaml:"s3_endpoint,omitempty"`
	S3Region                  string          `yaml:"s3_region,omitempty"`
	S3Prefix                  string          `yaml:"s3_prefix,omitempty"`
	S3AccessKeyID             string          `yaml:"s3_access_key_id,omitempty" secret:"true"`
	S3SecretAccessKey         string          `yaml:"s3_secret_access_key,omitempty" secret:"true"`
	S3UsePathStyle            bool            `yaml:"s3_use_path_style,omitempty"`
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
// `HTTPManifests` lists paths to UTCP-style YAML manifests for the
// HTTP driver. Each entry is loaded (`http.LoadManifest`) and its
// tools registered on the runtime catalog (`http.RegisterManifest`)
// during assembly — AFTER built-in tools and BEFORE `Entries` applies
// its middleware, so an entry naming a manifest tool resolves
// cleanly. `Validate` checks the list structurally only (non-empty
// entries, unique after `filepath.Clean`); existence and parsing are
// boot's job, not the validator's (the validator stays I/O-free).
//
// `Load` resolves a RELATIVE entry against the loaded config file's
// directory, rejecting one that escapes it (§7 rule 5) — lexically
// (Clean+prefix) always, and via a symlink-containment re-check when
// the joined path exists on disk (a symlink inside the directory
// crossing outside it is rejected; a not-yet-existing manifest gets
// the lexical check only). An ABSOLUTE entry is `filepath.Clean`ed
// and accepted as-is (the documented `/etc/harbor/tools/*.yaml`
// operator deployment shape — the same trust posture as
// `artifacts.fs_root`). A hand-built `*Config` (no `Load` call, e.g.
// a headless Go embedder) skips this resolution step entirely;
// embedders should pass absolute paths.
//
// A listed manifest that is missing, unparseable, or fails the
// driver's own validation (literal secrets, `.Auth` template leaks,
// unknown fields, missing env refs) fails assembly loudly, naming
// both the file and this field. A manifest tool whose name collides
// with an already-registered catalog tool fails assembly the same
// way. Restart-required: manifests reload only on process restart.
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

	// OAuthCredentialBrokers is the boot-declared list of NAMED credential
	// sinks (token endpoint + allowed downstream hosts + broker
	// credential env). It is the config home for the pinned sinks a
	// Protocol-installed, zero-URL provider descriptor references by name
	// — the credential-plane invariant keeps every sink-determining value
	// boot-declared, config/file-only. Additive; the inline
	// `oauth_providers[].remote` block stays valid. Restart-required.
	//
	// This list is boot-validated but has no runtime consumer yet: the
	// Protocol surface that resolves a provider descriptor's
	// `credential_broker` against it lands alongside it in the same wave.
	// Until then a declared broker is validated and inert (declaring one
	// changes no behaviour); this is the sink home landing ahead of its
	// consumer within the wave, not a dangling field.
	OAuthCredentialBrokers []ToolOAuthCredentialBrokerConfig `yaml:"oauth_credential_brokers,omitempty"`

	// AllowWireOAuthDescriptor is a DEV-ONLY, fail-closed opt-in that permits
	// `agent_config.set_oauth_provider` / `add_mcp_connection` to carry a FULL
	// OAuth-provider binding over the wire (`token_url` / `audience` / `scopes` /
	// `remote{}`) instead of only a boot-declared provider NAME — so a
	// coordinator can stand up a new OAuth-fronted MCP server at runtime without a
	// static `oauth_providers[]` block and a redeploy. It RELAXES the
	// credential-plane invariant that no admin-writable field determines a
	// credential sink, so it is default false / fail-closed: with it unset (all of
	// production), a wire descriptor carrying any credential-sink field is REJECTED
	// exactly as the zero-URL name-only binding rejects it today. When set, the
	// relaxation stays bounded — `allowed_downstream_hosts` is DERIVED from the
	// connected server's own URL (never a wire field) and the wire `token_url` /
	// `remote{}` dials face the identical token-exchange SSRF backstop. The same
	// opt-in is also available globally via the `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR`
	// boot env; the effective posture is this flag OR that env. Restart-required.
	// Do NOT enable in production.
	AllowWireOAuthDescriptor bool `yaml:"allow_wire_oauth_descriptor,omitempty"`

	// AllowWireInjection is a DEV-ONLY, fail-closed opt-in that permits
	// `agent_config.add_mcp_connection` to carry a per-user credential-INJECTION
	// mapping for a RECEIVER-STYLE MCP server over the wire (the `injection`
	// object: which boot-declared broker + which target header / basic / `_meta`
	// key), so a coordinator can ATTACH such a server at runtime and wire per-user
	// credential delivery to it without a static `tools.mcp_servers[].injection`
	// block and a redeploy. It is INDEPENDENT of AllowWireOAuthDescriptor — an
	// operator may enable wire-OAuth without wire-injection or vice versa. It
	// carries a NON-SECRET mapping only (the credential is still pulled per-user
	// from the named broker at call time; nothing secret rides the wire), but it
	// RELAXES the posture that no admin-writable field wires a credential to a
	// receiver, so it is default false / fail-closed: with it unset (all of
	// production), a connection descriptor carrying any injection field is
	// REJECTED. When set, the relaxation stays bounded — the pulled credential's
	// reachable host is DERIVED from the connection's own URL and validated against
	// the named broker's boot-declared allow-list (never a wire field), and every
	// declared target key must be redaction-covered. The same opt-in is also
	// available globally via the `HARBOR_ALLOW_WIRE_INJECTION` boot env; the
	// effective posture is this flag OR that env. Restart-required. Do NOT enable
	// in production.
	AllowWireInjection bool `yaml:"allow_wire_injection,omitempty"`

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

	// MCPArtifactEgressMaxBytes bounds ONE substituted artifact value on
	// one outbound MCP tool call — the ceiling for egress substitution,
	// where the runtime resolves an artifact id the model authored and
	// places the resolved BYTES into the outbound tool-call body.
	//
	// A value above the ceiling is REFUSED loud, never truncated: a
	// partial document delivered to a remote ingester is a corruption,
	// not a bounded read, so the artifact read path's truthful-truncation
	// posture deliberately does not apply here.
	//
	// It is deliberately independent of the heavy-output threshold — see
	// [DefaultMCPArtifactEgressMaxBytes], which also carries the sizing
	// arithmetic (`ceiling x in-flight mapped calls`, NOT multiplied by
	// the retry budget) and the base64 inflation figure.
	//
	// Zero / unset resolves to [DefaultMCPArtifactEgressMaxBytes] (8
	// MiB) through [ToolsConfig.ResolvedMCPArtifactEgressMaxBytes].
	// Optional. Restart-required.
	MCPArtifactEgressMaxBytes int `yaml:"mcp_artifact_egress_max_bytes,omitempty"`

	// MCPAppRenderAdmission is the narrow operator-facing switch for the
	// HA-56 fresh render-admission surface (the opt-in
	// `request_render_admission` `ui://` read authority that lets a
	// sandboxed MCP App re-invoke an app-only callback through the
	// admission-aware AppsAccessor seam). Default FALSE — the compatible
	// disabled surface: ordinary reads and the legacy live-binding path
	// work byte-for-byte, and the opt-in mint / admission-backed call
	// fail loud at the seam. When ENABLED the runtime resolves its
	// restart-stable AES-256-GCM sealing authority from the SAME
	// deployment-shared `tools.oauth_token_kek_env` the OAuth token store
	// uses — there is deliberately no second secret field and no
	// process-local seeded key. An empty env name, an unset / invalid
	// KEK, or a failure to construct the shared sealer fails readiness
	// LOUD even when no OAuth provider or credential broker is declared.
	// Restart-required.
	MCPAppRenderAdmission MCPAppRenderAdmissionConfig `yaml:"mcp_app_render_admission,omitempty"`
}

// MCPAppRenderAdmissionConfig is the operator-facing block that enables
// the fresh render-admission surface. The zero value (the block absent)
// is the compatible disabled surface.
type MCPAppRenderAdmissionConfig struct {
	// Enabled turns the HA-56 render-admission surface on. Default
	// false (backward-compatible deployments). Restart-required.
	Enabled bool `yaml:"enabled,omitempty"`
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

// MCPArtifactParams maps an MCP tool NAME to the parameter names on
// that tool which carry artifact BYTES.
//
// It is keyed per tool name in the same shape `ToolPolicies` and
// `ToolOAuthProviders` already establish — the SERVER-side MCP tool
// name, not the `<source>_<tool>` Harbor-facing name.
//
// It is OPERATOR-declared, and deliberately so: a remote server never
// decides when the runtime reads its own store. A server-declared "this
// parameter is an artifact reference" annotation was considered and
// rejected, because a remote server driving host-side privileged
// behaviour inverts the host obligation.
//
// A mapped parameter is validated against the server's own DISCOVERED
// input schema at attach: it must be declared there, and it must be
// declared string-typed. Advertising "this parameter takes artifact
// bytes" against a server whose schema never declared such a parameter
// is the same defect shape as advertising an unserviced capability, so
// the mapping is checked rather than trusted.
type MCPArtifactParams map[string][]string

// Signed and operator-authored artifact mappings share these exact admission
// ceilings. They bound both config validation and Protocol normalization so a
// declaration cannot be accepted at one door and refused at another.
const (
	MaxMCPArtifactMethods         = 32
	MaxMCPArtifactParamsPerMethod = 8
	MaxMCPArtifactNameBytes       = 128
	MaxMCPArtifactParamsJSONBytes = 8 * 1024
)

// DefaultMCPArtifactEgressMaxBytes bounds ONE substituted artifact value
// on one outbound MCP tool call.
//
// Deliberately NOT derived from the heavy-output threshold. Substituted
// bytes never enter a model's context — that is the whole point of the
// feature — so the budget is a NETWORK and MEMORY budget, not a token
// budget, and tying it to a context-window constant would make one
// number answer two unrelated questions. The independent-ceiling shape
// is the one the artifact fetch bounds already set.
//
// Sizing arithmetic an operator needs, stated rather than left to be
// discovered: the transient footprint is `ceiling x in-flight mapped
// calls`. It is NOT multiplied by the retry budget, because resolution
// happens once per dispatched call rather than once per attempt —
// resolving inside the reliability shell's loop would multiply it by
// four at the default policy.
//
// Base64 inflates the wire form by roughly a third, so 8 MiB of content
// is around 11 MB on the wire. That is acceptable over HTTP and is one
// reason the mapping is refused on stdio, where a frame that large is
// least appropriate.
const DefaultMCPArtifactEgressMaxBytes = 8 * 1024 * 1024

// ResolvedMCPArtifactEgressMaxBytes returns the configured
// `mcp_artifact_egress_max_bytes`, substituting
// [DefaultMCPArtifactEgressMaxBytes] when the field is unset, so the
// operator's value and the default resolve through ONE accessor rather
// than each call site re-deciding what zero means.
func (t ToolsConfig) ResolvedMCPArtifactEgressMaxBytes() int {
	if t.MCPArtifactEgressMaxBytes <= 0 {
		return DefaultMCPArtifactEgressMaxBytes
	}
	return t.MCPArtifactEgressMaxBytes
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
	Name   string `yaml:"name"`
	Driver string `yaml:"driver"`
	// CredentialSource selects the §4.4 seam through which this
	// provider's OWN client credential resolves:
	//   - "" / "env" — resolved from ClientIDEnv / ClientSecretEnv once
	//     at boot (the default; existing configs are byte-compatible).
	//   - "remote" — pulled from a coordinator endpoint (the Remote
	//     block) at first need. Valid only for the "tokenexchange"
	//     driver. Declaring "remote" alongside ClientIDEnv /
	//     ClientSecretEnv is a validation error (one source, no dual
	//     path).
	CredentialSource string `yaml:"credential_source,omitempty"`
	ClientIDEnv      string `yaml:"client_id_env,omitempty"`
	ClientSecretEnv  string `yaml:"client_secret_env,omitempty"`
	// Remote carries the coordinator-served credential-fetch parameters;
	// required when CredentialSource == "remote", rejected otherwise.
	Remote      *ToolOAuthRemoteConfig `yaml:"remote,omitempty"`
	Scopes      []string               `yaml:"scopes,omitempty"`
	AuthURL     string                 `yaml:"auth_url,omitempty"`
	TokenURL    string                 `yaml:"token_url,omitempty"`
	RedirectURL string                 `yaml:"redirect_url,omitempty"`
	Extra       map[string]string      `yaml:"extra,omitempty"`

	// AllowedDownstreamHosts is the boot-declared set of downstream
	// connection hosts (host[:port]) a southbound MCP binding may inject
	// this provider's credential into (the exchanged bearer for the
	// `tokenexchange` driver; the interactive bearer for `oauth2`). It is
	// the credential-sink allow-list: the credential-plane invariant is
	// that no admin-writable field determines a credential sink, so the
	// sink set is boot-declared and config/file-only. A binding whose MCP
	// connection host is not listed is refused at attach, fail-closed —
	// never a silent unauthenticated dial. Host comparison normalises
	// host[:port] (default-port equivalence; case-insensitive) via
	// [NormalizeDownstreamHost].
	//
	// MANDATORY (non-empty) for any provider a `tools.mcp_servers[]`
	// connection binds: a provider that can inject a bearer must declare
	// where. This is a behaviour change for an existing OAuth-bound
	// connection that never listed its downstream host (see the CHANGELOG
	// migration note) — the empty-list rejection surfaces at boot, not at
	// first call. Restart-required.
	AllowedDownstreamHosts []string `yaml:"allowed_downstream_hosts,omitempty"`

	// Audience is the boot-declared token-audience ceiling for the
	// `tokenexchange` driver. When non-empty it is the authority for the
	// exchanged token's audience — decoupled from the caller-chosen
	// provider name — closing the audience-picking lever. Empty preserves
	// the legacy audience-from-name behaviour (opt-in hardening;
	// backward-compatible). Ignored by the interactive `oauth2` driver.
	// Restart-required.
	Audience string `yaml:"audience,omitempty"`

	// ScopeCeiling is the boot-declared scope ceiling for the
	// `tokenexchange` driver. When non-empty the requested `Scopes` are
	// INTERSECTED against it — a requested scope outside the ceiling is
	// dropped, never honoured. Empty preserves the legacy pass-through.
	// Ignored by the interactive `oauth2` driver. Restart-required.
	ScopeCeiling []string `yaml:"scope_ceiling,omitempty"`

	// ResourceIndicator is the boot-declared RFC 8707 `resource` value the
	// `tokenexchange` driver carries as the `resource` form parameter on
	// every exchange request (the audience the exchanged token is bound to).
	// A JWT-shaped returned token whose `aud` claim excludes this value fails
	// the exchange loud (never cached). Empty preserves today's behaviour (no
	// `resource` parameter sent). NEVER auto-populated from discovery
	// (discovery is report-only; an operator copies a discovered+confirmed
	// value in by hand). Ignored by the interactive `oauth2` driver.
	// Restart-required.
	ResourceIndicator string `yaml:"resource_indicator,omitempty"`

	// IncludeActorToken opts a `tokenexchange` provider into carrying the
	// run's verified acting principal (`agent_id`, when present on ctx) as an
	// RFC 8693 `actor_token` on the exchange. Default false; an absent
	// `agent_id` sends no `actor_token` regardless (a byte-identical request
	// to today). The actor token is the runtime's VERIFIED acting principal —
	// never a client-supplied field. Ignored by the interactive `oauth2`
	// driver. Restart-required.
	//
	// Caching note: the exchanged token is cached at user granularity
	// (scope, tenant, user, source) — the acting principal is deliberately
	// NOT part of the cache key, because `agent_id` is not an isolation
	// principal. Within one cached token's TTL, a second acting principal
	// under the same (tenant, user) reuses the token minted under the
	// first — no isolation boundary is crossed (the token stays user-bound
	// and the `actor_asserted` audit signal fires only on the real
	// exchange). If a downstream broker scopes grants PER acting principal,
	// disable caching pressure by narrowing the token TTL rather than
	// expecting per-actor cache separation here.
	IncludeActorToken bool `yaml:"include_actor_token,omitempty"`

	// AllowPrivateTokenURL is a DEV-ONLY opt-in that permits the
	// `tokenexchange` driver's credential-bearing exchange POST to dial a
	// PRIVATE-range / link-local resolved address for THIS provider's own
	// `token_url` — the standard containerized local-dev topology, where the
	// broker (a coordinator) sits behind a private-IP TLS sidecar. It
	// RELAXES a load-bearing DNS-rebinding dial guard, so it is default
	// false / fail-closed: with it unset the guard refuses any resolved
	// private / link-local / ULA address. When set it relaxes ONLY that
	// branch, scoped to this provider's own boot-declared `token_url`; the
	// unspecified-address (0.0.0.0 / ::) block, the loopback carve-out
	// (loopback stays allowed either way), and the redirect refusal are
	// UNCHANGED. Boot-declared and config/file-only — never Protocol-writable
	// or derived from a discovered / wire descriptor. The same opt-in is
	// also available globally via the `HARBOR_DEV_ALLOW_PRIVATE_EXCHANGE`
	// boot env; the effective posture is this flag OR that env. Ignored by
	// the interactive `oauth2` driver. Restart-required. Do NOT enable in
	// production.
	AllowPrivateTokenURL bool `yaml:"allow_private_token_url,omitempty"`
}

// ToolOAuthCredentialBrokerConfig declares one NAMED, boot-declared
// credential broker — a pinned credential SINK the credential-plane
// invariant (no admin-writable field determines a credential sink) keeps
// off the wire. It is the boot home for the token endpoint + allowed
// downstream hosts a Protocol-installed, zero-URL provider descriptor
// references BY NAME: the sink-determining values live here, never on a
// wire-writable descriptor.
//
// Config/file-only, restart-required — NOT a Protocol surface. The
// existing inline `oauth_providers[].remote` block stays valid; this list
// is additive.
//
// Layout in YAML:
//
//	tools:
//	  oauth_credential_brokers:
//	    - name: m365-broker
//	      token_url: https://broker.example.com/oauth2/token
//	      allowed_downstream_hosts: ["graph.microsoft.com"]
//	      auth_token_env: HARBOR_M365_BROKER_TOKEN
//	      cache_ttl: 5m       # optional
//	      timeout: 10s        # optional
type ToolOAuthCredentialBrokerConfig struct {
	// Name is the operator-facing broker identifier (unique within the
	// slice; referenced by name from a Protocol-installed provider
	// descriptor). Required.
	Name string `yaml:"name"`
	// TokenURL is the broker's RFC-8693 token-exchange endpoint — the
	// credential sink the org's client_id / client_secret are POSTed to.
	// Required; must be https (or a loopback host for the dev/fixture
	// case). Boot-pinned: never wire-writable.
	TokenURL string `yaml:"token_url"`
	// AllowedDownstreamHosts is the sink allow-list for the exchanged
	// bearer. Required non-empty (a bearer-minting broker must declare
	// where its tokens may be injected).
	AllowedDownstreamHosts []string `yaml:"allowed_downstream_hosts"`
	// AuthTokenEnv names the env var holding the runtime's own broker
	// credential (§7 rule 2 — never hardcoded). Required non-empty. It is the
	// `Authorization: Bearer` the runtime presents to CredentialURL when it
	// pulls the org client credential the exchange POSTs to TokenURL.
	AuthTokenEnv string `yaml:"auth_token_env"`
	// CredentialURL is the boot-pinned coordinator credential endpoint the
	// runtime pulls its OWN org client credential (client_id / client_secret)
	// from — the `remote` credential-source PULL URL for a
	// Protocol-installed broker-pull provider. It is a credential-sink-adjacent
	// value (it authenticates with AuthTokenEnv and yields the org secret the
	// exchange POSTs), so it is BOOT-PINNED, config/file-only, never
	// wire-writable (the credential-plane invariant). Required (https,
	// or a loopback host for the dev/fixture case) for any broker referenced by
	// a Protocol-installed provider — validated loud when the broker is resolved
	// at install time. Distinct from TokenURL: CredentialURL is the GET the
	// runtime pulls its client credential from; TokenURL is the POST the
	// exchange sends it to.
	CredentialURL string `yaml:"credential_url,omitempty"`
	// Audience is the boot-pinned token-audience ceiling for the exchanged
	// downstream token — the authority for the exchanged token's audience,
	// decoupled from the caller-chosen provider name (closing the
	// audience-picking lever). Empty preserves the legacy
	// audience-from-name behaviour. Boot-pinned, never wire-writable.
	Audience string `yaml:"audience,omitempty"`
	// ScopeCeiling is the boot-pinned scope ceiling for the exchanged token.
	// When non-empty the SCOPES an installed descriptor requests are
	// INTERSECTED against it — a requested scope outside the ceiling is dropped,
	// never honoured (so an installed descriptor can never widen scope past the
	// boot ceiling). Empty preserves the legacy pass-through. Boot-pinned,
	// never wire-writable.
	ScopeCeiling []string `yaml:"scope_ceiling,omitempty"`
	// CacheTTL caps the in-memory serve horizon for a brokered token.
	// Optional; zero = driver default.
	CacheTTL time.Duration `yaml:"cache_ttl,omitempty"`
	// Timeout bounds a single broker exchange. Optional; zero = driver
	// default.
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// SignedOAuthMCPCapabilityAuthority is the optional, boot-only trust anchor
	// for signed OAuth MCP capability registration. Its presence does not
	// by itself enable the Protocol write: Enabled must be true. The issuer,
	// verifier source, and maximum authority lifetime are all fixed at boot;
	// none is ever derived from a Protocol request.
	SignedOAuthMCPCapabilityAuthority *ToolSignedOAuthMCPCapabilityAuthorityConfig `yaml:"signed_oauth_mcp_capability_authority,omitempty"`
}

// ToolSignedOAuthMCPCapabilityAuthorityConfig is the verifier/opt-in portion
// of a boot-declared OAuth credential broker. It is intentionally nested
// beneath that broker: the request may name a broker, but it cannot select an
// arbitrary issuer, key source, or credential sink.
//
// Layout in YAML:
//
//	tools:
//	  oauth_credential_brokers:
//	    - name: m365-broker
//	      signed_oauth_mcp_capability_authority:
//	        enabled: true
//	        issuer: https://authority.example.com
//	        jwks_url: https://authority.example.com/.well-known/jwks.json
//	        max_authority_lifetime: 10m
//
// Config/file-only and restart-required. The explicit lifetime is required:
// Signed capability registration requires a bounded envelope, but deliberately does not impose a
// product-wide lifetime on every deployment.
type ToolSignedOAuthMCPCapabilityAuthorityConfig struct {
	// Enabled is the explicit production opt-in. False is fail-closed.
	Enabled bool `yaml:"enabled,omitempty"`
	// Issuer is the exact JWT issuer the broker accepts.
	Issuer string `yaml:"issuer"`
	// JWKSURL or JWKSFile selects the fixed public-key source. Exactly one is
	// required when Enabled.
	JWKSURL  string `yaml:"jwks_url,omitempty"`
	JWKSFile string `yaml:"jwks_file,omitempty"`
	// MaxAuthorityLifetime caps exp-issued-at for signed envelopes. Required
	// and positive when Enabled; an unset value is never silently defaulted.
	MaxAuthorityLifetime time.Duration `yaml:"max_authority_lifetime"`
}

// ToolOAuthRemoteConfig declares the coordinator-served credential
// source. At first credential need the runtime performs a single
// authenticated GET against URL (Authorization: Bearer, token read from
// AuthTokenEnv), strict-parses the JSON response
// (`format_version`, `client_id`, `client_secret`, optional
// `expires_in`), and holds the credential in memory only — TTL-capped,
// single-flighted, refetched on expiry. Nothing is persisted.
//
// Layout in YAML:
//
//	oauth_providers:
//	  - name: m365-broker
//	    driver: tokenexchange
//	    credential_source: remote
//	    token_url: https://broker.example.com/oauth2/token
//	    remote:
//	      url: https://coordinator.example.com/runtimes/self/broker-credential
//	      auth_token_env: HARBOR_COORDINATOR_TOKEN
//	      cache_ttl: 5m       # optional; caps the in-memory serve horizon
//	      timeout: 10s        # optional; bounds a single fetch
//
// Restart-required (the provider LIST is boot-declared; only credential
// resolution is late).
type ToolOAuthRemoteConfig struct {
	// URL is the coordinator credential endpoint. Required; validated
	// well-formed at boot. MUST be https — the fetch sends the runtime's
	// service bearer token, so TLS is mandatory; plaintext http is
	// accepted only for a loopback host (127.0.0.1 / ::1 / localhost —
	// the local fixture / dev case). Redirects from the endpoint are
	// refused, never followed.
	URL string `yaml:"url"`
	// AuthTokenEnv names the env var holding the runtime's own service
	// token, sent as a Bearer credential. Required; validated non-empty
	// at boot (read lazily at fetch time so a rotated token is picked up
	// without restart).
	AuthTokenEnv string `yaml:"auth_token_env"`
	// CacheTTL caps the in-memory serve horizon. Optional; zero = the
	// driver default (5m). The effective horizon is min(response
	// expires_in, CacheTTL).
	CacheTTL time.Duration `yaml:"cache_ttl,omitempty"`
	// Timeout bounds a single fetch. Optional; zero = the driver default
	// (30s).
	Timeout time.Duration `yaml:"timeout,omitempty"`
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
	// Validation rejects an unknown name, a binding on any connection
	// without an http(s) `url` — explicit stdio AND an omitted/auto
	// transport with only a `command`, which would auto-select stdio and
	// silently never inject — and a static `Authorization` header
	// alongside a binding (one auth mode per connection). Restart-required.
	OAuthProvider string `yaml:"oauth_provider,omitempty"`
	// MetaAnnotations is a static, NON-SECRET set of operator-declared
	// key/values merged into the MCP `_meta` map on every identity-stamped
	// per-call RPC — the deployment's own attribution vocabulary.
	//
	// Each KEY is a `_meta` PATH: a key with no `.` sets a top-level key, and
	// a DOTTED key NESTS (`vendor.account_id` lands at
	// `_meta.vendor.account_id`, not as a literal flat key) — the same
	// interpretation, through the same helper, that `injection.meta_key` has
	// always had, so one `_meta` namespace has one meaning regardless of which
	// mechanism wrote into it.
	//
	// Validation rejects: an empty key or an empty path segment; a key whose
	// WHOLE value or ANY dot-segment is reserved (`tenant`, `user`, `session`,
	// `agent_id`, `traceparent`, `tracestate`, and any
	// `io.modelcontextprotocol/`-prefixed key); a path deeper than
	// MaxMCPMetaKeyDepth; and a path that collides with another declared path
	// on the same connection — equal to it, or a prefix of it — including the
	// `injection.meta_key` path. Optional. Restart-required.
	MetaAnnotations map[string]string `yaml:"meta_annotations,omitempty"`
	// OAuthDiscoveryAllowedOrigins is the explicit per-connection allowance
	// list for OAuth-requirement discovery cross-origin metadata fetches
	// When an MCP HTTP server advertises an OAuth requirement (a
	// `401` + `WWW-Authenticate` step-up), Harbor DISCOVERS the RFC 9728 →
	// RFC 8414 metadata chain and surfaces it as inert data; the
	// authorization-server hop is inherently cross-origin and is fetched only
	// when its origin (scheme://host[:port]) is listed here. Harbor never runs
	// the OAuth flow, never holds a token, and never dials a discovered
	// endpoint — the metadata is a report an operator confirms. Each entry
	// must be a public https origin; the runtime additionally refuses
	// private-range / IP-literal destinations at fetch time. Optional; empty
	// means no cross-origin metadata fetch is permitted (the AS half of the
	// chain surfaces as needs-allowance). Restart-required.
	OAuthDiscoveryAllowedOrigins []string `yaml:"oauth_discovery_allowed_origins,omitempty"`

	// ToolOAuthProviders are per-tool `oauth_provider` overrides keyed by the
	// MCP tool's server-side name (mirroring ToolPolicies' shape). A tool
	// named here binds THAT declared provider for its CallTool RPCs only; an
	// unlisted tool falls back to OAuthProvider (the connection-level
	// binding), and the resource / prompt paths always resolve OAuthProvider.
	// Each entry re-enforces every rule OAuthProvider does (unknown name /
	// stdio transport / static-Authorization conflict / downstream-host
	// allow-list). It closes the one-audience-per-server constraint for a
	// shared MCP server fronting N downstream resources. Boot-declared,
	// config/file-only — never a Protocol-writable field (the credential-plane
	// invariant). Optional; empty preserves today's behaviour.
	// Restart-required.
	ToolOAuthProviders map[string]string `yaml:"tool_oauth_providers,omitempty"`

	// Injection, when set, binds this connection to per-user credential
	// INJECTION for a RECEIVER-STYLE MCP server — one that authenticates by
	// RECEIVING its credential directly on each request (an arbitrary header,
	// an `Authorization: Basic` value, or a `_meta` key) instead of PULLING it
	// via RFC 8693. On every identity-stamped outbound tool call the runtime
	// SOURCES the acting principal's credential from the named broker (the SAME
	// per-user broker-pull `oauth_provider` performs — per-user via the ctx
	// identity, fetched-not-held, memory-only TTL) and INJECTS it into the
	// request per the declared, NON-SECRET mapping. Only the pulled value is
	// secret; the mapping (which broker + which target key/form) is config.
	// Mutually exclusive with OAuthProvider / ToolOAuthProviders / a static
	// `Authorization` header (one auth mode per connection) — the same
	// downstream-sink allow-list the bearer binding enforces applies here (the
	// pulled credential may only reach a boot-declared sink). Optional; empty
	// preserves today's behaviour. Restart-required.
	Injection *MCPCredentialInjectionConfig `yaml:"injection,omitempty"`

	// ArtifactByteEligible declares that this connection MAY receive
	// artifact BYTES through egress substitution. It is the containment
	// boundary for the feature: with it unset (every connection by
	// default), an `artifact_params` mapping is REFUSED at the door and
	// no outbound call ever carries resolved content.
	//
	// It is honest about what it governs. The reachable artifact SET is
	// unchanged by this flag — that stays the dispatching run's own
	// (tenant, user, session), enforced by the same run-scoped resolver
	// the in-process arm uses. What the flag governs is the RECIPIENT:
	// whether a remote server may be handed those bytes.
	//
	// It is WIRE-CONFIGURABLE and admin-writable, and that is a
	// deliberate departure from the fail-closed boot-gate shape its
	// siblings (`allow_wire_oauth_descriptor`, `allow_wire_injection`)
	// take. Those gates exist because their fields determine WHERE A
	// CREDENTIAL IS SENT, which is a boot-declared-only plane. This field
	// determines where a USER'S OWN CONTENT is sent — a different plane,
	// and one whose boundary Harbor has already accepted and named: a
	// shared runtime TRUSTS its co-tenant admins for runtime-added
	// connections, with one-runtime-per-tenant as the stated remedy for a
	// deployment needing hard isolation. A boot gate would additionally
	// break the use case the feature exists for, where a server attached
	// over the control plane must be usable without a redeploy.
	//
	// An artifact is stored as authored — unredacted — so a byte-eligible
	// connection CAN move a secret. That is stated plainly rather than
	// softened; the compensating control is the `mcp.artifact_egressed`
	// record, emitted fail-closed before every wire request.
	//
	// Refused on stdio (explicit or auto-selected from a command-only
	// config), on the same rule the sibling http-only fields use.
	// Optional. Restart-required.
	ArtifactByteEligible bool `yaml:"artifact_byte_eligible,omitempty"`

	// ArtifactParams maps this server's tool names to the parameters on
	// those tools which carry artifact BYTES. When a mapped parameter
	// arrives carrying an artifact id, the runtime resolves it under the
	// dispatching run's own identity and writes the resolved bytes into
	// the outbound tool-call body as standard base64 — the model authored
	// an id and never sees content.
	//
	// Requires ArtifactByteEligible: a mapping on a non-eligible
	// connection is refused loud rather than silently ignored. Each
	// mapped parameter must appear in the server's own discovered input
	// schema AND be declared string-typed; an absent or non-string
	// parameter fails the attach. Refused on stdio.
	//
	// Keyed per tool name in the shape [MCPArtifactParams] documents.
	// Optional; empty preserves today's behaviour exactly (an outbound
	// call is byte-identical to a build without the feature).
	// Restart-required.
	ArtifactParams MCPArtifactParams `yaml:"artifact_params,omitempty"`
}

// MCP credential-injection form discriminators (MCPCredentialInjectionConfig.Form).
const (
	// MCPInjectionFormHeader injects the pulled credential VALUE as the value of
	// a declared request header (e.g. `x-vendor-api-key: <pulled>`).
	MCPInjectionFormHeader = "header"
	// MCPInjectionFormBasic injects the pulled credential as the password half of
	// an `Authorization: Basic base64(username ":" <pulled>)` header.
	MCPInjectionFormBasic = "basic"
	// MCPInjectionFormMeta injects the pulled credential as the leaf value of a
	// declared `_meta` key path (dot-separated for nesting, e.g. `vendor.api_key`).
	MCPInjectionFormMeta = "meta"
)

// MCPCredentialInjectionConfig is the NON-SECRET per-connection mapping that
// binds a receiver-style MCP server to per-user credential injection. It names
// the broker the per-user credential is pulled from and declares WHERE the
// pulled value is placed on the outbound request — a header, an
// `Authorization: Basic` value, or a `_meta` key. No secret material lives here;
// only the broker-pulled value (resolved per-call from the ctx identity) is
// secret. Every declared target key must be redaction-covered (validation
// rejects a target the audit redactor would not hold to `***`), so the injected
// value can never leak uncredacted into an audit payload.
type MCPCredentialInjectionConfig struct {
	// Provider names the declared `tools.oauth_providers[]` broker entry the
	// per-user credential is pulled from (the SAME registry `oauth_provider`
	// resolves against). Required; an unknown name fails validation.
	Provider string `yaml:"provider"`
	// Form selects the injection form: "header" / "basic" / "meta". Required.
	Form string `yaml:"form"`
	// Header is the target request header NAME for Form=="header" (e.g.
	// `x-vendor-api-key`). Required for that form; must be a redaction-covered
	// credential key and must not be `Authorization` (use Form=="basic").
	Header string `yaml:"header,omitempty"`
	// BasicUsername is the username half for Form=="basic". The pulled
	// credential becomes the password half: `Authorization: Basic
	// base64(BasicUsername ":" <pulled>)`. Optional — an empty username
	// (base64(":"+cred)) is the common API-key-as-basic shape. Non-secret.
	BasicUsername string `yaml:"basic_username,omitempty"`
	// MetaKey is the target `_meta` key PATH for Form=="meta", dot-separated
	// for nesting (e.g. `vendor.api_key`). Required for that form; no path
	// segment may be a reserved `_meta` key (the isolation triple / agent_id /
	// trace-context / spec namespace) and the leaf segment must be a
	// redaction-covered credential key.
	MetaKey string `yaml:"meta_key,omitempty"`
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
// `MaxSteps` configures the planner step tranche and the runtime's
// continuation authority. Zero (the default) means "use the driver's
// internal default" — e.g. the react driver's `react.DefaultMaxSteps`
// (12); it never selects the legacy terminal-only run loop. The validator
// rejects negative values pre-boot.
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
	MaxBatchSpawns         int                     `yaml:"max_batch_spawns,omitempty"`
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

// BatchSpawnCap resolves the optional `max_batch_spawns` knob. A
// non-positive value (unset or zero) resolves to `DefaultMaxBatchSpawns`:
// a batch decision whose spawn count exceeds this BREADTH ceiling is
// rejected loudly in full — never truncated to the first N spawns.
// Distinguished from `SpawnDepthCap`, which bounds spawn-chain DEPTH,
// not the breadth of one response's spawns.
func (p PlannerConfig) BatchSpawnCap() int {
	if p.MaxBatchSpawns <= 0 {
		return DefaultMaxBatchSpawns
	}
	return p.MaxBatchSpawns
}

// DefaultMaxBatchSpawns is the ONE source of the batch-spawn breadth
// default: the ceiling on how many task spawns a single batch decision
// may carry. Conservative and operator-revisable via
// `planner.max_batch_spawns`; the batch-dispatch cap references this
// constant so no other literal copy of the value exists.
const DefaultMaxBatchSpawns = 5

// DefaultHeavyOutputThresholdBytes is the ONE source of the
// LLM-CONTEXT heavy-output default (128 KiB; RFC §6.5, §6.10): the byte
// size at or above which content the runtime is about to place into a
// model's context window is promoted to an artifact-backed stub
// instead.
//
// It answers exactly ONE question — how many bytes may enter a prompt.
// Every consumer that answers a DIFFERENT question carries its own
// named constant with its own godoc stating that question; the
// Console-facing counterpart is `DefaultConsoleInlinePayloadBytes`, the
// search preview bound is `search.HeavyPreviewThreshold`, and the
// terminal fold bound lives in `internal/tui/renderers`. The rule is
// NOT "no second copy of the number": it is that no second constant may
// answer THIS question, and a different question takes its own constant
// even when the answer currently coincides. This constant deliberately
// does not enumerate its consumers — an enumeration rots the moment
// another one lands and no gate notices.
//
// 128 KiB sits at the top of the range the heavy-output research named
// reasonable (16 KiB – 128 KiB). The cost is ~32k tokens of a 200k
// window for a result in the 32–128 KiB band; the saving is the extra
// planner turn a stub-then-fetch cycle costs for exactly that band.
// `Defaults()` seeds `ArtifactsConfig.HeavyOutputThresholdBytes` from
// it and an operator's explicit value overrides it.
const DefaultHeavyOutputThresholdBytes = 128 * 1024

// DefaultConsoleInlinePayloadBytes is the ONE source of the
// CONSOLE-FACING inline-payload bound (32 KiB): the byte size at or
// above which a Protocol reply projected for a browser — `pause.list`,
// `memory.get` / `memory.list`, the flow catalog, and the
// `mcp.servers.read_resource` / `mcp.apps.call_tool` /
// `mcp.apps.tool_context` reads — ships an artifact reference instead
// of inline bytes.
//
// It is deliberately NOT `DefaultHeavyOutputThresholdBytes`. That value
// prices tokens against a context window; this one prices bytes into an
// HTTP reply a browser renders, where a reference costs the reader one
// more round trip it was already going to make and an inline payload
// costs every reader on the page. The selection is Protocol-visible —
// it decides which arm of a sum-typed reply is populated — so it is
// pinned rather than tracking the LLM-context threshold it used to
// alias. The two happen to have had the same answer; they were never
// the same question.
//
// There is deliberately no operator knob for it: an operator who raises
// `artifacts.heavy_output_threshold_bytes` is answering the prompt-size
// question, and that answer must not silently reshape wire replies for
// every Protocol client.
const DefaultConsoleInlinePayloadBytes = 32 * 1024

// DefaultArtifactFetchMaxBytes is the ONE source of the artifact
// read-back default (64 KiB): the window served when a caller names no
// bound of its own. Large enough for the structured-JSON payload a
// heavy tool result typically is, small enough that an accidental read
// of a multi-megabyte artifact does not fill a model's context window.
// `ArtifactsConfig.FetchDefaultMaxBytes` resolves to it when unset, and
// both the Protocol byte read and the `artifact_fetch` builtin reference
// this constant rather than carrying a literal copy.
const DefaultArtifactFetchMaxBytes = 64 * 1024

// DefaultArtifactFetchHardMaxBytes is the ONE source of the artifact
// read-back CEILING default (1 MiB): the bound a caller's own request is
// clamped to. `ArtifactsConfig.FetchHardMaxBytes` resolves to it when
// unset.
//
// The clamp is served, not refused, and the response says it was applied
// — a caller cannot know a deployment's ceiling before asking. The
// guarantee is bounded to ONE read; aggregate consumption stays the
// governance layer's concern.
const DefaultArtifactFetchHardMaxBytes = 1 * 1024 * 1024

// ResolvedFetchDefaultMaxBytes returns the configured
// `FetchDefaultMaxBytes`, substituting `DefaultArtifactFetchMaxBytes`
// when the field is zero or negative. Zero means "the operator named no
// bound", which is what the `omitempty` tag declares, so a configuration
// written before the field existed resolves to the documented default.
func (c ArtifactsConfig) ResolvedFetchDefaultMaxBytes() int {
	if c.FetchDefaultMaxBytes <= 0 {
		return DefaultArtifactFetchMaxBytes
	}
	return c.FetchDefaultMaxBytes
}

// ResolvedFetchHardMaxBytes returns the configured `FetchHardMaxBytes`,
// substituting `DefaultArtifactFetchHardMaxBytes` when the field is zero
// or negative.
func (c ArtifactsConfig) ResolvedFetchHardMaxBytes() int {
	if c.FetchHardMaxBytes <= 0 {
		return DefaultArtifactFetchHardMaxBytes
	}
	return c.FetchHardMaxBytes
}

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

// VirtualAgentsConfig is the boot YAML declaration of the configured
// top-level agent's optional virtual-agent profiles (`virtual_agents:`).
//
// Virtual agents are NOT registered agents: they never appear in
// `agents.list`, are never `control.start` targets, and never join the
// isolation tuple. A profile is a per-run CONFIGURATION projection —
// the effective child run is the parent's frozen config plus the
// profile's bounded, non-recursive narrow-only overlay. The overlay may
// narrow model parameters / skills / tools / limits and add trusted
// specialist instructions; it can never widen resources or guardrails,
// attach capabilities, own providers / hooks / memory, recurse into
// another profile, or target A2A (those dimensions have no fields).
//
// The owner MUST equal the runtime's configured top-level agent
// (validated at boot against the same agent id the run-loop driver
// resolves as its default). Profiles declared here share the ONE
// canonical representation with the durable agent-config `virtual_agents`
// section (internal/virtualagent); at run start the revision section
// replaces this YAML map (agent-config over yaml precedence).
type VirtualAgentsConfig struct {
	// Owner is the top-level agent that owns these profiles. Required
	// when the block is present; must equal the runtime's configured
	// default agent id.
	Owner string `yaml:"owner,omitempty"`
	// MaxProfiles is the operator cap for this map. Zero uses the canonical
	// virtualagent default.
	MaxProfiles int `yaml:"max_profiles,omitempty"`
	// Profiles is the ordered profile declaration list. A duplicate key
	// is rejected at boot; order is not semantic (the canonical map is
	// key-sorted).
	Profiles []VirtualAgentProfileConfig `yaml:"profiles,omitempty"`
}

// IsZero reports whether the block is entirely absent — the
// backward-compatible no-profiles path.
func (v VirtualAgentsConfig) IsZero() bool {
	return v.Owner == "" && len(v.Profiles) == 0
}

// VirtualAgentProfileConfig is the YAML declaration of ONE virtual-agent
// profile. Strict decoding (the loader's yaml.Strict) rejects any
// unknown field, so a widening dimension that has no canonical home
// (providers, hooks, memory, capabilities, a parent-profile reference,
// an A2A target) fails boot loud by construction.
type VirtualAgentProfileConfig struct {
	// Key is the profile identifier the planner's `_spawn_task`
	// `virtual_agent` argument selects. Restricted charset
	// `[a-z0-9][a-z0-9._-]*`, ≤ 128 bytes.
	Key string `yaml:"key"`
	// Label is the human-readable profile name (audit / observability).
	Label string `yaml:"label,omitempty"`
	// Parent is the owning top-level agent. Empty defaults to the block
	// owner; a non-empty value must equal it.
	Parent string `yaml:"parent,omitempty"`
	// LLM narrows the child's sampling parameters.
	LLM *VirtualAgentLLMConfig `yaml:"llm,omitempty"`
	// Skills narrows the child's skills to the intersection with the
	// parent's effective set. Omitted (nil) = no narrowing; an explicit
	// empty list narrows to no skills.
	Skills *[]string `yaml:"skills,omitempty"`
	// Tools narrows the child's tool exposure by UNIONING extra
	// exclusions onto the parent's effective exclusion set — it can
	// only hide tools, never re-expose one.
	Tools *VirtualAgentToolsConfig `yaml:"tools,omitempty"`
	// Limits narrows the child's run limits (max steps / token budget).
	Limits *VirtualAgentLimitsConfig `yaml:"limits,omitempty"`
	// Instructions adds trusted, operator-authored specialist guidance
	// rendered verbatim in the trusted additive prompt position,
	// bounded to 16 KiB.
	Instructions string `yaml:"instructions,omitempty"`
	// InputPatterns limits inherited artifact references by filename and MIME type.
	InputPatterns []string `yaml:"input_patterns,omitempty"`
	// InputCount limits the number of inherited artifact references.
	InputCount int `yaml:"input_count,omitempty"`
	// InputDisposition selects the disposition applied to accepted inputs.
	InputDisposition string `yaml:"input_disposition,omitempty"`
	// OutputSchema is the profile's terminal JSON Schema contract.
	OutputSchema json.RawMessage `yaml:"output_schema,omitempty"`
}

// VirtualAgentLLMConfig is the sampling-parameter narrowing section of
// a virtual-agent profile.
type VirtualAgentLLMConfig struct {
	// Model is the model the child's next run requests. Validated for
	// bounds at boot; the runtime's LLM surface serves it.
	Model string `yaml:"model,omitempty"`
	// Temperature is the sampling temperature in [0,2].
	Temperature *float64 `yaml:"temperature,omitempty"`
	// MaxTokens is the per-message output-token ceiling, clamped to the
	// parent's resolved ceiling at run start (never widened).
	MaxTokens *int `yaml:"max_tokens,omitempty"`
	// ReasoningEffort is one of off | low | medium | high.
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
}

// VirtualAgentToolsConfig is the tool-narrowing section of a
// virtual-agent profile (exclusions only — there is no enable field).
type VirtualAgentToolsConfig struct {
	// DisabledTools names tools additionally excluded from the child's
	// catalog view.
	DisabledTools []string `yaml:"disabled_tools,omitempty"`
	// PausedServers names MCP servers additionally paused for the
	// child's catalog view.
	PausedServers []string `yaml:"paused_servers,omitempty"`
}

// VirtualAgentLimitsConfig is the run-limit narrowing section of a
// virtual-agent profile (caps only — the child can never run longer or
// spend a larger budget than its parent allows).
type VirtualAgentLimitsConfig struct {
	// MaxSteps caps the child's run-loop step count, clamped to the
	// driver's run-loop cap.
	MaxSteps *int `yaml:"max_steps,omitempty"`
	// TokenBudget caps the child's trajectory-compression token budget.
	TokenBudget *int `yaml:"token_budget,omitempty"`
}
