package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"strings"
	"time"
)

// allowedJWTAlgorithms is the asymmetric-only allowlist enforced by
// IdentityConfig validation. Per AGENTS.md §7 (security rule 1),
// HS*/none are NEVER acceptable.
var allowedJWTAlgorithms = map[string]struct{}{
	"RS256": {},
	"RS384": {},
	"RS512": {},
	"ES256": {},
	"ES384": {},
	"ES512": {},
}

var (
	allowedLogFormats = map[string]struct{}{"json": {}, "text": {}}
	allowedLogLevels  = map[string]struct{}{"debug": {}, "info": {}, "warn": {}, "error": {}}
	allowedDrivers    = map[string]struct{}{"inmem": {}, "sqlite": {}, "postgres": {}}
)

// Validate runs every section validator and returns the first error,
// formatted with the offending YAML path and the source filename
// (when known). Nil on success.
//
// This is the full-binary profile: it includes the Protocol-server
// identity ceremony (JWT algorithms / issuer / audience / JWKS) that a
// Runtime serving the Protocol edge MUST carry. Headless library
// consumers that never serve the Protocol validate with ValidateCore
// instead. `Load` always runs the full Validate — a YAML-loaded config
// is binary-shaped by definition.
func (c *Config) Validate() error {
	return c.runValidators(true)
}

// ValidateCore runs every section validator EXCEPT the Protocol-server
// identity ceremony (`identity.jwt_algorithms` / `issuer` / `audience`
// / JWKS source — the `validateIdentity` section). Rationale:
// a Go consumer embedding the Runtime headless — never serving the
// Protocol — is not forced to configure a JWT surface it never serves.
//
// The profile is subtractive and minimal: ONLY the identity section is
// skipped. Everything a headless embedder can meaningfully configure
// (state / llm / events / sessions / artifacts / tasks / memory /
// skills / tools / planner / governance / telemetry / server / CLI)
// stays validated — anything ambiguous stays in core (fail-closed
// bias). Full `Validate()` semantics are unchanged; a config that
// passes Validate always passes ValidateCore.
func (c *Config) ValidateCore() error {
	return c.runValidators(false)
}

// runValidators is the shared section-validator walk. The order is
// load-bearing for error precedence (first failure wins) and mirrors
// the pre-110c Validate order exactly; includeIdentity toggles the one
// section the ValidateCore profile subtracts.
func (c *Config) runValidators(includeIdentity bool) error {
	validators := []func() error{
		c.validateServer,
	}
	if includeIdentity {
		validators = append(validators, c.validateIdentity)
	}
	validators = append(validators,
		c.validateTelemetry,
		c.validateState,
		c.validateLLM,
		c.validateEmbeddings,
		c.validateGovernance,
		c.validateEvents,
		c.validateSessions,
		c.validatePauseResume,
		c.validateArtifacts,
		c.validateTasks,
		c.validateDistributed,
		c.validateMemory,
		c.validateSkills,
		c.validateTools,
		c.validatePlanner,
		c.validateMultimodal,
		c.validateCLI,
	)
	for _, v := range validators {
		if err := v(); err != nil {
			return c.wrapValidationError(err)
		}
	}
	return nil
}

func (c *Config) wrapValidationError(err error) error {
	src := c.source
	if src == "" {
		src = "<unknown>"
	}
	return fmt.Errorf("%w (source: %s)", err, src)
}

func (c *Config) validateServer() error {
	if c.Server.BindAddr == "" {
		return fieldError("server.bind_addr", "must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.Server.BindAddr); err != nil {
		return fieldError("server.bind_addr",
			fmt.Sprintf("must be host:port, got %q (%v)", c.Server.BindAddr, err))
	}
	if c.Server.ShutdownGracePeriod <= 0 {
		return fieldError("server.shutdown_grace_period", "must be > 0")
	}
	// CORS allowlist validation. Each entry must be
	// an exact origin (`scheme://host[:port]`); wildcards are forbidden
	// unless the operator explicitly opts in via `server.cors_dev_allow_any`.
	// CLAUDE.md §7: never wildcard in production.
	for i, raw := range c.Server.AllowedOrigins {
		o := strings.TrimSpace(raw)
		if o == "" {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				"must not be empty or whitespace")
		}
		if o == "*" || strings.Contains(o, "*") {
			if !c.Server.CORSDevAllowAny {
				return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
					"wildcard (\"*\") not allowed; set server.cors_dev_allow_any: true to enable the dev-only any-origin posture (NEVER in production)")
			}
			// Wildcard entry with the dev flag set is a no-op — the
			// dev-any path is driven by CORSDevAllowAny directly, not
			// by an allowlist entry. Skip the URL-shape check.
			continue
		}
		u, err := url.Parse(o)
		if err != nil {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				fmt.Sprintf("must be a valid origin (scheme://host[:port]), got %q (%v)", raw, err))
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				fmt.Sprintf("scheme must be http or https, got %q in %q", u.Scheme, raw))
		}
		if u.Host == "" {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				fmt.Sprintf("host must be non-empty, got %q", raw))
		}
		if u.Path != "" && u.Path != "/" {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				fmt.Sprintf("must be an origin (no path), got %q", raw))
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fieldError(fmt.Sprintf("server.allowed_origins[%d]", i),
				fmt.Sprintf("must be an origin (no query or fragment), got %q", raw))
		}
	}
	return nil
}

func (c *Config) validateIdentity() error {
	if len(c.Identity.JWTAlgorithms) == 0 {
		return fieldError("identity.jwt_algorithms",
			"must list at least one asymmetric algorithm (RS256/RS384/RS512/ES256/ES384/ES512)")
	}
	for _, alg := range c.Identity.JWTAlgorithms {
		if _, ok := allowedJWTAlgorithms[alg]; !ok {
			return fieldError("identity.jwt_algorithms",
				fmt.Sprintf("algorithm %q not allowed; allowed: %s",
					alg, sortedKeys(allowedJWTAlgorithms)))
		}
	}
	if c.Identity.Issuer == "" {
		return fieldError("identity.issuer", "must not be empty")
	}
	if c.Identity.Audience == "" {
		return fieldError("identity.audience", "must not be empty")
	}
	if c.Identity.JWKSURL == "" && c.Identity.JWKSFile == "" {
		return fieldError("identity",
			"one of jwks_url or jwks_file must be set")
	}
	if c.Identity.JWKSURL != "" && c.Identity.JWKSFile != "" {
		return fieldError("identity",
			"set only one of jwks_url or jwks_file, not both")
	}
	return nil
}

func (c *Config) validateTelemetry() error {
	if _, ok := allowedLogFormats[c.Telemetry.LogFormat]; !ok {
		return fieldError("telemetry.log_format",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedLogFormats), c.Telemetry.LogFormat))
	}
	if _, ok := allowedLogLevels[c.Telemetry.LogLevel]; !ok {
		return fieldError("telemetry.log_level",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedLogLevels), c.Telemetry.LogLevel))
	}
	if c.Telemetry.ServiceName == "" {
		return fieldError("telemetry.service_name", "must not be empty")
	}
	return nil
}

