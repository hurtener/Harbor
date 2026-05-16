// Package config defines Harbor's strongly-typed configuration surface.
//
// Configuration is loaded once at boot via Load (or LoadFromBytes for
// tests). After Load returns, the *Config is immutable; subsystems
// hold a *Config reference and read from it concurrently. This is the
// concurrent-reuse contract from D-025.
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
//     `restart` (whether explicitly tagged or absent). Phase 02 ships
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
	Audit     AuditConfig     `yaml:"audit,omitempty"`     // owned by Phase 03 + audit phases
	Protocol  ProtocolConfig  `yaml:"protocol,omitempty"`  // owned by protocol phases
	CLI       CLIConfig       `yaml:"cli,omitempty"`       // owned by CLI phases
	Tools     ToolsConfig     `yaml:"tools,omitempty"`     // owned by tools subsystem phases (26 / 27 / 28 / 29)

	// source records the originating filename for error messages.
	// Empty when LoadFromBytes is called without a name. Unexported so
	// it never appears in YAML / logging output.
	source string `yaml:"-"`
}

// ServerConfig is the network surface for the Harbor binary.
type ServerConfig struct {
	BindAddr            string        `yaml:"bind_addr"`
	ShutdownGracePeriod time.Duration `yaml:"shutdown_grace_period"`
}

// IdentityConfig configures JWT validation. Per AGENTS.md §7 the
// algorithm allowlist must contain only asymmetric algorithms.
type IdentityConfig struct {
	JWTAlgorithms []string `yaml:"jwt_algorithms"`
	Issuer        string   `yaml:"issuer"`
	Audience      string   `yaml:"audience"`
	JWKSURL       string   `yaml:"jwks_url,omitempty"`
	JWKSFile      string   `yaml:"jwks_file,omitempty"`
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
// (Phase 32+).
//
// `Driver` selects the §4.4 LLM driver. Phase 32 ships `"mock"`;
// Phase 33 registers `"bifrost"`. Empty defaults to `"mock"` so a
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
type LLMConfig struct {
	Driver               string                           `yaml:"driver"`
	Provider             string                           `yaml:"provider"`
	Model                string                           `yaml:"model"`
	APIKey               string                           `yaml:"api_key" secret:"true"`
	BaseURL              string                           `yaml:"base_url,omitempty"`
	Timeout              time.Duration                    `yaml:"timeout"`
	ContextWindowReserve float64                          `yaml:"context_window_reserve,omitempty"`
	ModelProfiles        map[string]LLMModelProfileConfig `yaml:"model_profiles,omitempty"`
	// Corrections toggles + per-model-profile-override the Phase 34
	// provider-correction layer. Omitted = enabled (production
	// default). See `LLMCorrectionsConfig` for the wire shape.
	Corrections LLMCorrectionsConfig `yaml:"corrections,omitempty"`

	// CustomProviders is the registry of operator-declared
	// OpenAI-compatible providers (Phase 33a). Each entry adds a new
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
	// fall through to bifrost's package-level defaults (Phase 33a
	// unification of timeout/retry knobs that were previously
	// scattered). Restart-required.
	NetworkDefaults LLMNetworkDefaults `yaml:"network_defaults,omitempty"`
}

// LLMCustomProviderConfig declares one operator-configured
// OpenAI-compatible LLM endpoint (Phase 33a). At least one entry is
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
//   - `BaseProviderType` — wire family. Phase 33a accepts only `""`
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
// absent (Phase 33a). Zero-valued fields fall through to bifrost's
// package-level defaults so an operator who omits the block sees
// today's Phase 33 behaviour unchanged.
type LLMNetworkDefaults struct {
	Timeout             time.Duration `yaml:"timeout,omitempty"`
	MaxRetries          int           `yaml:"max_retries,omitempty"`
	RetryBackoffInitial time.Duration `yaml:"retry_backoff_initial,omitempty"`
	RetryBackoffMax     time.Duration `yaml:"retry_backoff_max,omitempty"`
	Concurrency         int           `yaml:"concurrency,omitempty"`
	BufferSize          int           `yaml:"buffer_size,omitempty"`
}

// LLMCorrectionsConfig is the top-level toggle for the Phase 34
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
// `"google/gemini-3.1-flash-lite"`). Phase 32 ships the shape;
// Phase 33+ consume the fields.
//
//   - `ContextWindowTokens` is the model's hard input-token cap.
//     REQUIRED (> 0); the safety net's token-budget guard uses it.
//   - `TokenEstimator` selects the estimator algorithm. "" / "chars_div_4"
//     — default chars/4 + per-message overhead. Phase 33+ may
//     register tiktoken-equivalent estimators by name.
//   - `JSONSchemaMode` — Phase 35 reads ("native" / "tools" /
//     "prompted"). Phase 32 stores opaque.
//   - `DefaultMaxTokens` — Phase 36b's identity-tier override
//     target. nil → use the runtime/governance default.
//   - `ReasoningEffort` — request-level default. Empty string →
//     "use provider default."
//   - `CostOverrides` — fallback per-1M-token rates when the
//     provider doesn't include cost in its response. Phase 36a
//     reads when accumulating identity-scoped cost.
type LLMModelProfileConfig struct {
	ContextWindowTokens int                     `yaml:"context_window_tokens"`
	TokenEstimator      string                  `yaml:"token_estimator,omitempty"`
	JSONSchemaMode      string                  `yaml:"json_schema_mode,omitempty"`
	DefaultMaxTokens    *int                    `yaml:"default_max_tokens,omitempty"`
	ReasoningEffort     string                  `yaml:"reasoning_effort,omitempty"`
	CostOverrides       *LLMCostOverridesConfig `yaml:"cost_overrides,omitempty"`
	// Corrections — per-model overrides for the Phase 34 correction
	// layer. nil → use the per-provider defaults baked into
	// `internal/llm/corrections.defaultProfileFor`. See
	// `LLMCorrectionsProfileConfig` for the wire shape.
	Corrections *LLMCorrectionsProfileConfig `yaml:"corrections,omitempty"`
	// MaxRetries (Phase 36) — caps the validator-driven retry loop the
	// `internal/llm/retry` wrapper runs against this model. Zero (the
	// default) maps to `llm.DefaultMaxRetries` (1). Negative is
	// rejected by `validateLLM`.
	MaxRetries int `yaml:"max_retries,omitempty"`
}

// LLMCorrectionsProfileConfig is one per-model profile override for
// the Phase 34 correction layer. Each field is the operator-facing
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
// when the provider response doesn't include cost). Phase 36a reads.
type LLMCostOverridesConfig struct {
	InputPer1M     float64 `yaml:"input_per_1m_tokens"`
	OutputPer1M    float64 `yaml:"output_per_1m_tokens"`
	ReasoningPer1M float64 `yaml:"reasoning_per_1m_tokens,omitempty"`
	Currency       string  `yaml:"currency,omitempty"`
}

// GovernanceConfig holds the V1 governance policy surface — per-tier
// cost ceilings, rate limits, and MaxTokens caps — all enforced
// exclusively through `IdentityTiers` (Phase 36a + 36b). Hot-reload is
// not yet wired; every field is restart-required.
//
// **Latent default (Wave 7b scoping):** an empty `IdentityTiers` map +
// empty `DefaultTier` disables every per-policy enforcement. Operators
// turn on enforcement per tier by populating `IdentityTiers` with at
// least one `GovernanceTierConfig` entry and (optionally) setting
// `DefaultTier`.
//
// **Removed in D-081 (chore/governance-config-consolidation):** the
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

	// IdentityTiers maps tier name to its policy bundle. Empty = no
	// enforcement (latent default). Each entry's fields are
	// independently opt-in — set `budget_ceiling_usd` only to enforce
	// cost ceilings, leaving rate-limit + MaxTokens latent.
	IdentityTiers map[string]GovernanceTierConfig `yaml:"identity_tiers,omitempty"`
}