func (c *Config) validateState() error {
	if _, ok := allowedDrivers[c.State.Driver]; !ok {
		return fieldError("state.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedDrivers), c.State.Driver))
	}
	if c.State.Driver != "inmem" && c.State.DSN == "" {
		return fieldError("state.dsn",
			fmt.Sprintf("must be set when driver=%q", c.State.Driver))
	}
	return nil
}

// allowedLLMDrivers is the registered-driver allowlist Harbor ships
// with. Harbor adds "bifrost" here when its driver registers.
var allowedLLMDrivers = map[string]struct{}{
	"mock":    {},
	"bifrost": {}, // A later phase will register the factory; the name is reserved here so a config that targets bifrost passes validation today and only the registry-miss fires at runtime.
}

func (c *Config) validateLLM() error {
	// Driver — empty is accepted and treated as the runtime's
	// `llm.DefaultDriver` (flipped this to
	// `"bifrost"`). The loader's `Defaults()` populates the same
	// string so any production config loaded from YAML carries an
	// explicit driver; hand-constructed config values (e.g. in tests
	// built previously) keep working with `"mock"` when the
	// test blank-imports the mock package to seat its registration.
	driver := c.LLM.Driver
	if driver == "" {
		driver = "bifrost"
	}
	if _, ok := allowedLLMDrivers[driver]; !ok {
		return fieldError("llm.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedLLMDrivers), c.LLM.Driver))
	}
	// Custom-provider validation runs before the legacy single-provider
	// checks so we can decide which path applies. When
	// `llm.provider` matches a custom-provider `name`, the entry's
	// `base_url`/`api_key_env_var`/`models`/`timeout` fill the role
	// the legacy `llm.base_url`/`llm.api_key`/`llm.timeout` fields
	// played; the legacy fields stay optional in that case.
	customNames, err := c.validateLLMCustomProviders(driver)
	if err != nil {
		return err
	}
	// Validate network defaults independently of which provider path
	// applies — operators may tune them with a native primary too.
	if err := c.validateLLMNetworkDefaults(); err != nil {
		return err
	}
	// Bifrost-driver knobs are required only for real drivers; the
	// mock driver ignores Provider/Model/APIKey/Timeout. Keep the
	// canonical fixture valid by enforcing these when driver != "mock".
	if driver != "mock" {
		if c.LLM.Provider == "" {
			return fieldError("llm.provider", "must not be empty")
		}
		if c.LLM.Model == "" {
			return fieldError("llm.model", "must not be empty")
		}
		// When `llm.provider` names a custom-provider entry, the
		// entry's `api_key_env_var` / `base_url` / `timeout` apply —
		// the legacy `llm.api_key` / `llm.base_url` / `llm.timeout`
		// fields are not required.
		_, isCustom := customNames[c.LLM.Provider]
		if !isCustom {
			if c.LLM.APIKey == "" {
				return fieldError("llm.api_key", "must not be empty (or declare llm.provider as a custom-provider name)")
			}
			if c.LLM.Timeout <= 0 {
				return fieldError("llm.timeout", "must be > 0 (or declare llm.provider as a custom-provider name)")
			}
		}
	}
	// Context-window reserve is the safety-net's token-budget
	// margin. 0 (zero) is accepted as a sentinel for "use the
	// runtime default" (`llm.DefaultContextWindowReserve = 0.05`);
	// values >= 1 are rejected because they would fail every
	// request.
	if c.LLM.ContextWindowReserve < 0 || c.LLM.ContextWindowReserve >= 1.0 {
		return fieldError("llm.context_window_reserve",
			fmt.Sprintf("must be in [0, 1), got %g", c.LLM.ContextWindowReserve))
	}
	// Each model profile must declare a positive context-window
	// cap. The safety net's token-budget guard depends on this.
	for name, prof := range c.LLM.ModelProfiles {
		if prof.ContextWindowTokens <= 0 {
			return fieldError(
				fmt.Sprintf("llm.model_profiles[%q].context_window_tokens", name),
				fmt.Sprintf("must be > 0, got %d", prof.ContextWindowTokens),
			)
		}
		if prof.DefaultMaxTokens != nil && *prof.DefaultMaxTokens <= 0 {
			return fieldError(
				fmt.Sprintf("llm.model_profiles[%q].default_max_tokens", name),
				"must be > 0 when set",
			)
		}
		if prof.CostOverrides != nil {
			if prof.CostOverrides.InputPer1M < 0 || prof.CostOverrides.OutputPer1M < 0 || prof.CostOverrides.ReasoningPer1M < 0 {
				return fieldError(
					fmt.Sprintf("llm.model_profiles[%q].cost_overrides", name),
					"per-1m rates must be >= 0",
				)
			}
		}
		// correction-layer overrides — validate enum values.
		// Empty string is always valid (= use per-provider default).
		if prof.Corrections != nil {
			if err := validateCorrectionsProfile(name, prof.Corrections); err != nil {
				return err
			}
		}
		// JSONSchemaMode is the legacy operator-facing string
		// that the snapshot normalises into `llm.OutputMode`. Validate
		// the enum here so operators get a useful error at boot.
		if _, ok := allowedJSONSchemaModes[prof.JSONSchemaMode]; !ok {
			return fieldError(
				fmt.Sprintf("llm.model_profiles[%q].json_schema_mode", name),
				fmt.Sprintf("must be one of \"\", \"native\", \"tools\", \"prompted\"; got %q", prof.JSONSchemaMode),
			)
		}
		// MaxRetries must be non-negative.
		if prof.MaxRetries < 0 {
			return fieldError(
				fmt.Sprintf("llm.model_profiles[%q].max_retries", name),
				fmt.Sprintf("must be >= 0, got %d", prof.MaxRetries),
			)
		}
	}
	return nil
}

// allowedJSONSchemaModes is the enum allowlist for
// `LLMModelProfileConfig.JSONSchemaMode`. Empty string is the
// "operator did not declare" sentinel — the snapshot's
// `applyDefaults` will fall back to the per-provider default
// `llm.OutputMode` via `corrections.DefaultOutputModeFor`.
var allowedJSONSchemaModes = map[string]struct{}{
	"":         {},
	"native":   {},
	"tools":    {},
	"prompted": {},
}

// allowedMessageOrderings is the enum allowlist for
// `LLMCorrectionsProfileConfig.MessageOrdering`. Empty is always
// valid; explicit values must match.
var allowedMessageOrderings = map[string]struct{}{
	"":                    {},
	"system_first_strict": {},
}

// allowedSchemaModes is the enum allowlist for
// `LLMCorrectionsProfileConfig.SchemaMode`.
var allowedSchemaModes = map[string]struct{}{
	"":              {},
	"openai_strict": {},
	"permissive":    {},
}

// allowedReasoningRoutings is the enum allowlist for
// `LLMCorrectionsProfileConfig.ReasoningEffortRouting`.
var allowedReasoningRoutings = map[string]struct{}{
	"":               {},
	"thinking_model": {},
}

// allowedResponseFormatShapes is the enum allowlist for
// `LLMCorrectionsProfileConfig.ResponseFormatShape`.
var allowedResponseFormatShapes = map[string]struct{}{
	"":          {},
	"json_only": {},
	"anthropic": {},
}

// validateCorrectionsProfile enforces the per-profile
// correction-layer enum constraints. Each enum's empty string maps
// to "use the per-provider default" — the operator opts in by setting
// a specific value.
func validateCorrectionsProfile(name string, c *LLMCorrectionsProfileConfig) error {
	if _, ok := allowedMessageOrderings[c.MessageOrdering]; !ok {
		return fieldError(
			fmt.Sprintf("llm.model_profiles[%q].corrections.message_ordering", name),
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedMessageOrderings), c.MessageOrdering),
		)
	}
	if _, ok := allowedSchemaModes[c.SchemaMode]; !ok {
		return fieldError(
			fmt.Sprintf("llm.model_profiles[%q].corrections.schema_mode", name),
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedSchemaModes), c.SchemaMode),
		)
	}
	if _, ok := allowedReasoningRoutings[c.ReasoningEffortRouting]; !ok {
		return fieldError(
			fmt.Sprintf("llm.model_profiles[%q].corrections.reasoning_effort_routing", name),
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedReasoningRoutings), c.ReasoningEffortRouting),
		)
	}
	if _, ok := allowedResponseFormatShapes[c.ResponseFormatShape]; !ok {
		return fieldError(
			fmt.Sprintf("llm.model_profiles[%q].corrections.response_format_shape", name),
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedResponseFormatShapes), c.ResponseFormatShape),
		)
	}
	return nil
}

func (c *Config) validateGovernance() error {
	if c.Governance.RepairAttempts < 0 {
		return fieldError("governance.repair_attempts", "must be >= 0")
	}
	// validate the IdentityTiers block. Empty map is
	// the latent default (no enforcement); the validator rejects only
	// malformed entries. The pre-Phase-36a single-knob fields
	// (`default_max_tokens`, `cost_ceiling_usd`, `rate_limit_tps`)
	// were removed — the loader now emits a deprecation
	// warning and drops them before this validator runs, so there is
	// nothing left for `validateGovernance` to reject for those keys.
	for name, tier := range c.Governance.IdentityTiers {
		if name == "" {
			return fieldError("governance.identity_tiers", "tier names must not be empty")
		}
		if tier.BudgetCeilingUSD < 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].budget_ceiling_usd", name),
				"must be >= 0 (omit to disable)",
			)
		}
		if tier.MaxTokens < 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].max_tokens", name),
				"must be >= 0 (omit to disable)",
			)
		}
		rl := tier.RateLimit
		if rl.Capacity < 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].rate_limit.capacity", name),
				"must be >= 0 (omit to disable)",
			)
		}
		if rl.RefillTokens < 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].rate_limit.refill_tokens", name),
				"must be >= 0",
			)
		}
		if rl.RefillInterval < 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].rate_limit.refill_interval", name),
				"must be >= 0",
			)
		}
		// Coherence checks — partial rate-limit config is operator-
		// confusing. Enforce: if any of (Capacity, RefillTokens,
		// RefillInterval) is set, RefillInterval must be > 0 OR
		// Capacity must be set (one-shot bucket is allowed: drains to
		// zero, never refills).
		if (rl.RefillTokens > 0 || rl.RefillInterval > 0) && rl.Capacity == 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].rate_limit.capacity", name),
				"must be > 0 when refill_tokens or refill_interval is set",
			)
		}
		if rl.RefillTokens > 0 && rl.RefillInterval == 0 {
			return fieldError(
				fmt.Sprintf("governance.identity_tiers[%q].rate_limit.refill_interval", name),
				"must be > 0 when refill_tokens is set",
			)
		}
	}
	if c.Governance.DefaultTier != "" {
		if _, ok := c.Governance.IdentityTiers[c.Governance.DefaultTier]; !ok {
			return fieldError("governance.default_tier",
				fmt.Sprintf("%q must reference an entry in identity_tiers", c.Governance.DefaultTier))
		}
	}
	return nil
}

// allowedEventDrivers is the registered-driver allowlist. An earlier phase
// shipped "inmem"; Harbor adds "durable" (the StateStore-backed
// event log).
var allowedEventDrivers = map[string]struct{}{"inmem": {}, "durable": {}}

func (c *Config) validateEvents() error {
	if c.Events.Driver == "" {
		return fieldError("events.driver", "must not be empty")
	}
	if _, ok := allowedEventDrivers[c.Events.Driver]; !ok {
		return fieldError("events.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedEventDrivers), c.Events.Driver))
	}
	if c.Events.MaxSubscribersPerSession <= 0 {
		return fieldError("events.max_subscribers_per_session", "must be > 0")
	}
	if c.Events.SubscriberBufferSize <= 0 {
		return fieldError("events.subscriber_buffer_size", "must be > 0")
	}
	if c.Events.IdleTimeout <= 0 {
		return fieldError("events.idle_timeout", "must be > 0")
	}
	if c.Events.DropWindow <= 0 {
		return fieldError("events.drop_window", "must be > 0")
	}
	if c.Events.ReplayBufferSize < 0 {
		return fieldError("events.replay_buffer_size", "must be >= 0 (zero disables replay)")
	}
	// StateDriver / StateDSN are optional and only meaningful for the
	// `durable` driver. When set they must name a real StateStore
	// driver and pair a DSN with any non-inmem backend (mirrors
	// validateState). An empty StateDriver is valid even for the
	// durable driver — it triggers the loud best-effort degradation,
	// not a config error.
	if c.Events.StateDriver != "" {
		if _, ok := allowedDrivers[c.Events.StateDriver]; !ok {
			return fieldError("events.state_driver",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedDrivers), c.Events.StateDriver))
		}
		if c.Events.StateDriver != "inmem" && c.Events.StateDSN == "" {
			return fieldError("events.state_dsn",
				fmt.Sprintf("must be set when events.state_driver=%q", c.Events.StateDriver))
		}
	}
	return nil
}

func (c *Config) validateSessions() error {
	if c.Sessions.IdleTTL <= 0 {
		return fieldError("sessions.idle_ttl", "must be > 0")
	}
	if c.Sessions.HardCap <= 0 {
		return fieldError("sessions.hard_cap", "must be > 0")
	}
	if c.Sessions.SweepInterval <= 0 {
		return fieldError("sessions.sweep_interval", "must be > 0")
	}
	if c.Sessions.IdleTTL > c.Sessions.HardCap {
		return fieldError("sessions.idle_ttl",
			fmt.Sprintf("must be <= sessions.hard_cap (%s); got %s",
				c.Sessions.HardCap, c.Sessions.IdleTTL))
	}
	if c.Sessions.SweepInterval > c.Sessions.IdleTTL {
		return fieldError("sessions.sweep_interval",
			fmt.Sprintf("must be <= sessions.idle_ttl (%s) so sessions can't live past TTL by more than one sweep; got %s",
				c.Sessions.IdleTTL, c.Sessions.SweepInterval))
	}
	return nil
}

// validatePauseResume validates the pause-lifecycle
// block. Both fields are zero-meaning-default because the block is
// OFF by default (unlike the always-on sessions sweeper):
// max_park_duration 0 = pauses never expire and no sweeper starts;
// sweep_interval 0 = the documented 1m default applies (Defaults()
// sets it; a hand-built Config without the block stays valid).
// Negative values are rejected loud rather than silently treated as
// the default. When both are set, the sweep cadence must not exceed
// the park ceiling — otherwise a pause overstays its deadline by more
// than one sweep (same posture as sessions.sweep_interval vs
// idle_ttl).
func (c *Config) validatePauseResume() error {
	if c.PauseResume.MaxParkDuration < 0 {
		return fieldError("pauseresume.max_park_duration", "must be >= 0 (0 = pauses never expire)")
	}
	if c.PauseResume.SweepInterval < 0 {
		return fieldError("pauseresume.sweep_interval", "must be >= 0 (0 = the documented 1m default)")
	}
	if c.PauseResume.MaxParkDuration > 0 {
		// The one-sweep-overstay invariant is checked against the
		// EFFECTIVE interval: an explicit `sweep_interval: 0` means the
		// documented 1m default applies, and that default must not
		// exceed the park ceiling either (checkpoint audit — a
		// 30s max_park_duration with the defaulted 1m cadence would
		// overstay its own validated invariant).
		effective := c.PauseResume.SweepInterval
		if effective == 0 {
			effective = defaultPauseSweepInterval
		}
		if effective > c.PauseResume.MaxParkDuration {
			return fieldError("pauseresume.sweep_interval",
				fmt.Sprintf("must be <= pauseresume.max_park_duration (%s) so a pause can't overstay its deadline by more than one sweep; got %s (0 = the documented %s default)",
					c.PauseResume.MaxParkDuration, c.PauseResume.SweepInterval, defaultPauseSweepInterval))
		}
	}
	return nil
}

// defaultPauseSweepInterval mirrors
// `pauseresume.DefaultSweepInterval` (internal/runtime/pauseresume/
// sweeper.go). Duplicated, not imported — `internal/config` MUST NOT
// depend on the runtime packages (AGENTS.md §4.4). Drift is caught by
// `TestRunSweeper_DefaultIntervalMirrorsValidator` in
// `internal/runtime/pauseresume/sweeper_test.go`.
const defaultPauseSweepInterval = time.Minute

// allowedArtifactsDrivers is the V1 artifacts-driver allowlist. Harbor
// ships `inmem` + `fs`; Harbor adds `sqlite` and `postgres`;
// Harbor adds the S3-style driver. The validator only checks
// shape; the registry surfaces the matching factory at Open time.
var allowedArtifactsDrivers = map[string]struct{}{
	"inmem":    {},
	"fs":       {},
	"sqlite":   {},
	"postgres": {},
	"s3":       {},
}