// GovernanceTierConfig is one tier's policy bundle (Phase 36a + 36b).
// Each field is independently opt-in: set the cost field only to enforce
// the cost ceiling, leave the rest zero-valued for latent behaviour.
//
// `BudgetCeilingUSD` — Phase 36a. Per-identity cost ceiling. PreCall
// blocks when the (identity, tier) accumulator total ≥ this. 0 = no
// ceiling.
//
// `RateLimit` — Phase 36b. Per-(identity, model) token bucket. Zero-
// valued (Capacity == 0) = no rate limit.
//
// `MaxTokens` — Phase 36b. Per-call cap. Requests whose `MaxTokens`
// exceed this fail loudly with `ErrMaxTokensExceeded`. 0 = no cap.
type GovernanceTierConfig struct {
	BudgetCeilingUSD float64                   `yaml:"budget_ceiling_usd,omitempty"`
	RateLimit        GovernanceRateLimitConfig `yaml:"rate_limit,omitempty"`
	MaxTokens        int                       `yaml:"max_tokens,omitempty"`
}

// GovernanceRateLimitConfig is the token-bucket shape (Phase 36b).
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
// `Driver` selects a `memory.MemoryStore` driver. Phase 23 ships
// only `"inmem"`; Phase 25 adds `"sqlite"` and `"postgres"`. Default
// `inmem`.
//
// `DSN` is required when `Driver` is `"sqlite"` or `"postgres"`.
// The format mirrors the StateStore + ArtifactStore drivers (bare
// file path or `file:` URI for SQLite; libpq-compatible connection
// string for Postgres). `secret:"true"` redacts the value in
// audit-redacted logs.
//
// `Strategy` selects the memory shape: `"none"` (Phase 23), or
// `"truncation"` / `"rolling_summary"` (Phase 24). Default `none`.
// `memory.Open` rejects strategies the configured driver does not
// implement with `ErrStrategyNotImplemented`.
//
// `BudgetTokens` is the truncation / rolling-summary budget cap
// (token estimate). Zero means "no budget" — appending is
// unbounded.
//
// `RecoveryBacklogMax` is the bounded queue size for the
// `rolling_summary` strategy's recovery loop (D-035). Default 16
// (applied by the loader when the section is omitted). Overflow
// drops oldest and emits `memory.recovery_dropped` on the bus.
// Ignored by the `none` and `truncation` strategies.
//
// Restart-required (no `reload:"live"`).
type MemoryConfig struct {
	Driver             string `yaml:"driver"`
	DSN                string `yaml:"dsn,omitempty" secret:"true"`
	Strategy           string `yaml:"strategy,omitempty"`
	BudgetTokens       int    `yaml:"budget_tokens,omitempty"`
	RecoveryBacklogMax int    `yaml:"recovery_backlog_max,omitempty"`
}