func (c *Config) validateArtifacts() error {
	if c.Artifacts.Driver == "" {
		return fieldError("artifacts.driver", "must not be empty")
	}
	if _, ok := allowedArtifactsDrivers[c.Artifacts.Driver]; !ok {
		return fieldError("artifacts.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedArtifactsDrivers), c.Artifacts.Driver))
	}
	if c.Artifacts.Driver == "fs" && c.Artifacts.FSRoot == "" {
		return fieldError("artifacts.fs_root",
			fmt.Sprintf("must be set when driver=%q", c.Artifacts.Driver))
	}
	if (c.Artifacts.Driver == "sqlite" || c.Artifacts.Driver == "postgres") && c.Artifacts.DSN == "" {
		return fieldError("artifacts.dsn",
			fmt.Sprintf("must be set when driver=%q", c.Artifacts.Driver))
	}
	if c.Artifacts.Driver == "s3" && c.Artifacts.S3Bucket == "" {
		return fieldError("artifacts.s3_bucket",
			fmt.Sprintf("must be set when driver=%q", c.Artifacts.Driver))
	}
	if c.Artifacts.HeavyOutputThresholdBytes < 0 {
		return fieldError("artifacts.heavy_output_threshold_bytes", "must be >= 0")
	}
	return nil
}

// allowedTasksDrivers is the tasks-driver allowlist. `inprocess` is
// the default (live state, no restart survival); `durable` persists
// task/group/patch records through the StateStore so they survive a
// runtime restart (pair it with a durable state.driver — sqlite or
// postgres — for cross-process survival).
var allowedTasksDrivers = map[string]struct{}{"inprocess": {}, "durable": {}}

func (c *Config) validateTasks() error {
	if c.Tasks.Driver == "" {
		return fieldError("tasks.driver", "must not be empty")
	}
	if _, ok := allowedTasksDrivers[c.Tasks.Driver]; !ok {
		return fieldError("tasks.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedTasksDrivers), c.Tasks.Driver))
	}
	// backgroundtasks-config knobs. Defaults are applied in
	// `Defaults()`; the validator rejects negative / zero values so an
	// operator-set override that elides the field flips back to the
	// default rather than silently disabling the feature.
	if c.Tasks.RetainTurnTimeout <= 0 {
		return fieldError("tasks.retain_turn_timeout", "must be > 0")
	}
	if c.Tasks.ContinuationHopLimit <= 0 {
		return fieldError("tasks.continuation_hop_limit", "must be > 0")
	}
	return nil
}

// allowedDistributedBusDrivers is the distributed bus driver
// allowlist. `loopback` is the default (in-process, no durability);
// `durable` persists every BusEnvelope through the StateStore and
// projects it onto the local event bus, with a poller for
// cross-instance / restart-replay delivery (StateStore-backed —
// Postgres-as-queue on a shared Postgres store). NATS / Redis Streams
// remain future drivers in the same post-V1 set.
var allowedDistributedBusDrivers = map[string]struct{}{"loopback": {}, "durable": {}}

// allowedDistributedRemoteDrivers is the V1 RemoteTransport driver
// allowlist. Harbor ships `loopback`; Harbor adds the `a2a` wire
// driver.
var allowedDistributedRemoteDrivers = map[string]struct{}{
	"loopback": {},
	"a2a":      {},
}

func (c *Config) validateDistributed() error {
	if c.Distributed.BusDriver == "" {
		return fieldError("distributed.bus_driver", "must not be empty")
	}
	if _, ok := allowedDistributedBusDrivers[c.Distributed.BusDriver]; !ok {
		return fieldError("distributed.bus_driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedDistributedBusDrivers), c.Distributed.BusDriver))
	}
	if c.Distributed.RemoteDriver == "" {
		return fieldError("distributed.remote_driver", "must not be empty")
	}
	if _, ok := allowedDistributedRemoteDrivers[c.Distributed.RemoteDriver]; !ok {
		return fieldError("distributed.remote_driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedDistributedRemoteDrivers), c.Distributed.RemoteDriver))
	}
	// bus_poll_interval is optional (the durable driver applies a default
	// when unset); a negative value is a misconfiguration, not "use the
	// default", so reject it rather than silently coercing.
	if c.Distributed.BusPollInterval < 0 {
		return fieldError("distributed.bus_poll_interval", "must be >= 0 (zero uses the driver default)")
	}
	return nil
}

// allowedMemoryDrivers is the V1 memory-driver allowlist. An earlier phase
// shipped `inmem`; Harbor adds `sqlite` and `postgres`.
var allowedMemoryDrivers = map[string]struct{}{
	"inmem":    {},
	"sqlite":   {},
	"postgres": {},
}

// memoryDriversRequiringDSN names the drivers whose `DSN` field must
// be non-empty. the persistent drivers need explicit DSNs;
// `inmem` does not.
var memoryDriversRequiringDSN = map[string]struct{}{
	"sqlite":   {},
	"postgres": {},
}

// allowedMemoryStrategies is the V1 memory-strategy allowlist.
// An earlier phase added `truncation` and `rolling_summary` alongside the
// `none`. The allowlist tracks the operational set so an
// operator-set unsupported strategy is rejected at config
// validation rather than later at memory.Open — fail fast.
var allowedMemoryStrategies = map[string]struct{}{
	"none":            {},
	"truncation":      {},
	"rolling_summary": {},
}

// allowedRetrievalModes is the retrieval-mode allowlist shared by
// the memory + skills blocks. Empty keeps each subsystem's default
// retrieval; "semantic" opts in to embedding-similarity retrieval
// (which the embeddings block must back — see validateEmbeddings).
var allowedRetrievalModes = map[string]struct{}{
	"":         {},
	"semantic": {},
}

// allowedEmbeddingsDrivers is the registered embeddings-driver
// allowlist. The production gateway driver is the only V1 entry;
// there is deliberately no mock/stub driver (AGENTS.md §13).
var allowedEmbeddingsDrivers = map[string]struct{}{
	"bifrost": {},
}

// validateEmbeddings validates the optional `embeddings:` block and
// the cross-block invariant that backs the semantic-retrieval
// modes: enabling `memory.retrieval: semantic` or
// `skills.retrieval: semantic` REQUIRES a configured embeddings
// block. The error names the missing key and points at the example
// config so the boot failure is actionable — a semantic mode never
// silently degrades to non-semantic retrieval (AGENTS.md §13).
func (c *Config) validateEmbeddings() error {
	semanticConsumers := make([]string, 0, 2)
	if c.Memory.Retrieval == "semantic" {
		semanticConsumers = append(semanticConsumers, "memory.retrieval")
	}
	if c.Skills.Retrieval == "semantic" {
		semanticConsumers = append(semanticConsumers, "skills.retrieval")
	}

	if c.Embeddings.IsZero() {
		if len(semanticConsumers) > 0 {
			return fieldError("embeddings",
				fmt.Sprintf("block is required when %s is %q — set embeddings.provider, embeddings.model, and embeddings.api_key (see examples/harbor.yaml)",
					strings.Join(semanticConsumers, " / "), "semantic"))
		}
		return nil
	}

	if c.Embeddings.Driver != "" {
		if _, ok := allowedEmbeddingsDrivers[c.Embeddings.Driver]; !ok {
			return fieldError("embeddings.driver",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedEmbeddingsDrivers), c.Embeddings.Driver))
		}
	}
	if c.Embeddings.Provider == "" {
		return fieldError("embeddings.provider", "must not be empty when the embeddings block is set")
	}
	if c.Embeddings.Model == "" {
		return fieldError("embeddings.model", "must not be empty when the embeddings block is set")
	}
	if c.Embeddings.APIKey == "" {
		return fieldError("embeddings.api_key",
			"must not be empty when the embeddings block is set (literal or env.NAME reference)")
	}
	if c.Embeddings.Timeout < 0 {
		return fieldError("embeddings.timeout", "must be >= 0")
	}
	if c.Embeddings.Dimensions < 0 {
		return fieldError("embeddings.dimensions", "must be >= 0")
	}
	return nil
}

func (c *Config) validateMemory() error {
	if c.Memory.Driver == "" {
		return fieldError("memory.driver", "must not be empty")
	}
	if _, ok := allowedMemoryDrivers[c.Memory.Driver]; !ok {
		return fieldError("memory.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedMemoryDrivers), c.Memory.Driver))
	}
	if _, needsDSN := memoryDriversRequiringDSN[c.Memory.Driver]; needsDSN {
		if c.Memory.DSN == "" {
			return fieldError("memory.dsn",
				fmt.Sprintf("must not be empty when driver=%q", c.Memory.Driver))
		}
	}
	if c.Memory.Strategy != "" {
		if _, ok := allowedMemoryStrategies[c.Memory.Strategy]; !ok {
			return fieldError("memory.strategy",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedMemoryStrategies), c.Memory.Strategy))
		}
	}
	if c.Memory.BudgetTokens < 0 {
		return fieldError("memory.budget_tokens", "must be >= 0")
	}
	if c.Memory.RecoveryBacklogMax < 0 {
		return fieldError("memory.recovery_backlog_max", "must be >= 0")
	}
	if _, ok := allowedRetrievalModes[c.Memory.Retrieval]; !ok {
		return fieldError("memory.retrieval",
			fmt.Sprintf("must be empty or %q, got %q", "semantic", c.Memory.Retrieval))
	}
	if c.Memory.RetrievalTopK < 0 {
		return fieldError("memory.retrieval_top_k", "must be >= 0")
	}
	if c.Memory.RetrievalMinScore < -1 || c.Memory.RetrievalMinScore > 1 {
		return fieldError("memory.retrieval_min_score",
			fmt.Sprintf("must be in [-1, 1], got %g", c.Memory.RetrievalMinScore))
	}
	return nil
}

// allowedSkillsDrivers is the V1 skills-driver allowlist. Harbor
// ships only `"localdb"`. A later phase will add `"portico"`.
var allowedSkillsDrivers = map[string]struct{}{
	"localdb": {},
}

// skillsDriversRequiringDSN names the drivers whose `DSN` field must
// be supplied. Mirrors `memoryDriversRequiringDSN`.
var skillsDriversRequiringDSN = map[string]struct{}{
	"localdb": {},
}

// validateSkills validates the optional `skills:` block. The block
// is fully optional at the config layer — an empty SkillsConfig
// passes silently and the skills subsystem stays uninitialised. The
// runtime wiring decides whether `skills.Open` is called; that path
// fails loudly on its own when handed an empty DSN.
//
// When the operator HAS supplied any skills field, the validator
// enforces driver-allowlist + driver-requires-DSN.
func (c *Config) validateSkills() error {
	// Directory shape-validation runs unconditionally so a typo'd
	// `skills.directory` block fails at load time even when the
	// parent store fields are empty.
	if err := c.validateSkillsDirectory(); err != nil {
		return err
	}
	// Retrieval-mode shape-validation runs unconditionally so a
	// typo'd `skills.retrieval` fails at load time too.
	if _, ok := allowedRetrievalModes[c.Skills.Retrieval]; !ok {
		return fieldError("skills.retrieval",
			fmt.Sprintf("must be empty or %q, got %q", "semantic", c.Skills.Retrieval))
	}
	if c.Skills.Driver == "" && c.Skills.DSN == "" {
		if len(c.Skills.Directory.Pinned) > 0 ||
			c.Skills.Directory.MaxEntries != 0 ||
			c.Skills.Directory.Selection != "" {
			return fieldError("skills.driver",
				"must not be empty when skills.directory is set (the directory browses the configured store)")
		}
		if c.Skills.Retrieval != "" {
			return fieldError("skills.driver",
				"must not be empty when skills.retrieval is set (the retrieval mode shapes the configured store's search)")
		}
		return nil
	}
	if c.Skills.Driver == "" {
		return fieldError("skills.driver",
			"must not be empty when any skills field is set")
	}
	if _, ok := allowedSkillsDrivers[c.Skills.Driver]; !ok {
		return fieldError("skills.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedSkillsDrivers), c.Skills.Driver))
	}
	if _, needsDSN := skillsDriversRequiringDSN[c.Skills.Driver]; needsDSN {
		if c.Skills.DSN == "" {
			return fieldError("skills.dsn",
				fmt.Sprintf("must not be empty when driver=%q (use \":memory:\" for ephemeral)",
					c.Skills.Driver))
		}
	}
	return nil
}

// allowedSkillsDirectorySelections mirrors the `skills.Selection`
// canonical values (`internal/skills/directory.go`). Duplicated, not
// imported — `internal/config` MUST NOT depend on the skills package
// (AGENTS.md §4.4). Drift is caught by
// `TestDirectoryFromConfig_SelectionAllowlistMirrorsValidator` in
// `internal/skills/from_config_test.go`.
var allowedSkillsDirectorySelections = map[string]struct{}{
	"pinned_then_recent": {},
	"pinned_then_top":    {},
}

// validateSkillsDirectory validates the optional `skills.directory`
// block. Runs even when the parent skills block
// is empty so a typo'd directory block under a not-yet-configured
// store still fails at load time rather than silently no-oping.
func (c *Config) validateSkillsDirectory() error {
	d := c.Skills.Directory
	if d.MaxEntries != 0 && (d.MaxEntries < 1 || d.MaxEntries > 200) {
		return fieldError("skills.directory.max_entries",
			fmt.Sprintf("must be 0 (default) or in [1, 200], got %d", d.MaxEntries))
	}
	if d.Selection != "" {
		if _, ok := allowedSkillsDirectorySelections[d.Selection]; !ok {
			return fieldError("skills.directory.selection",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedSkillsDirectorySelections), d.Selection))
		}
		// checkpoint audit (addendum): nothing in
		// production increments UseCount yet, so `pinned_then_top`
		// would validate cleanly and then silently degrade to
		// alphabetical ordering (every counter is 0) — the §13
		// no-silent-degradation posture says fail loud instead (the
		// tools.http_manifests precedent). Remove this guard when a
		// production usage-bump path lands.
		if d.Selection == "pinned_then_top" {
			return fieldError("skills.directory.selection",
				"pinned_then_top is not wired yet — no production path increments skill "+
					"usage counters, so the ordering would silently degrade to alphabetical; "+
					"use pinned_then_recent (or omit the field) until usage tracking lands")
		}
	}
	seen := make(map[string]struct{}, len(d.Pinned))
	for i, name := range d.Pinned {
		prefix := fmt.Sprintf("skills.directory.pinned[%d]", i)
		if name == "" {
			return fieldError(prefix, "must not be empty")
		}
		if _, dup := seen[name]; dup {
			return fieldError(prefix,
				fmt.Sprintf("duplicate pinned skill %q (names must be unique)", name))
		}
		seen[name] = struct{}{}
	}
	return nil
}

// allowedMCPTransportModes mirrors the MCPTransportMode allowlist
// in `internal/tools/drivers/mcp/auto.go`. Duplicated (not imported)
// because `internal/config` MUST NOT depend on a concrete driver
// package (AGENTS.md §4.4 — drivers depend on interfaces, not the
// other way round). A drift between the two lists is caught by
// `TestValidateTools_TransportModeAllowlistMirrors_MCPDriver` in
// `internal/tools/drivers/mcp/mcp_test.go`.
var allowedMCPTransportModes = map[string]struct{}{
	"auto":            {},
	"sse":             {},
	"streamable_http": {},
	"stdio":           {},
}

// allowedMCPAppDisplayModes mirrors the closed display-mode set
// (`validDisplayModes`) in `internal/tools/drivers/mcp/mcp.go`. Duplicated
// (not imported) for the same reason as allowedMCPTransportModes:
// `internal/config` MUST NOT depend on a concrete driver package
// (AGENTS.md §4.4). A drift between the two lists is caught by
// `TestValidateTools_MCPAppDisplayModeAllowlistMirrors_MCPDriver` in
// `internal/tools/drivers/mcp/mcp_test.go`.
var allowedMCPAppDisplayModes = map[string]struct{}{
	"inline":     {},
	"fullscreen": {},
	"pip":        {},
}

// validateTools checks the tools configuration: the
// HTTP manifest paths + the MCP servers. Later phases extend
// (A2A peers, OAuth token stores, etc.). The
// manifest itself is parsed by `internal/tools/drivers/http` at
// boot; this validator only enforces structural shape so a typo
// (empty list entry, trailing comma in YAML) fails at config load
// rather than during driver registration.
//
// Per-MCP-server invariants:
//   - Name non-empty + unique across servers.
//   - TransportMode in the allowlist (empty defaults to "auto" at
//     driver-construction time; the validator accepts empty).
//   - URL set when transport is sse / streamable_http.
//   - Command set when transport is stdio.
//   - KeepAlive >= 0.
//
// Auto-mode + empty URL + empty Command is rejected (no candidate
// transport would be selected).
func (c *Config) validateTools() error {
	// built-in tools opt-in via name. Each entry
	// must be in the mirror allowlist; a typo fails loudly with the
	// known set in the error message.
	seenBuiltIn := make(map[string]struct{}, len(c.Tools.BuiltIn))
	for i, name := range c.Tools.BuiltIn {
		prefix := fmt.Sprintf("tools.built_in[%d]", i)
		if name == "" {
			return fieldError(prefix, "must not be empty")
		}
		if _, dup := seenBuiltIn[name]; dup {
			return fieldError(prefix,
				fmt.Sprintf("duplicate built-in tool %q (must be unique within tools.built_in)", name))
		}
		seenBuiltIn[name] = struct{}{}
		if _, ok := allowedBuiltInTools[name]; !ok {
			return fieldError(prefix,
				fmt.Sprintf("unknown built-in tool %q (known: %s)",
					name, sortedKeys(allowedBuiltInTools)))
		}
	}
	// operator-declared custom tools (the
	// scaffold reads these). Each entry's name must be non-empty,
	// unique within the slice, and not collide with `tools.built_in`.
	// Each input/output field's type must be in the V1.1 yaml-
	// shorthand allowlist. Empty `tools.custom` is valid.
	seenCustom := make(map[string]struct{}, len(c.Tools.Custom))
	for i, ct := range c.Tools.Custom {
		prefix := fmt.Sprintf("tools.custom[%d]", i)
		if ct.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := seenCustom[ct.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate custom tool %q (must be unique within tools.custom)", ct.Name))
		}
		seenCustom[ct.Name] = struct{}{}
		if _, builtIn := seenBuiltIn[ct.Name]; builtIn {
			return fieldError(prefix+".name",
				fmt.Sprintf("collides with tools.built_in entry %q (custom tool names must not shadow built-ins)", ct.Name))
		}
		if ct.Description == "" {
			return fieldError(prefix+".description",
				"must not be empty (the description surfaces in the planner-facing tool catalog and the generated Go comment)")
		}
		for field, ftype := range ct.Input {
			if field == "" {
				return fieldError(prefix+".input", "input field names must not be empty")
			}
			if _, ok := allowedCustomToolTypes[ftype]; !ok {
				return fieldError(
					fmt.Sprintf("%s.input[%q]", prefix, field),
					fmt.Sprintf("unknown type %q (known: %s)", ftype, sortedKeys(allowedCustomToolTypes)))
			}
		}
		for field, ftype := range ct.Output {
			if field == "" {
				return fieldError(prefix+".output", "output field names must not be empty")
			}
			if _, ok := allowedCustomToolTypes[ftype]; !ok {
				return fieldError(
					fmt.Sprintf("%s.output[%q]", prefix, field),
					fmt.Sprintf("unknown type %q (known: %s)", ftype, sortedKeys(allowedCustomToolTypes)))
			}
		}
	}
	// SDK friction audit (docs/notes/sdk-friction-audit.md §1): the
	// manifest loader (`LoadManifest` / `RegisterManifest`)
	// has no boot-path consumer yet — a populated list would validate
	// cleanly and then silently register nothing (§13 — no silent
	// degradation). Fail loud until the boot wiring lands. An empty
	// list stays valid (the shipped examples carry `http_manifests: []`).
	if len(c.Tools.HTTPManifests) > 0 {
		return fieldError("tools.http_manifests",
			"declared manifests are not loaded at boot yet — the surface is not wired "+
				"(see docs/notes/sdk-friction-audit.md §1); remove the entries until the "+
				"boot loader lands, or register HTTP tools programmatically via the HTTP tool driver")
	}
	// operator-declared granted scopes
	// pass-through. The validator asserts only that each entry is a
	// non-empty string; scope names are operator-defined per their
	// tool sources (no allowlist). An empty list is valid (the
	// existing latent default — no scopes granted, tools with
	// AuthScopes are invisible to the planner).
	for i, s := range c.Tools.GrantedScopes {
		if strings.TrimSpace(s) == "" {
			return fieldError(fmt.Sprintf("tools.granted_scopes[%d]", i),
				"must not be empty (each granted scope is a non-empty operator-defined string)")
		}
	}
	names := make(map[string]struct{})
	for i, s := range c.Tools.MCPServers {
		prefix := fmt.Sprintf("tools.mcp_servers[%d]", i)
		if s.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := names[s.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate name %q (must be unique)", s.Name))
		}
		names[s.Name] = struct{}{}
		mode := s.TransportMode
		if mode == "" {
			mode = "auto"
		}
		if _, ok := allowedMCPTransportModes[mode]; !ok {
			return fieldError(prefix+".transport_mode",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedMCPTransportModes), s.TransportMode))
		}
		switch mode {
		case "sse", "streamable_http":
			if s.URL == "" {
				return fieldError(prefix+".url",
					fmt.Sprintf("must be set when transport_mode=%q", mode))
			}
		case "stdio":
			if len(s.Command) == 0 {
				return fieldError(prefix+".command",
					"must be set (argv form) when transport_mode=\"stdio\"")
			}
			if s.Command[0] == "" {
				return fieldError(prefix+".command[0]",
					"binary path must not be empty")
			}
		case "auto":
			if s.URL == "" && len(s.Command) == 0 {
				return fieldError(prefix,
					"auto mode requires url or command")
			}
		}
		if s.KeepAlive < 0 {
			return fieldError(prefix+".keep_alive", "must be >= 0")
		}
		// per-server default tool policy + per-tool
		// overrides. Both optional; omitting them preserves today's
		// behaviour (every tool inherits tools.DefaultPolicy()).
		if s.Policy != nil {
			if err := validateToolPolicy(prefix+".policy", *s.Policy); err != nil {
				return err
			}
		}
		toolPolicyNames := make(map[string]struct{}, len(s.ToolPolicies))
		for toolName, tp := range s.ToolPolicies {
			if strings.TrimSpace(toolName) == "" {
				return fieldError(prefix+".tool_policies",
					"override key (tool name) must not be empty")
			}
			if _, dup := toolPolicyNames[toolName]; dup {
				// Go maps cannot carry duplicate keys, so this guards a
				// future shape change; kept for parity with the unique
				// constraint stated in the phase plan.
				return fieldError(prefix+".tool_policies",
					fmt.Sprintf("duplicate override key %q (must be unique)", toolName))
			}
			toolPolicyNames[toolName] = struct{}{}
			if err := validateToolPolicy(
				fmt.Sprintf("%s.tool_policies[%q]", prefix, toolName), tp); err != nil {
				return err
			}
		}
	}
	// MCP App host capability. Optional; nil resolves to the inline-only
	// baseline. When set, each declared display mode must be in the closed
	// set (inline / fullscreen / pip) and unique — a typo fails at config
	// load rather than silently advertising an unrenderable mode.
	if c.Tools.MCPAppHost != nil {
		seenMode := make(map[string]struct{}, len(c.Tools.MCPAppHost.DisplayModes))
		for i, mode := range c.Tools.MCPAppHost.DisplayModes {
			field := fmt.Sprintf("tools.mcp_app_host.display_modes[%d]", i)
			if _, ok := allowedMCPAppDisplayModes[mode]; !ok {
				return fieldError(field,
					fmt.Sprintf("must be one of %s, got %q",
						sortedKeys(allowedMCPAppDisplayModes), mode))
			}
			if _, dup := seenMode[mode]; dup {
				return fieldError(field,
					fmt.Sprintf("duplicate display mode %q (must be unique)", mode))
			}
			seenMode[mode] = struct{}{}
		}
	}
	// A2A peers. Empty list is valid. Each entry must
	// declare a non-empty URL, a TrustTier in [1, 5], a non-negative
	// LatencyTierMS, and a non-negative AgentCardTTL. URL scheme
	// enforcement (HTTPS-only by default) is deferred to the driver —
	// validateTools accepts any non-empty string so test fixtures
	// using `http://localhost` round-trip; the driver applies the
	// loopback / allowlist rule at construction.
	for i, p := range c.Tools.A2APeers {
		if p.URL == "" {
			return fieldError(fmt.Sprintf("tools.a2a_peers[%d].url", i), "must not be empty")
		}
		if p.TrustTier < 1 || p.TrustTier > 5 {
			return fieldError(fmt.Sprintf("tools.a2a_peers[%d].trust_tier", i),
				fmt.Sprintf("must be in [1,5], got %d", p.TrustTier))
		}
		if p.LatencyTierMS < 0 {
			return fieldError(fmt.Sprintf("tools.a2a_peers[%d].latency_tier_ms", i),
				fmt.Sprintf("must be >= 0, got %d", p.LatencyTierMS))
		}
		if p.AgentCardTTL < 0 {
			return fieldError(fmt.Sprintf("tools.a2a_peers[%d].agent_card_ttl", i),
				"must be >= 0")
		}
	}
	// `tools.oauth_providers[]` operator-config block (closes
	// issue #116 — the deferred construction gap). Empty list is
	// valid (no OAuth-bound entries → no providers needed). When the
	// list is non-empty:
	//   - every Name is unique within the slice;
	//   - Driver / ClientIDEnv / ClientSecretEnv are non-empty (the
	//     driver registry resolves Driver at boot; ClientIDEnv /
	//     ClientSecretEnv name the env vars the driver reads via
	//     os.Getenv at construction time per §7 rule 2);
	//   - Driver must be in the bundled driver allowlist. An unknown
	//     driver fails validate (rather than boot) so an operator
	//     typoing the driver name gets a clear pre-boot error.
	// Operators who declare any `tools.oauth_providers[]` entry MUST
	// also set `tools.oauth_token_kek_env`; the dev stack constructs a
	// single AES-256-GCM Sealer over the named env var (§7).
	oauthProviderNames := make(map[string]struct{}, len(c.Tools.OAuthProviders))
	for i, p := range c.Tools.OAuthProviders {
		prefix := fmt.Sprintf("tools.oauth_providers[%d]", i)
		if p.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := oauthProviderNames[p.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate provider name %q (must be unique within tools.oauth_providers[])", p.Name))
		}
		oauthProviderNames[p.Name] = struct{}{}
		if p.Driver == "" {
			return fieldError(prefix+".driver", "must not be empty")
		}
		if _, ok := allowedOAuthDrivers[p.Driver]; !ok {
			return fieldError(prefix+".driver",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedOAuthDrivers), p.Driver))
		}
		if p.ClientIDEnv == "" {
			return fieldError(prefix+".client_id_env",
				"must not be empty (env var name holding the client_id; §7 rule 2 — never hardcoded)")
		}
		if p.ClientSecretEnv == "" {
			return fieldError(prefix+".client_secret_env",
				"must not be empty (env var name holding the client_secret; §7 rule 2 — never hardcoded)")
		}
	}
	if len(c.Tools.OAuthProviders) > 0 && c.Tools.OAuthTokenKEKEnv == "" {
		return fieldError("tools.oauth_token_kek_env",
			"must not be empty when tools.oauth_providers[] is set (names env var holding the 32-byte hex KEK for AES-256-GCM token encryption at rest; §7 rule 2)")
	}

	// catalog wiring entries. Empty list is valid;
	// duplicate names are rejected; an entry whose Approval AND OAuth
	// are both nil is a configuration typo (nothing to wire) and is
	// rejected with a clear error. Policy / binding-scope strings are
	// checked against the canonical allowlists so a typo fails at
	// `harbor validate` time instead of at `harbor dev` boot.
	//
	// cross-validation: every `entries[].oauth.provider` value
	// MUST resolve to a `tools.oauth_providers[].name` declared above.
	// An unresolved reference fails loud with both the entry and the
	// unknown provider name in the error message.
	seenEntries := make(map[string]struct{})
	for i, e := range c.Tools.Entries {
		prefix := fmt.Sprintf("tools.entries[%d]", i)
		if e.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := seenEntries[e.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate entry for tool %q (must be unique)", e.Name))
		}
		seenEntries[e.Name] = struct{}{}
		// `loading_mode` is the third configurable
		// surface on a `tools.entries[]` row (alongside `approval` and
		// `oauth`). Valid values: "" (use registrar default), "always",
		// "deferred". Unknown values fail loud pre-boot per CLAUDE.md
		// §13 (a silent default would hide an operator typo).
		switch e.LoadingMode {
		case "", "always", "deferred":
		default:
			return fieldError(prefix+".loading_mode",
				fmt.Sprintf("must be one of [\"always\", \"deferred\"] or empty, got %q", e.LoadingMode))
		}
		if e.Approval == nil && e.OAuth == nil && e.LoadingMode == "" {
			return fieldError(prefix,
				"at least one of `approval`, `oauth`, or `loading_mode` must be set (an entry with no fields is a configuration typo)")
		}
		if e.Approval != nil {
			if _, ok := allowedApprovalPolicies[e.Approval.Policy]; !ok {
				return fieldError(prefix+".approval.policy",
					fmt.Sprintf("must be one of %s, got %q",
						sortedKeys(allowedApprovalPolicies), e.Approval.Policy))
			}
			if e.Approval.Policy == "tagged" && len(e.Approval.RequireTags) == 0 {
				return fieldError(prefix+".approval.require_tags",
					"must be set when policy=\"tagged\" (a tagged policy with no tags would never trigger)")
			}
		}
		if e.OAuth != nil {
			if e.OAuth.Provider == "" {
				return fieldError(prefix+".oauth.provider", "must not be empty")
			}
			if _, ok := allowedOAuthBindingScopes[e.OAuth.BindingScope]; !ok {
				return fieldError(prefix+".oauth.binding_scope",
					fmt.Sprintf("must be one of %s, got %q",
						sortedKeys(allowedOAuthBindingScopes), e.OAuth.BindingScope))
			}
			// cross-validation — entry's provider name MUST
			// resolve to a configured tools.oauth_providers[].name.
			if _, ok := oauthProviderNames[e.OAuth.Provider]; !ok {
				return fieldError(prefix+".oauth.provider",
					fmt.Sprintf("references unknown OAuth provider %q (declared providers: %s; declare via tools.oauth_providers[])",
						e.OAuth.Provider, sortedKeysFromSet(oauthProviderNames)))
			}
		}
	}
	return nil
}