// SkillsConfig is owned by the skills subsystem phases.
//
// `Driver` selects a `skills.SkillStore` driver. Phase 37 ships the
// `"localdb"` driver only; Phase 49 (Portico) adds `"portico"`.
// Default `localdb`.
//
// `DSN` is required when `Driver` is `"localdb"`. The format mirrors
// the StateStore + MemoryStore drivers (bare file path or `file:`
// URI for SQLite; `:memory:` honoured for tests). `secret:"true"`
// redacts the value in audit-redacted logs.
type SkillsConfig struct {
	Driver string `yaml:"driver,omitempty"`
	DSN    string `yaml:"dsn,omitempty" secret:"true"`
}

// TasksConfig configures the TaskRegistry driver and the Phase 21
// backgroundtasks-config knobs. `Driver` selects the registered
// driver name; Phase 20 ships only `"inprocess"`.
//
// `RetainTurnTimeout` is the maximum time the runtime engine will
// block a foreground turn waiting for retain-turn groups to resolve.
// Defaults to 5 minutes (RFC §6.8); zero or negative values are
// rejected by validation. Consumed by the engine wiring scheduled
// for Phase 60+ (runtime↔tasks integration); validated today so an
// operator's deployment is rejected for an invalid value even
// before the consumer lands.
//
// `ContinuationHopLimit` caps the number of background-continuation
// hops a planner runtime may take before requiring user
// confirmation. Defaults to 8 (RFC §6.8); zero or negative values
// are rejected by validation. Consumed by the planner concretes
// (Phase 42+); same "validate today, consume later" pattern as
// `RetainTurnTimeout`.
//
// Restart-required (no `reload:"live"` tag).
type TasksConfig struct {
	Driver               string        `yaml:"driver"`
	RetainTurnTimeout    time.Duration `yaml:"retain_turn_timeout"`
	ContinuationHopLimit int           `yaml:"continuation_hop_limit"`
}