// allowedCustomToolTypes is the V1.1 yaml-shorthand type allowlist for
// `tools.custom[].input` / `.output` entries. Each
// value maps to a Go primitive at scaffold time:
//
//	string   → string
//	integer  → int
//	number   → float64
//	boolean  → bool
//	[]string → []string
//
// V1.1 keeps the surface flat — nested objects + arrays of objects are
// not supported through the yaml shorthand. Operators with complex
// shapes register tools by hand via `inproc.RegisterFunc`, where the
// schema deriver already handles arbitrary Go types.
var allowedCustomToolTypes = map[string]struct{}{
	"string":   {},
	"integer":  {},
	"number":   {},
	"boolean":  {},
	"[]string": {},
}

// KnownCustomToolTypes returns the sorted allowlist of yaml-shorthand
// types `tools.custom[]` accepts. Public so the scaffold engine + a
// future drift test can read the same source of truth.
func KnownCustomToolTypes() []string {
	out := make([]string, 0, len(allowedCustomToolTypes))
	for t := range allowedCustomToolTypes {
		out = append(out, t)
	}
	sortStringSlice(out)
	return out
}

// allowedBuiltInTools mirrors `internal/tools/builtin.KnownNames()`.
// Same duplication rationale as `allowedApprovalPolicies` — the
// `internal/config` package MUST NOT import a concrete tool-side
// package (CLAUDE.md §4.4). The mirror is asserted by the
// `TestKnownNames_MirrorsConfigAllowlist` test in
// `internal/tools/builtin/builtin_test.go`. New built-ins land here
// in the same PR as their addition to the registry.
var allowedBuiltInTools = map[string]struct{}{
	"clock.now": {},
	"text.echo": {},
	// meta-tools for discovery + escape-hatch.
	"tool_search":        {},
	"tool_get":           {},
	"skill_search":       {},
	"skill_get":          {},
	"declarative_action": {},
	"artifact_fetch":     {},
	// the canonical skills surface. `skill_list`
	// joins the discovery set; `skill_propose` (persistence-capable
	// generator) is a deliberate operator opt-in, absent from every
	// recommended default.
	"skill_list":    {},
	"skill_propose": {},
}

// KnownBuiltInTools returns the sorted built-in allowlist as a slice.
// Public so the `internal/tools/builtin` mirror test can reach it
// without importing internal validator state.
func KnownBuiltInTools() []string {
	out := make([]string, 0, len(allowedBuiltInTools))
	for name := range allowedBuiltInTools {
		out = append(out, name)
	}
	sortStringSlice(out)
	return out
}