// DistributedConfig configures Harbor's distributed contracts (Phase
// 22). `BusDriver` selects the MessageBus driver; `RemoteDriver`
// selects the RemoteTransport driver. V1 ships only `"loopback"` for
// both. Post-V1 phase 86 adds durable bus drivers; Phase 29 adds the
// A2A wire RemoteTransport driver. Restart-required (no `reload:"live"`).
type DistributedConfig struct {
	BusDriver    string `yaml:"bus_driver"`
	RemoteDriver string `yaml:"remote_driver"`
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

// ArtifactsConfig configures the ArtifactStore driver, the
// filesystem-driver root path, the SQL-driver connection string, and
// the heavy-output threshold above which the runtime mandatorily
// routes payloads through the store.
//
// `Driver` selects an artifacts driver. V1 ships five drivers:
// `inmem` (the floor; per-process lifetime, no persistence), `fs`
// (single-binary production target), `sqlite` (Phase 18 — SQLite-
// backed, durable across restart), `postgres` (Phase 18 —
// Postgres-backed, durable across restart, multi-replica safe), and
// `s3` (Phase 19 — S3-compatible object-store-backed, durable;
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
// 32 KB (D-022, RFC §6.10). Per-tool overrides land at Phase 26 via
// the tool catalog; the field is the runtime-wide default. Consumed
// by the tool dispatcher (Phase 26+) and the LLM-edge catch-all
// (Phase 32) — validated today so an operator's deployment is
// rejected for an invalid value even before the consumers land.
//
// S3* fields configure the Phase 19 S3-style driver (AWS S3 / MinIO /
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
// limits. Phase 05 filled the previously-reserved slot with the
// inmem driver's defaults; Phase 06 adds ReplayBufferSize for the
// in-memory ring buffer that backs Replayer. Phase 57 registers the
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
// (Phase 57) persists events through. They are OPTIONAL and ignored
// by every other driver: when Driver=="durable" and StateDriver is
// empty, the durable driver auto-degrades to a best-effort in-memory
// ring buffer and emits a loud runtime.warning (D-074) — replay is
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

// AuditConfig is owned by Phase 03 + later audit phases.
type AuditConfig struct{}

// ToolsConfig is owned by the tools subsystem phases (Phase 26+).
// The block is optional — operators who don't attach external tool
// sources omit it entirely.
//
// `HTTPManifests` lists paths to UTCP-style YAML manifests loaded
// at boot by Phase 27's HTTP driver. Paths may be absolute or
// relative; the loader rejects empty strings and resolves each via
// `filepath.Clean` before reading. An empty list is valid.
//
// `MCPServers` lists Phase 28's MCP southbound attachments. Each
// entry boots a `*mcp.Provider` whose discovered tools / resources
// / prompts are merged into the runtime catalog.
//
// `A2APeers` lists Phase 29's A2A peers. The wire driver
// (`internal/distributed/drivers/a2a`) reads this slice at
// construction.
//
// `Entries` lists per-tool catalog wiring declarations: operators
// attach approval policies and / or OAuth bindings to a tool name
// without writing Go wiring code. Phase 64a (D-090) — the catalog
// builder reads this list at boot and auto-wraps each named tool's
// descriptor with the declared middleware. An entry whose Name does
// not resolve to a registered tool fails the catalog build loud
// (§13 "fail loudly at boot"); an entry that names an unknown
// approval policy or OAuth provider also fails loud.
//
// Restart-required (no `reload:"live"` tag): adding / removing tool
// providers at runtime is a Phase 91+ Protocol surface concern.
//
// `OAuthProviders` closes D-090's deferred "OAuth provider construction"
// gap (issue #116 / D-095). Each entry declares one named OAuth provider
// resolved at boot through the `internal/tools/auth` driver registry
// (§4.4 seam). The V1 default driver is `oauth2` (generic OAuth2/PKCE
// Authorization Code flow). When any `tools.entries[].oauth.provider`
// references a name, that name MUST appear in `OAuthProviders` —
// validateTools enforces.
//
// `OAuthTokenKEKEnv` names the env var holding the 32-byte hex-encoded
// key-encryption key (KEK) the OAuth token store consumes for
// AES-256-GCM encryption at rest (§7 + Phase 30). Required whenever
// `OAuthProviders` is non-empty; the dev-stack reads the env at boot
// and fails closed when the env value is empty or wrong-length.
type ToolsConfig struct {
	HTTPManifests    []string                  `yaml:"http_manifests,omitempty"`
	MCPServers       []MCPServerConfig         `yaml:"mcp_servers,omitempty"`
	A2APeers         []A2APeerConfig           `yaml:"a2a_peers,omitempty"`
	Entries          []ToolEntryConfig         `yaml:"entries,omitempty"`
	OAuthProviders   []ToolOAuthProviderConfig `yaml:"oauth_providers,omitempty"`
	OAuthTokenKEKEnv string                    `yaml:"oauth_token_kek_env,omitempty"`
}

// ToolOAuthProviderConfig declares one operator-configured OAuth
// provider (D-095, closes issue #116 and D-090's deferred construction
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

// ToolEntryConfig is one per-tool catalog wiring declaration. Phase
// 64a / D-090. The shape is intentionally small: the catalog builder
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
	// Approval declares an approval-gate wiring for this tool. Omit
	// for tools that need no gating. When present, `Approval.Policy`
	// MUST be one of the canonical policy names; an unknown value
	// fails closed.
	Approval *ToolApprovalConfig `yaml:"approval,omitempty"`
	// OAuth declares an OAuth binding for this tool. Omit for tools
	// that need no OAuth. When present, `OAuth.Provider` MUST name a
	// configured OAuth source and `OAuth.BindingScope` MUST be one of
	// "user" / "agent" (Phase 30 D-083).
	OAuth *ToolOAuthConfig `yaml:"oauth,omitempty"`
}

// ToolApprovalConfig declares an approval-gate wiring for one tool.
// Phase 64a / D-090.
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

// ToolOAuthConfig declares an OAuth binding for one tool. Phase 64a
// / D-090.
type ToolOAuthConfig struct {
	// Provider names the OAuth source the tool binds to. The catalog
	// builder consults the configured OAuth registry; a name that
	// resolves to no source fails the catalog build loud.
	Provider string `yaml:"provider"`
	// BindingScope is "user" or "agent" (Phase 30 / D-083). An
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
type ProtocolConfig struct{}

// CLIConfig is owned by the CLI phases.
type CLIConfig struct{}