// sortStringSlice keeps the package free of an `sort` import for one
// trivial use. Bubble sort is fine for ≤ 32 entries.
func sortStringSlice(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// allowedApprovalPolicies mirrors the bundled `approval.ApprovalPolicy`
// implementations. Duplicated here (not imported)
// because `internal/config` MUST NOT depend on a concrete tool-side
// package (CLAUDE.md §4.4 — drivers depend on interfaces, not the
// other way round). Drift between the two surfaces is caught by
// `TestValidateTools_PolicyAllowlistMirrors_ApprovalPackage` in
// `internal/tools/catalog`.
var allowedApprovalPolicies = map[string]struct{}{
	"deny-all":    {},
	"approve-all": {},
	"tagged":      {},
}

// allowedOAuthBindingScopes mirrors `auth.BindingScope`
// Same duplication rationale as `allowedApprovalPolicies`.
// Drift caught by `TestValidateTools_BindingScopeAllowlistMirrors_AuthPackage`.
var allowedOAuthBindingScopes = map[string]struct{}{
	"user":  {},
	"agent": {},
}

// allowedOAuthDrivers mirrors the `internal/tools/auth` driver
// registry. V1 ships only the `oauth2` driver (generic OAuth2/
// PKCE Authorization Code flow). New drivers under
// `internal/tools/auth/drivers/<name>/` add a row here in the same PR.
// Same duplication rationale as `allowedApprovalPolicies` — the
// `internal/config` package MUST NOT import a concrete driver package
// (§4.4 — drivers depend on interfaces, not the other way round). The
// auth-package test `TestRegisteredDriversMatchConfigAllowlist`
// asserts no drift between the two surfaces.
var allowedOAuthDrivers = map[string]struct{}{
	"oauth2": {},
}

// allowedPlannerDrivers mirrors the `internal/planner` driver registry
// (closes issue #126). V1 ships only the `react` driver (the
// reference LLM-driven ReAct planner —). New drivers
// under `internal/planner/<name>/` add a row here in the same PR. Same
// duplication rationale as `allowedOAuthDrivers` — the `internal/config`
// package MUST NOT import a concrete driver package (§4.4 — drivers
// depend on interfaces, not the other way round). The planner-package
// test `TestConfigAllowlist_AcceptsReactDriver + TestConfigAllowlist_RejectsUnknownDriver` asserts no
// drift between the two surfaces.
var allowedPlannerDrivers = map[string]struct{}{
	"react": {},
}

// validatePlanner checks the planner-config block. Empty Driver
// defaults to "react" (the V1 reference planner — see PlannerConfig
// godoc). Unknown driver names fail loud pre-boot with the registered
// allowlist in the error message; negative MaxSteps is rejected.
//
// The allowlist mirror is intentional — `internal/config` MUST NOT
// import a concrete driver package (§4.4). A drift between the two
// surfaces is caught by `TestConfigAllowlist_AcceptsReactDriver + TestConfigAllowlist_RejectsUnknownDriver`
// in `internal/planner`.
func (c *Config) validatePlanner() error {
	driver := c.Planner.Driver
	if driver == "" {
		driver = "react"
	}
	if _, ok := allowedPlannerDrivers[driver]; !ok {
		return fieldError("planner.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedPlannerDrivers), c.Planner.Driver))
	}
	if c.Planner.MaxSteps < 0 {
		return fieldError("planner.max_steps",
			fmt.Sprintf("must be >= 0 (0 = use driver default), got %d", c.Planner.MaxSteps))
	}
	if _, ok := allowedReasoningReplayModes[c.Planner.ReasoningReplay]; !ok {
		return fieldError("planner.reasoning_replay",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedReasoningReplayModes), c.Planner.ReasoningReplay))
	}
	if c.Planner.MaxToolExamplesPerTool < 0 {
		return fieldError("planner.max_tool_examples_per_tool",
			fmt.Sprintf("must be >= 0 (0 = use driver default of 3), got %d",
				c.Planner.MaxToolExamplesPerTool))
	}
	if c.Planner.SkillsContextMax < 0 {
		return fieldError("planner.skills_context_max",
			fmt.Sprintf("must be >= 0 (0 = use dev-runtime default of 5), got %d",
				c.Planner.SkillsContextMax))
	}
	if c.Planner.AbsoluteMaxSpawnDepth < 0 {
		return fieldError("planner.absolute_max_spawn_depth",
			fmt.Sprintf("must be >= 0 (0 = use dev-runtime default of 4), got %d",
				c.Planner.AbsoluteMaxSpawnDepth))
	}
	if c.Planner.TokenBudget < 0 {
		return fieldError("planner.token_budget",
			fmt.Sprintf("must be >= 0 (0 = trajectory compression disabled), got %d",
				c.Planner.TokenBudget))
	}
	return nil
}

// validateMultimodal validates the attachment
// disposition block. Keys must be `*`, a family wildcard (`type/*`),
// or an exact `type/subtype` media type; values must satisfy the
// disposition grammar (`ref` / `inline` / `provider_native` /
// `tool:<name>`). The grammar locally mirrors
// `planner.ParseDisposition` — `internal/config` MUST NOT import
// `internal/planner` (the import direction; the
// allowlist-mirror pattern, same as `allowedPlannerDrivers`); the
// planner package's `TestParseDisposition_ConfigGrammarLockstep`
// asserts no drift.
func (c *Config) validateMultimodal() error {
	for key, value := range c.Multimodal.Disposition {
		if !validDispositionMIMEKey(key) {
			return fieldError("multimodal.disposition",
				fmt.Sprintf("key %q must be \"*\", a family wildcard (\"image/*\"), or an exact media type (\"application/pdf\")", key))
		}
		if !validDispositionValue(value) {
			return fieldError("multimodal.disposition",
				fmt.Sprintf("value %q for key %q must be one of ref | inline | provider_native | tool:<name>", value, key))
		}
	}
	return nil
}

// validDispositionMIMEKey reports whether key is a legal
// `multimodal.disposition` map key: the literal `*`, a `type/*`
// family wildcard, or an exact `type/subtype` media type.
func validDispositionMIMEKey(key string) bool {
	if key == "*" {
		return true
	}
	typ, sub, found := strings.Cut(key, "/")
	if !found || typ == "" || sub == "" {
		return false
	}
	return !strings.Contains(sub, "/")
}

// validDispositionValue locally mirrors the planner disposition
// grammar (see validateMultimodal's doc for the lockstep rationale).
func validDispositionValue(value string) bool {
	switch value {
	case "ref", "inline", "provider_native":
		return true
	}
	name, found := strings.CutPrefix(value, "tool:")
	return found && name != ""
}

// allowedReasoningReplayModes mirrors the `planner.ReasoningReplayMode`
// enum. The empty string is accepted — it is the
// unset sentinel that the planner resolves to `never`. `internal/config`
// MUST NOT import `internal/planner` (the allowlist-mirror pattern, same
// as `allowedPlannerDrivers`); the planner package's
// `TestConfigAllowlist_ReasoningReplayMirror` test asserts no drift.
var allowedReasoningReplayModes = map[string]struct{}{
	"":      {},
	"never": {},
	"text":  {},
}

// fieldError formats a validation error with the offending path so
// the operator can grep for the key in their YAML.
func fieldError(path, reason string) error {
	return fmt.Errorf("config.%s: %s", path, reason)
}

// validateToolPolicy validates one `ToolPolicyConfig` block (
// ). `prefix` is the field path so the error names the offending
// key (e.g. `tools.mcp_servers[0].policy`). Rules:
//   - `max_attempts >= 0`. The TOTAL attempt count incl. the first.
//     0 (the Go zero value = the operator omitted the key) means
//     "inherit the default attempt count" — the per-field fall-through
//     documents (a `policy:` that sets only `timeout_ms` keeps
//     the default 4 attempts). YAML cannot distinguish "absent" from
//     "0" for a plain int, and the projection treats both as
//     fall-through, so validation matches that: only a NEGATIVE value
//     is an error. (1 = exactly one attempt, no retry.)
//   - `timeout_ms >= 0`.
//   - `backoff_base_ms` / `backoff_max_ms` / `backoff_mult` >= 0.
//   - each `retry_on` value is a known error class.
func validateToolPolicy(prefix string, p ToolPolicyConfig) error {
	if p.MaxAttempts < 0 {
		return fieldError(prefix+".max_attempts",
			fmt.Sprintf("must be >= 0 (TOTAL attempts incl. the first; 0 = inherit the default), got %d", p.MaxAttempts))
	}
	if p.TimeoutMS < 0 {
		return fieldError(prefix+".timeout_ms",
			fmt.Sprintf("must be >= 0, got %d", p.TimeoutMS))
	}
	if p.BackoffBaseMS < 0 {
		return fieldError(prefix+".backoff_base_ms",
			fmt.Sprintf("must be >= 0, got %d", p.BackoffBaseMS))
	}
	if p.BackoffMaxMS < 0 {
		return fieldError(prefix+".backoff_max_ms",
			fmt.Sprintf("must be >= 0, got %d", p.BackoffMaxMS))
	}
	if p.BackoffMult < 0 {
		return fieldError(prefix+".backoff_mult",
			fmt.Sprintf("must be >= 0, got %v", p.BackoffMult))
	}
	for i, class := range p.RetryOn {
		if _, ok := validToolPolicyErrorClasses[class]; !ok {
			return fieldError(fmt.Sprintf("%s.retry_on[%d]", prefix, i),
				fmt.Sprintf("unknown error class %q (allowed: 5xx, permanent, timeout, transient)", class))
		}
	}
	return nil
}

// IsValidationError reports whether err originated in validation
// (vs. a parse or env-override failure). Callers who want to
// distinguish boot-time misconfiguration from filesystem trouble
// can errors.Is on ErrConfigInvalid first, then this helper.
func IsValidationError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrConfigInvalid) && strings.Contains(err.Error(), "config.")
}

// nativeBifrostProviders mirrors `bfschemas.StandardProviders` (the
// `github.com/maximhq/bifrost/core/schemas` v1.5.8 native-provider
// list). The mirror lives here so the config package stays decoupled
// from the bifrost SDK — when a new native bifrost provider is added
// in a future bifrost release, this list updates in lockstep via the
// next phase plan. only widens this surface; future phases
// may consult bifrost directly if the decoupling proves costly.
var nativeBifrostProviders = map[string]struct{}{
	"openai":      {},
	"azure":       {},
	"anthropic":   {},
	"bedrock":     {},
	"cohere":      {},
	"vertex":      {},
	"mistral":     {},
	"ollama":      {},
	"groq":        {},
	"sgl":         {},
	"parasail":    {},
	"perplexity":  {},
	"cerebras":    {},
	"gemini":      {},
	"openrouter":  {},
	"elevenlabs":  {},
	"huggingface": {},
	"nebius":      {},
	"xai":         {},
	"replicate":   {},
	"vllm":        {},
	"runway":      {},
	"fireworks":   {},
}

// allowedCustomBaseProviderTypes is the wire-protocol family allowlist
// for `LLMCustomProviderConfig.BaseProviderType`. The
// empty string defaults to `"openai"` in the driver; both are valid
// here. Future phases widen.
var allowedCustomBaseProviderTypes = map[string]struct{}{
	"":       {},
	"openai": {},
}

// validateLLMCustomProviders validates the operator-declared custom
// provider list and returns the set of declared names so
// the legacy single-provider validator can decide whether the
// configured `llm.provider` resolves to a custom or native entry.
//
// `driver` is the resolved driver name (mock/bifrost) — `mock` skips
// validation since it doesn't consume the custom-provider list.
func (c *Config) validateLLMCustomProviders(driver string) (map[string]struct{}, error) {
	names := make(map[string]struct{}, len(c.LLM.CustomProviders))
	if driver == "mock" || len(c.LLM.CustomProviders) == 0 {
		return names, nil
	}
	for i, cp := range c.LLM.CustomProviders {
		fieldPath := fmt.Sprintf("llm.custom_providers[%d]", i)
		if cp.Name == "" {
			return nil, fieldError(fieldPath+".name", "must not be empty")
		}
		if cp.BaseURL == "" {
			return nil, fieldError(fieldPath+".base_url",
				fmt.Sprintf("must not be empty (custom provider %q)", cp.Name))
		}
		if cp.APIKeyEnvVar == "" {
			return nil, fieldError(fieldPath+".api_key_env_var",
				fmt.Sprintf("must not be empty (custom provider %q)", cp.Name))
		}
		if strings.HasPrefix(cp.APIKeyEnvVar, "env.") {
			return nil, fieldError(fieldPath+".api_key_env_var",
				fmt.Sprintf("write the env var NAME without the %q prefix (custom provider %q) — the driver resolves os.Getenv(name) at construction", "env.", cp.Name))
		}
		if len(cp.Models) == 0 {
			return nil, fieldError(fieldPath+".models",
				fmt.Sprintf("must contain at least one model (custom provider %q)", cp.Name))
		}
		for j, m := range cp.Models {
			if m == "" {
				return nil, fieldError(fmt.Sprintf("%s.models[%d]", fieldPath, j),
					fmt.Sprintf("must not be empty (custom provider %q)", cp.Name))
			}
		}
		if _, ok := allowedCustomBaseProviderTypes[cp.BaseProviderType]; !ok {
			return nil, fieldError(fieldPath+".base_provider_type",
				fmt.Sprintf("must be one of %s, got %q (custom provider %q)",
					sortedKeys(allowedCustomBaseProviderTypes), cp.BaseProviderType, cp.Name))
		}
		if _, exists := names[cp.Name]; exists {
			return nil, fieldError(fieldPath+".name",
				fmt.Sprintf("duplicate custom provider name %q", cp.Name))
		}
		if _, native := nativeBifrostProviders[cp.Name]; native {
			return nil, fieldError(fieldPath+".name",
				fmt.Sprintf("custom provider name %q collides with a native bifrost provider; pick a different name", cp.Name))
		}
		if cp.Timeout < 0 {
			return nil, fieldError(fieldPath+".timeout",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		if cp.MaxRetries < 0 {
			return nil, fieldError(fieldPath+".max_retries",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		if cp.RetryBackoffInitial < 0 {
			return nil, fieldError(fieldPath+".retry_backoff_initial",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		if cp.RetryBackoffMax < 0 {
			return nil, fieldError(fieldPath+".retry_backoff_max",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		if cp.Concurrency < 0 {
			return nil, fieldError(fieldPath+".concurrency",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		if cp.BufferSize < 0 {
			return nil, fieldError(fieldPath+".buffer_size",
				fmt.Sprintf("must be >= 0 (custom provider %q)", cp.Name))
		}
		names[cp.Name] = struct{}{}
	}
	// Cross-check: if `llm.provider` is set and doesn't match a
	// native bifrost provider, it MUST match a declared custom-
	// provider name. The mock-driver short-circuit above prevents
	// this from firing in the test fixture.
	if driver != "mock" && c.LLM.Provider != "" {
		_, native := nativeBifrostProviders[c.LLM.Provider]
		_, custom := names[c.LLM.Provider]
		if !native && !custom {
			return nil, fieldError("llm.provider",
				fmt.Sprintf("must match a native bifrost provider OR a declared llm.custom_providers entry; got %q (native: %s; declared custom: %s)",
					c.LLM.Provider, sortedKeys(nativeBifrostProviders), sortedKeysFromSet(names)))
		}
	}
	return names, nil
}

// validateLLMNetworkDefaults rejects negative durations / counts.
// Zero-valued fields are accepted — they fall through to bifrost's
// package-level defaults at construction.
func (c *Config) validateLLMNetworkDefaults() error {
	nd := c.LLM.NetworkDefaults
	if nd.Timeout < 0 {
		return fieldError("llm.network_defaults.timeout", "must be >= 0")
	}
	if nd.MaxRetries < 0 {
		return fieldError("llm.network_defaults.max_retries", "must be >= 0")
	}
	if nd.RetryBackoffInitial < 0 {
		return fieldError("llm.network_defaults.retry_backoff_initial", "must be >= 0")
	}
	if nd.RetryBackoffMax < 0 {
		return fieldError("llm.network_defaults.retry_backoff_max", "must be >= 0")
	}
	if nd.Concurrency < 0 {
		return fieldError("llm.network_defaults.concurrency", "must be >= 0")
	}
	if nd.BufferSize < 0 {
		return fieldError("llm.network_defaults.buffer_size", "must be >= 0")
	}
	return nil
}

// sortedKeysFromSet renders a comma-separated list of map keys for
// error messages, matching `sortedKeys` but for the custom-provider
// name set so callers don't have to convert.
func sortedKeysFromSet(m map[string]struct{}) string {
	if len(m) == 0 {
		return "(none)"
	}
	return sortedKeys(m)
}

// sortedKeys returns a deterministic comma-separated list of map
// keys for human-readable error messages. Avoids depending on
// Go's randomized map iteration making the error text non-stable.
func sortedKeys(m map[string]struct{}) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Tiny manual sort to avoid pulling in `sort` for one call site.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return strings.Join(keys, ",")
}

// allowedDevHotReloadPolicies enumerates the retain-
// in-flight policy values an operator may configure under
// `cli.dev_hot_reload.policy`. Centralised here so both the validator
// and the dev cmd reference one allowlist; unknown values fail loud at
// load time per CLAUDE.md §13 ("fail loudly at boot").
var allowedDevHotReloadPolicies = map[string]struct{}{
	DevHotReloadPolicyDrain:    {},
	DevHotReloadPolicyCancel:   {},
	DevHotReloadPolicyDisabled: {},
}

// validateCLI checks the CLI section. is the first
// consumer — the `cli.dev_hot_reload` block configures the `harbor dev`
// fsnotify watcher. Unknown policy values are rejected; a negative
// drain timeout is rejected; an empty WatchRoots list is rejected when
// the watcher is ENABLED via explicit operator opt-in. An entirely
// zero-valued block (`Enabled == nil` AND `Policy == ""` AND no other
// fields set) is accepted as the "operator didn't touch it" case —
// the loader's `Defaults()` seeds the production defaults when going
// through `Load`, while library callers / tests that construct
// `*config.Config` by hand are allowed to skip the CLI section without
// tripping the watcher's enabled-but-rootless guard.
func (c *Config) validateCLI() error {
	hr := c.CLI.DevHotReload
	if hr.Policy != "" {
		if _, ok := allowedDevHotReloadPolicies[hr.Policy]; !ok {
			return fieldError("cli.dev_hot_reload.policy",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedDevHotReloadPolicies), hr.Policy))
		}
	}
	if hr.DrainTimeout < 0 {
		return fieldError("cli.dev_hot_reload.drain_timeout",
			fmt.Sprintf("must be >= 0, got %s", hr.DrainTimeout))
	}
	for i, root := range hr.WatchRoots {
		if strings.TrimSpace(root) == "" {
			return fieldError(fmt.Sprintf("cli.dev_hot_reload.watch_roots[%d]", i),
				"path must not be empty")
		}
	}
	// "Operator didn't touch it" zero-value detection: every field is
	// at its zero. The loader's `Defaults()` runs before yaml unmarshal,
	// so any operator who LOADED a config (via `config.Load`) has
	// non-zero defaults populated. Skipping the enabled-but-rootless
	// check in this case lets hand-built test configs round-trip
	// without per-test CLI seeding while still rejecting the
	// production-typo case (an operator's yaml that explicitly sets
	// `enabled: true` with `watch_roots: []`).
	zeroValue := hr.Enabled == nil && hr.Policy == "" && hr.DrainTimeout == 0 && len(hr.WatchRoots) == 0
	if zeroValue {
		return nil
	}
	// An enabled watcher with no roots is rejected. After the loader's
	// defaults pass + operator yaml merge, an explicit `enabled: true`
	// (or implicit via leaving the loader's default) with `watch_roots:
	// []` is a configuration typo per §13.
	enabled := hr.Enabled == nil || *hr.Enabled
	if enabled && hr.Policy != DevHotReloadPolicyDisabled && len(hr.WatchRoots) == 0 {
		return fieldError("cli.dev_hot_reload.watch_roots",
			"must list at least one path when hot-reload is enabled (set enabled: false or policy: disabled to opt out)")
	}
	return nil
}

// LiveReloadable returns dotted YAML paths for every field tagged
// `reload:"live"`. Harbor ships zero live fields so this returns
// an empty slice; later phases that opt in extend it automatically.
func (c *Config) LiveReloadable() []string {
	var paths []string
	v := reflect.ValueOf(c).Elem()
	collectLiveReload(v, nil, &paths)
	return paths
}

func collectLiveReload(v reflect.Value, prefix []string, out *[]string) {
	t := v.Type()
	for i := range v.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := yamlName(f)
		if name == "" || name == "-" {
			continue
		}
		// Build a child path without aliasing `prefix` — a bare
		// append could share backing storage across sibling fields.
		path := make([]string, 0, len(prefix)+1)
		path = append(path, prefix...)
		path = append(path, name)
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct {
			collectLiveReload(fv, path, out)
			continue
		}
		if f.Tag.Get("reload") == "live" {
			*out = append(*out, strings.Join(path, "."))
		}
	}
}
