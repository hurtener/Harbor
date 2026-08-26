package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hurtener/Harbor/internal/persistence/postgrespool"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/virtualagent"
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

// jwksMaxStaleFloor is the smallest configurable `identity.jwks_max_stale`
// ceiling. A ceiling below the minimum possible JWKS refresh window can
// never be satisfied, so a positive value below this floor is rejected.
// Zero is accepted and means "apply the safe built-in default." The
// single source for the floor; validateIdentity derives its rejection
// message from this const rather than hardcoding the literal.
const jwksMaxStaleFloor = 1 * time.Minute

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
		c.validateRuntime,
		c.validateTelemetry,
		c.validatePostgres,
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
		c.validateObservability,
		c.validatePlanner,
		c.validateMultimodal,
		c.validateCLI,
		c.validateVirtualAgents,
	)
	for _, v := range validators {
		if err := v(); err != nil {
			return c.wrapValidationError(err)
		}
	}
	return nil
}

func (c *Config) validatePostgres() error {
	p := c.Postgres.Pool
	if p.MaxOpen < 0 {
		return fieldError("postgres.pool.max_open", "must be zero (default) or at least 1")
	}
	if p.MaxIdle < 0 {
		return fieldError("postgres.pool.max_idle", "must be zero (default) or non-negative")
	}
	maxOpen := p.MaxOpen
	if maxOpen == 0 {
		maxOpen = postgrespool.DefaultMaxOpenConns
	}
	maxIdle := p.MaxIdle
	if maxIdle == 0 {
		maxIdle = postgrespool.DefaultMaxIdleConns
	}
	if maxIdle > maxOpen {
		return fieldError("postgres.pool.max_idle", fmt.Sprintf("must be <= postgres.pool.max_open (%d)", maxOpen))
	}
	if p.ConnMaxLifetime < 0 {
		return fieldError("postgres.pool.conn_max_lifetime", "must be zero (default) or non-negative")
	}
	if p.ConnMaxIdleTime < 0 {
		return fieldError("postgres.pool.conn_max_idle_time", "must be zero (default) or non-negative")
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

// validateRuntime validates the `runtime.*` block. Today that is only the
// run-completion hook: a non-positive timeout is defaulted at run start (not
// an error), but a negative timeout is a mistake and rejected loud; a set
// `run_completion.timeout` with an empty `run_completion.tool` is a
// misconfiguration (a timeout for a hook that will never fire).
func (c *Config) validateRuntime() error {
	rc := c.Runtime.Hooks.RunCompletion
	if rc.Timeout < 0 {
		return fieldError("runtime.hooks.run_completion.timeout", "must not be negative")
	}
	if rc.Timeout != 0 && strings.TrimSpace(rc.Tool) == "" {
		return fieldError("runtime.hooks.run_completion.tool",
			"must be set when runtime.hooks.run_completion.timeout is configured")
	}
	return c.validateRuntimeNaming()
}

// validateRuntimeNaming validates the `runtime.naming` fleet-default
// auto-naming block: the same bounds the agent-config `naming` section is
// validated against. A negative after_turns / repeat_every / max_repetitions
// is rejected; a set max_title_len must be in [8,200]; a repeat_every > 0 with
// max_repetitions < 1 is rejected (no unlimited value exists). The model is
// validated against ModelProfiles at boot when set. The block is only
// meaningfully consulted when auto is true, but the bounds are enforced
// regardless so a mistake surfaces at boot rather than at first run.
func (c *Config) validateRuntimeNaming() error {
	n := c.Runtime.Naming
	reasoningMode := strings.TrimSpace(n.ReasoningMode)
	if n.ReasoningMode != reasoningMode || (reasoningMode != "" && reasoningMode != "off" && reasoningMode != "provider_default") {
		return fieldError("runtime.naming.reasoning_mode", "must be one of off, provider_default")
	}
	if n.AfterTurns < 0 {
		return fieldError("runtime.naming.after_turns", "must not be negative")
	}
	if n.RepeatEvery < 0 {
		return fieldError("runtime.naming.repeat_every", "must not be negative")
	}
	if n.MaxRepetitions < 0 {
		return fieldError("runtime.naming.max_repetitions", "must not be negative")
	}
	if n.RepeatEvery > 0 && n.MaxRepetitions < 1 {
		return fieldError("runtime.naming.max_repetitions",
			"must be >= 1 when runtime.naming.repeat_every > 0 (no unlimited value exists)")
	}
	if n.MaxTitleLen != 0 && (n.MaxTitleLen < 8 || n.MaxTitleLen > 200) {
		return fieldError("runtime.naming.max_title_len", "must be within [8,200]")
	}
	if strings.TrimSpace(n.Model) != "" {
		if _, ok := c.LLM.ModelProfiles[n.Model]; !ok {
			return fieldError("runtime.naming.model",
				fmt.Sprintf("references model %q with no configured llm.model_profiles entry", n.Model))
		}
	}
	return nil
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
	// The pprof debug listener is loopback-only by construction. An empty
	// DebugAddr disables it; a non-empty one must be a loopback host:port
	// — exposing a profiler off-box is a security footgun, so the
	// validator fails closed (CLAUDE.md §7). The HARBOR_DEBUG_ADDR env
	// override (cmd/harbor) applies the SAME gate via this shared helper.
	if c.Server.DebugAddr != "" {
		if err := ValidateLoopbackAddr(c.Server.DebugAddr); err != nil {
			return fieldError("server.debug_addr", err.Error())
		}
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
	// The max-stale ceiling fails closed: a negative value is nonsense,
	// and a positive value below the refresh-window floor can never be
	// satisfied. Zero is accepted and means "apply the safe default."
	// There is intentionally no "disable" sentinel — Harbor's posture is
	// fail-closed (no identity-downgrading knobs).
	if c.Identity.JWKSMaxStale < 0 {
		return fieldError("identity.jwks_max_stale", "must not be negative")
	}
	if c.Identity.JWKSMaxStale > 0 && c.Identity.JWKSMaxStale < jwksMaxStaleFloor {
		return fieldError("identity.jwks_max_stale",
			fmt.Sprintf("must be >= %s or 0 for the safe default", jwksMaxStaleFloor))
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
	if err := validatePostgresMigrationMode("state.migration_mode", c.State.Driver, c.State.MigrationMode); err != nil {
		return err
	}
	return nil
}

func validatePostgresMigrationMode(field, driver string, mode sqlmigrate.Mode) error {
	if _, err := mode.Resolve(); err != nil {
		return fieldError(field, err.Error())
	}
	if mode != "" && driver != "postgres" {
		return fieldError(field, fmt.Sprintf("must be empty unless driver=%q", "postgres"))
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
		// A brokered primary (credential_source: remote) sources its key from
		// the coordinator broker, NOT llm.api_key — so the api_key-required
		// check is skipped (and the brokered-XOR-local rule below REJECTS a set
		// api_key). The timeout knob still applies.
		brokered := c.LLM.CredentialSource == "remote"
		if !isCustom {
			if c.LLM.APIKey == "" && !brokered {
				return fieldError("llm.api_key", "must not be empty (or declare llm.provider as a custom-provider name, or set llm.credential_source: remote)")
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
	if err := c.validateInferenceBrokers(); err != nil {
		return err
	}
	if err := validateLLMProviderRoute(c.LLM.ProviderRoute); err != nil {
		return err
	}
	if err := c.validateLLMExternalGrant(); err != nil {
		return err
	}
	return nil
}

func validateLLMProviderRoute(cfg LLMProviderRouteConfig) error {
	configured := strings.TrimSpace(cfg.ResolverURL) != "" || strings.TrimSpace(cfg.AuthTokenEnv) != "" ||
		strings.TrimSpace(cfg.RuntimeID) != "" || cfg.Timeout != 0
	if !configured {
		return nil
	}
	if strings.TrimSpace(cfg.ResolverURL) == "" {
		return fieldError("llm.provider_route.resolver_url", "must be set when provider_route is configured")
	}
	if err := validatePinnedServiceURL("llm.provider_route.resolver_url", cfg.ResolverURL); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.AuthTokenEnv) == "" {
		return fieldError("llm.provider_route.auth_token_env", "must name the env var holding the runtime service token")
	}
	if strings.TrimSpace(cfg.RuntimeID) == "" {
		return fieldError("llm.provider_route.runtime_id", "must be set when provider_route is configured")
	}
	if cfg.Timeout < 0 {
		return fieldError("llm.provider_route.timeout", "must be non-negative")
	}
	return nil
}

// validateLLMExternalGrant validates the non-secret runtime grant posture.
// The verifier is constructed only after this gate succeeds; malformed key
// material must therefore fail at boot rather than on the first provider
// call. A configured organization is the runtime-side authority fence and
// is deliberately required for every enabled mode.
func (c *Config) validateLLMExternalGrant() error {
	g := c.LLM.ExternalGrant
	if g.Mode == "" || g.Mode == "disabled" {
		if coordinatorConfigured(g.Coordinator) {
			return fieldError("llm.external_grant.coordinator", "requires external grants to be optional or required")
		}
		return nil
	}
	if g.Mode != "optional" && g.Mode != "required" {
		return fieldError("llm.external_grant.mode", fmt.Sprintf("must be one of \"disabled\", \"optional\", \"required\", got %q", g.Mode))
	}
	if g.RouteMode != "" && g.RouteMode != "runtime_default" && g.RouteMode != "coordinator_bound" {
		return fieldError("llm.external_grant.route_mode", fmt.Sprintf("must be empty, \"runtime_default\", or \"coordinator_bound\", got %q", g.RouteMode))
	}
	for path, value := range map[string]string{
		"audience":   g.Audience,
		"runtime_id": g.RuntimeID,
	} {
		if strings.TrimSpace(value) == "" {
			return fieldError("llm.external_grant."+path, "must be set when external grants are enabled")
		}
	}
	for i, organization := range g.AuthorizedOrganizations {
		if strings.TrimSpace(organization) == "" {
			return fieldError(fmt.Sprintf("llm.external_grant.authorized_organizations[%d]", i), "must not be empty")
		}
	}
	if len(g.PublicKeys) == 0 {
		return fieldError("llm.external_grant.public_keys", "must contain at least one base64-encoded Ed25519 public key when external grants are enabled")
	}
	for id, encoded := range g.PublicKeys {
		if strings.TrimSpace(id) == "" {
			return fieldError("llm.external_grant.public_keys", "key ids must not be empty")
		}
		var decoded []byte
		var err error
		for _, encoding := range []*base64.Encoding{
			base64.RawURLEncoding,
			base64.URLEncoding,
			base64.RawStdEncoding,
			base64.StdEncoding,
		} {
			decoded, err = encoding.DecodeString(encoded)
			if err == nil {
				break
			}
		}
		if len(decoded) != ed25519.PublicKeySize {
			return fieldError(fmt.Sprintf("llm.external_grant.public_keys[%q]", id), "must be a base64-encoded Ed25519 public key")
		}
	}
	if coordinatorConfigured(g.Coordinator) {
		if err := validateExternalGrantCoordinator(g.Coordinator); err != nil {
			return err
		}
	}
	return nil
}

func coordinatorConfigured(cfg ExternalGrantCoordinatorConfig) bool {
	return strings.TrimSpace(cfg.ReceiptURL) != "" || strings.TrimSpace(cfg.TopUpURL) != "" || strings.TrimSpace(cfg.CredentialURL) != "" || strings.TrimSpace(cfg.AuthTokenEnv) != "" ||
		cfg.Timeout != 0 || cfg.MaxBatch != 0 || cfg.ReconcileInterval != 0
}

func validateExternalGrantCoordinator(cfg ExternalGrantCoordinatorConfig) error {
	if strings.TrimSpace(cfg.ReceiptURL) == "" && strings.TrimSpace(cfg.CredentialURL) == "" {
		return fieldError("llm.external_grant.coordinator.receipt_url", "must be set when the coordinator transport is configured")
	}
	if strings.TrimSpace(cfg.ReceiptURL) != "" {
		if err := validatePinnedServiceURL("llm.external_grant.coordinator.receipt_url", cfg.ReceiptURL); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.TopUpURL) != "" {
		if strings.TrimSpace(cfg.ReceiptURL) == "" {
			return fieldError("llm.external_grant.coordinator.receipt_url", "must be set when top_up_url is configured")
		}
		if err := validatePinnedServiceURL("llm.external_grant.coordinator.top_up_url", cfg.TopUpURL); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.CredentialURL) != "" {
		if err := validatePinnedServiceURL("llm.external_grant.coordinator.credential_url", cfg.CredentialURL); err != nil {
			return err
		}
	}
	if strings.TrimSpace(cfg.AuthTokenEnv) == "" {
		return fieldError("llm.external_grant.coordinator.auth_token_env", "must name the env var holding the runtime service token")
	}
	if cfg.Timeout < 0 {
		return fieldError("llm.external_grant.coordinator.timeout", "must be non-negative")
	}
	if cfg.MaxBatch < 0 || cfg.MaxBatch > 1000 {
		return fieldError("llm.external_grant.coordinator.max_batch", "must be between 0 and 1000")
	}
	if strings.TrimSpace(cfg.ReceiptURL) == "" && (cfg.MaxBatch != 0 || cfg.ReconcileInterval != 0) {
		return fieldError("llm.external_grant.coordinator.receipt_url", "must be set when receipt batching or reconciliation is configured")
	}
	if cfg.ReconcileInterval < 0 {
		return fieldError("llm.external_grant.coordinator.reconcile_interval", "must be non-negative")
	}
	return nil
}

func validatePinnedServiceURL(path, raw string) error {
	u, err := url.Parse(raw)
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 || err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fieldError(path, "must be a valid http(s) URL with a host")
	}
	if u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fieldError(path, "must not contain user info, a query, or a fragment")
	}
	if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
		return fieldError(path, "must use https unless the host is loopback")
	}
	return nil
}

// allowedLLMCredentialSources is the closed value set the primary
// provider's `credential_source` may take: the empty / "local" boot-time
// env resolution (the default) or the "remote" broker-pull.
var allowedLLMCredentialSources = map[string]struct{}{
	"": {}, "local": {}, "remote": {},
}

// validateInferenceBrokers validates the boot-declared inference-plane
// credential brokers (unique names; well-formed https / loopback
// credential_url; non-empty auth_token_env; non-negative cache_ttl /
// timeout) AND the primary provider's brokered-XOR-local credential source
// The brokered-XOR-local rule: a "remote" source REQUIRES a resolvable `inference_broker` and
// REJECTS a set `api_key` (both set is a config error); a local source
// REJECTS a set `inference_broker`. Every sink-determining value is pinned
// on the named broker, never on the wire (the credential-plane invariant).
func (c *Config) validateInferenceBrokers() error {
	brokerNames := make(map[string]struct{}, len(c.LLM.InferenceBrokers))
	for i, b := range c.LLM.InferenceBrokers {
		prefix := fmt.Sprintf("llm.inference_brokers[%d]", i)
		if b.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := brokerNames[b.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate broker name %q (must be unique within llm.inference_brokers[])", b.Name))
		}
		brokerNames[b.Name] = struct{}{}
		if b.CredentialURL == "" {
			return fieldError(prefix+".credential_url",
				"must not be empty (the coordinator credential-pull endpoint — the pinned credential sink)")
		}
		u, err := url.Parse(b.CredentialURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fieldError(prefix+".credential_url",
				fmt.Sprintf("must be a well-formed http(s) URL with a host, got %q", b.CredentialURL))
		}
		// TLS mandatory off loopback — the GET carries the runtime's service
		// bearer token and returns the provider API key (§7).
		if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
			return fieldError(prefix+".credential_url",
				fmt.Sprintf("must be https for non-loopback hosts (the pull carries the runtime service bearer and returns the provider key; plaintext http is allowed only for 127.0.0.1 / ::1 / localhost), got %q", b.CredentialURL))
		}
		if b.AuthTokenEnv == "" {
			return fieldError(prefix+".auth_token_env",
				"must not be empty (env var name holding the runtime's broker credential; §7 rule 2 — never hardcoded)")
		}
		for j, sc := range b.ScopeCeiling {
			if strings.TrimSpace(sc) == "" {
				return fieldError(fmt.Sprintf("%s.scope_ceiling[%d]", prefix, j), "must be a non-empty scope string")
			}
		}
		if b.CacheTTL < 0 {
			return fieldError(prefix+".cache_ttl", "must be >= 0")
		}
		if b.Timeout < 0 {
			return fieldError(prefix+".timeout", "must be >= 0")
		}
	}

	// Brokered XOR local — validated for the primary provider.
	src := c.LLM.CredentialSource
	if _, ok := allowedLLMCredentialSources[src]; !ok {
		return fieldError("llm.credential_source",
			fmt.Sprintf("must be one of \"\", \"local\", \"remote\"; got %q", src))
	}
	if src == "remote" {
		if c.LLM.InferenceBroker == "" {
			return fieldError("llm.inference_broker",
				"must name a declared llm.inference_brokers[] entry when llm.credential_source is \"remote\" (a brokered provider has no local key source)")
		}
		if _, ok := brokerNames[c.LLM.InferenceBroker]; !ok {
			return fieldError("llm.inference_broker",
				fmt.Sprintf("names no declared llm.inference_brokers[] entry (%q); an unknown broker fails loud", c.LLM.InferenceBroker))
		}
		if c.LLM.APIKey != "" {
			return fieldError("llm.api_key",
				"must be empty when llm.credential_source is \"remote\" (brokered XOR local — a provider declares exactly one key source, never both)")
		}
	} else if c.LLM.InferenceBroker != "" {
		return fieldError("llm.inference_broker",
			"must be empty unless llm.credential_source is \"remote\" (a local provider sources its key from api_key, not a broker)")
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
	// malformed entries. The pre-tiered single-knob fields
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
	// The HA-64 conversation-turn projection store block. Optional: an
	// empty driver leaves the projection unwired. A non-empty driver
	// must be one of the shipped triad and carries the same DSN
	// contract as the other driver-selecting blocks.
	if t := c.Sessions.Turns; t.Driver != "" {
		if _, ok := allowedProjectionDrivers[t.Driver]; !ok {
			return fieldError("sessions.turns.driver",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedProjectionDrivers), t.Driver))
		}
		if t.Driver != "inmem" && t.DSN == "" {
			return fieldError("sessions.turns.dsn",
				fmt.Sprintf("must be set when driver=%q", t.Driver))
		}
	}
	if err := validatePostgresMigrationMode("sessions.turns.migration_mode", c.Sessions.Turns.Driver, c.Sessions.Turns.MigrationMode); err != nil {
		return err
	}
	return nil
}

// allowedProjectionDrivers is the closed driver triad every
// runtime-owned durable projection store (the HA-64 turns projection
// and the HA-65 observability rollups) ships with — in-memory
// (dev/embedded), SQLite (modernc.org/sqlite, CGo-free) and Postgres
// (pgx) with conformance parity.
var allowedProjectionDrivers = map[string]struct{}{
	"inmem":    {},
	"sqlite":   {},
	"postgres": {},
}

// validateObservability validates the HA-65 observability-rollup
// block. Optional: an empty rollups.driver leaves the projection
// unwired and the surface at 501. A non-empty driver must be one of
// the shipped triad and carries the DSN contract.
func (c *Config) validateObservability() error {
	if r := c.Observability.Rollups; r.Driver != "" {
		if _, ok := allowedProjectionDrivers[r.Driver]; !ok {
			return fieldError("observability.rollups.driver",
				fmt.Sprintf("must be one of %s, got %q",
					sortedKeys(allowedProjectionDrivers), r.Driver))
		}
		if r.Driver != "inmem" && r.DSN == "" {
			return fieldError("observability.rollups.dsn",
				fmt.Sprintf("must be set when driver=%q", r.Driver))
		}
	}
	if err := validatePostgresMigrationMode("observability.rollups.migration_mode", c.Observability.Rollups.Driver, c.Observability.Rollups.MigrationMode); err != nil {
		return err
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
	if err := validatePostgresMigrationMode("artifacts.migration_mode", c.Artifacts.Driver, c.Artifacts.MigrationMode); err != nil {
		return err
	}
	if c.Artifacts.Driver == "s3" && c.Artifacts.S3Bucket == "" {
		return fieldError("artifacts.s3_bucket",
			fmt.Sprintf("must be set when driver=%q", c.Artifacts.Driver))
	}
	if c.Artifacts.HeavyOutputThresholdBytes < 0 {
		return fieldError("artifacts.heavy_output_threshold_bytes", "must be >= 0")
	}
	// The read-back bound. Zero on either field means "unset" and
	// resolves to the documented built-in, which is what keeps a
	// configuration written before these fields existed valid. A NEGATIVE
	// value is not an omission — it is an operator asking for something
	// incoherent, so it is refused by name rather than silently
	// reinterpreted.
	if c.Artifacts.FetchDefaultMaxBytes < 0 {
		return fieldError("artifacts.fetch_default_max_bytes",
			"must be >= 0 (0 selects the built-in 64 KiB default)")
	}
	if c.Artifacts.FetchHardMaxBytes < 0 {
		return fieldError("artifacts.fetch_hard_max_bytes",
			"must be >= 0 (0 selects the built-in 1 MiB default)")
	}
	// The comparison runs on the RESOLVED values, because a default
	// above a configured ceiling is the same misconfiguration whether the
	// operator wrote the default or inherited it.
	if def, hard := c.Artifacts.ResolvedFetchDefaultMaxBytes(), c.Artifacts.ResolvedFetchHardMaxBytes(); def > hard {
		return fieldError("artifacts.fetch_default_max_bytes",
			fmt.Sprintf("resolved default %d must not exceed the resolved artifacts.fetch_hard_max_bytes %d", def, hard))
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
	if err := validatePostgresMigrationMode("memory.migration_mode", c.Memory.Driver, c.Memory.MigrationMode); err != nil {
		return err
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
	if c.Memory.RecentTurns < 0 {
		return fieldError("memory.recent_turns", "must be >= 0")
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
// ships `"localdb"` and the durable/shared `"postgres"` store.
var allowedSkillsDrivers = map[string]struct{}{
	"localdb":  {},
	"postgres": {},
}

// skillsDriversRequiringDSN names the drivers whose `DSN` field must
// be supplied. Mirrors `memoryDriversRequiringDSN`.
var skillsDriversRequiringDSN = map[string]struct{}{
	"localdb":  {},
	"postgres": {},
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
	if err := c.validateSessionPersonalCutover(); err != nil {
		return err
	}
	// Directory shape-validation runs unconditionally so a typo'd
	// `skills.directory` block fails at load time even when the
	// parent store fields are empty.
	if err := c.validateSkillsDirectory(); err != nil {
		return err
	}
	// boot_agent_packs shape-validation runs unconditionally too (same
	// posture — a malformed pack declaration must fail at load time even
	// before the store fields are consulted).
	if err := c.validateBootAgentPacks(); err != nil {
		return err
	}
	// Retrieval-mode shape-validation runs unconditionally so a
	// typo'd `skills.retrieval` fails at load time too.
	if _, ok := allowedRetrievalModes[c.Skills.Retrieval]; !ok {
		return fieldError("skills.retrieval",
			fmt.Sprintf("must be empty or %q, got %q", "semantic", c.Skills.Retrieval))
	}
	if err := validatePostgresMigrationMode("skills.migration_mode", c.Skills.Driver, c.Skills.MigrationMode); err != nil {
		return err
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
		if len(c.Skills.SessionPersonalCutover.Tenants) > 0 {
			return fieldError("skills.driver",
				"must not be empty when skills.session_personal_cutover is set (the declared migration requires the configured skill store)")
		}
		if len(c.Skills.BootAgentPacks) > 0 {
			return fieldError("skills.driver",
				"must not be empty when skills.boot_agent_packs is set (the boot agent pack composite resolver reads the configured skill store)")
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

const maxSessionPersonalCutoverTenants = 256

const (
	maxSessionPersonalCutoverTenantID = 128
	maxSessionPersonalCutoverEpoch    = 128
	maxSessionPersonalCutoverDigest   = 256
)

func (c *Config) validateSessionPersonalCutover() error {
	declarations := c.Skills.SessionPersonalCutover.Tenants
	if len(declarations) > maxSessionPersonalCutoverTenants {
		return fieldError("skills.session_personal_cutover.tenants", fmt.Sprintf("must contain at most %d declarations", maxSessionPersonalCutoverTenants))
	}
	seen := make(map[string]struct{}, len(declarations))
	for i, declaration := range declarations {
		path := fmt.Sprintf("skills.session_personal_cutover.tenants[%d]", i)
		if !validSessionPersonalCutoverToken(declaration.TenantID, maxSessionPersonalCutoverTenantID) {
			return fieldError(path+".tenant_id", "must be a trimmed bounded token")
		}
		if !validSessionPersonalCutoverToken(declaration.Epoch, maxSessionPersonalCutoverEpoch) {
			return fieldError(path+".epoch", "must be a trimmed bounded token")
		}
		if !validSessionPersonalCutoverToken(declaration.RosterDigest, maxSessionPersonalCutoverDigest) {
			return fieldError(path+".roster_digest", "must be a trimmed bounded token")
		}
		if _, ok := seen[declaration.TenantID]; ok {
			return fieldError(path+".tenant_id", fmt.Sprintf("duplicates tenant %q", declaration.TenantID))
		}
		seen[declaration.TenantID] = struct{}{}
	}
	return nil
}

func validSessionPersonalCutoverToken(value string, maximum int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// validateBootAgentPackDirectoryShape is the shared, normalization-free
// directory shape check for `skills.boot_agent_packs[].directory`. BOTH
// the validator (validateBootAgentPacks) and the loader's resolve pass
// (resolveBootAgentPackDirectories) run this SAME predicate so the
// raw-value contract cannot drift between the two.
//
// It rejects an empty or whitespace-surrounded value outright and
// enforces the rune ceiling on the RAW/stored value — never on a
// trimmed or filepath.Clean-ed copy. The loader MUST run it BEFORE any
// Clean/Join normalization: filepath.Clean collapses an over-bound
// `a/../` path below the ceiling, and filepath.Join resolves a relative
// value against the config directory, either of which would otherwise
// let a raw value validation must refuse slip through shortened.
func validateBootAgentPackDirectoryShape(directory string) error {
	if directory == "" || directory != strings.TrimSpace(directory) {
		return fmt.Errorf("must not be empty or have surrounding whitespace (an absolute path, or a relative path the loader resolves against the config file's directory)")
	}
	if r := len([]rune(directory)); r > MaxBootAgentPackDirectoryRunes {
		return fmt.Errorf("must be at most %d runes, got %d", MaxBootAgentPackDirectoryRunes, r)
	}
	return nil
}

// validateBootAgentPacks validates the closed shape of the
// `skills.boot_agent_packs` declarations (see BootAgentPackConfig): the
// deterministic bounds, the required per-declaration fields, the unique
// (tenant_id, agent_id) pairs, and the exact one-relative-package-
// directory-name include shape. It runs unconditionally from
// validateSkills so a malformed declaration fails at load time even
// before the parent store fields are consulted. It performs NO
// filesystem reads, lifecycle calls, or state writes — the loader's
// directory resolution (resolveBootAgentPackDirectories) is a separate,
// also I/O-free, lexical pass.
//
// The boot-agent MATCH check (each entry's agent_id must equal the
// runtime-resolved boot/default agent id) is deliberately NOT here: the
// config package does not know that value. Runtime calls
// [Config.ValidateBootAgentPacksForAgent] with the authoritative
// resolved id.
func (c *Config) validateBootAgentPacks() error {
	packs := c.Skills.BootAgentPacks
	if len(packs) == 0 {
		return nil
	}
	if len(packs) > MaxBootAgentPacks {
		return fieldError("skills.boot_agent_packs",
			fmt.Sprintf("must contain at most %d declarations, got %d", MaxBootAgentPacks, len(packs)))
	}
	seenPair := make(map[string]struct{}, len(packs))
	aggregateIncludes := 0
	for i, p := range packs {
		prefix := fmt.Sprintf("skills.boot_agent_packs[%d]", i)
		// tenant_id / agent_id reuse the same trimmed-bounded-token
		// predicate the session-personal-cutover tenant declarations use:
		// no surrounding whitespace, printable ASCII, bounded length.
		if !validSessionPersonalCutoverToken(p.TenantID, MaxBootAgentPackFieldRunes) {
			return fieldError(prefix+".tenant_id",
				fmt.Sprintf("must be a trimmed, bounded token (no surrounding whitespace, printable ASCII, at most %d runes)", MaxBootAgentPackFieldRunes))
		}
		if !validSessionPersonalCutoverToken(p.AgentID, MaxBootAgentPackFieldRunes) {
			return fieldError(prefix+".agent_id",
				fmt.Sprintf("must be a trimmed, bounded token (no surrounding whitespace, printable ASCII, at most %d runes)", MaxBootAgentPackFieldRunes))
		}
		// directory: absolute (the authoritative deployment shape) or
		// relative (resolved by the loader against the config file's
		// directory, never CWD). The RAW value's shape — no surrounding
		// whitespace, within the rune ceiling — is enforced by the shared
		// validateBootAgentPackDirectoryShape helper, the SAME predicate
		// the loader's resolve pass applies BEFORE any Clean/Join
		// normalization. Bounding the stored/raw value (never a trimmed
		// or normalized copy) means an arbitrary run of spaces or an
		// over-bound `a/../` path cannot be shortened past the ceiling.
		// Shape-only, no I/O.
		if err := validateBootAgentPackDirectoryShape(p.Directory); err != nil {
			return fieldError(prefix+".directory", err.Error())
		}
		if len(p.Include) == 0 {
			return fieldError(prefix+".include",
				"must list at least one package-directory name")
		}
		if len(p.Include) > MaxBootAgentPackIncludes {
			return fieldError(prefix+".include",
				fmt.Sprintf("must contain at most %d entries, got %d", MaxBootAgentPackIncludes, len(p.Include)))
		}
		aggregateIncludes += len(p.Include)
		if aggregateIncludes > MaxBootAgentPackAggregateIncludes {
			return fieldError("skills.boot_agent_packs",
				fmt.Sprintf("aggregate include count %d exceeds the cap of %d across all declarations (the boot resolver enumerates every include into one composed view)", aggregateIncludes, MaxBootAgentPackAggregateIncludes))
		}
		// Duplicate (tenant_id, agent_id) pairs. The pair key is
		// NUL-joined; a NUL cannot appear in either validated token
		// (printable ASCII), so the separator is collision-free.
		pair := p.TenantID + "\x00" + p.AgentID
		if _, dup := seenPair[pair]; dup {
			return fieldError(prefix,
				fmt.Sprintf("duplicates (tenant_id=%q, agent_id=%q) already declared earlier in the list — each (tenant, agent) pair may declare at most one boot pack", p.TenantID, p.AgentID))
		}
		seenPair[pair] = struct{}{}
		// Include duplicates: raw (as written) AND normalized
		// (case-insensitive — the resolver matches package-directory
		// names case-insensitively, mirroring skills.CanonicalPackName).
		seenRaw := make(map[string]struct{}, len(p.Include))
		seenNorm := make(map[string]struct{}, len(p.Include))
		for j, inc := range p.Include {
			incPrefix := fmt.Sprintf("%s.include[%d]", prefix, j)
			norm, err := validateBootAgentPackInclude(inc)
			if err != nil {
				return fieldError(incPrefix, err.Error())
			}
			if _, dup := seenRaw[inc]; dup {
				return fieldError(incPrefix,
					fmt.Sprintf("duplicate include %q (must be unique)", inc))
			}
			if _, dup := seenNorm[norm]; dup {
				return fieldError(incPrefix,
					fmt.Sprintf("duplicate include %q — a case-variant of an earlier entry (the resolver matches package-directory names case-insensitively)", inc))
			}
			seenRaw[inc] = struct{}{}
			seenNorm[norm] = struct{}{}
		}
	}
	return nil
}

// validateBootAgentPackInclude checks that ONE `include` entry is EXACTLY
// one relative package-directory name: non-empty with no surrounding
// whitespace, not `.` / `..`, single-segment (no `/` or `\` path
// separator), and no drive/volume/URI form — the absolute and
// multi-segment shapes all reduce to a separator or prefix, so the
// single-segment rule rejects them together. On success it returns the
// normalized comparison form ([normalizeBootAgentPackInclude]) the
// duplicate pass keys on.
func validateBootAgentPackInclude(name string) (string, error) {
	if name == "" || name != strings.TrimSpace(name) {
		return "", errors.New(`must be a non-empty relative package-directory name with no surrounding whitespace`)
	}
	if name == "." || name == ".." {
		return "", errors.New(`must be a single relative package-directory name — "." / ".." are not package directories`)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", errors.New(`must be exactly ONE relative package-directory name — no path separators ("/" or "\"), no absolute path, and no multi-segment path`)
	}
	if strings.Contains(name, ":") {
		return "", errors.New(`must be a single relative package-directory name — no drive/volume prefix or URI form`)
	}
	if r := len([]rune(name)); r > MaxBootAgentPackFieldRunes {
		return "", fmt.Errorf("must be at most %d runes, got %d", MaxBootAgentPackFieldRunes, r)
	}
	return normalizeBootAgentPackInclude(name), nil
}

// normalizeBootAgentPackInclude returns the canonical comparison form of
// an include entry: trimmed + lowercased, the same canonicalisation the
// skills pack resolver applies to pack names (skills.CanonicalPackName),
// so a case-variant duplicate cannot declare the same package twice.
func normalizeBootAgentPackInclude(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// ValidateBootAgentPacksForAgent is the runtime-facing, pure validation
// helper for `skills.boot_agent_packs`. The runtime passes the
// AUTHORITATIVE resolved boot/default agent id — the value comes from
// runtime assembly, never a hard-coded default in this package — and
// every declared entry whose agent_id differs is rejected. An EMPTY
// resolved id fails when entries exist (fail loud, never a silent skip).
// An empty declaration list passes for any resolved id (field absent =
// today's behaviour).
//
// The helper is pure: no filesystem reads, no lifecycle calls, no state
// writes, and no mutation of the receiver — safe to call concurrently on
// a shared immutable *Config.
func (c *Config) ValidateBootAgentPacksForAgent(resolvedAgentID string) error {
	return ValidateBootAgentPacks(c.Skills.BootAgentPacks, resolvedAgentID)
}

// ValidateBootAgentPacks is the standalone form of
// [Config.ValidateBootAgentPacksForAgent] over an explicit pack slice —
// the pure predicate the runtime can call without holding a *Config. It
// performs only the boot-agent match: an entry whose agent_id differs
// from the resolved id is rejected, and an empty resolved id fails when
// entries exist. The closed SHAPE of the declarations (bounds,
// duplicates, include form) is validated separately by
// [Config.Validate] — this helper assumes the slice already passed it.
func ValidateBootAgentPacks(packs []BootAgentPackConfig, resolvedAgentID string) error {
	if len(packs) == 0 {
		return nil
	}
	if strings.TrimSpace(resolvedAgentID) == "" {
		return fieldError("skills.boot_agent_packs",
			"entries are declared but no resolved boot/default agent id was provided — the runtime must pass the authoritative resolved agent id (never resolve to a hard-coded default here)")
	}
	for i, p := range packs {
		if p.AgentID != resolvedAgentID {
			return fieldError(fmt.Sprintf("skills.boot_agent_packs[%d].agent_id", i),
				fmt.Sprintf("entry targets agent %q but the runtime-resolved boot/default agent is %q — every boot_agent_packs entry must target the resolved boot agent", p.AgentID, resolvedAgentID))
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
	// HTTP-manifest boot loader: structural validation only (existence
	// / parsing is boot's job — the validator stays I/O-free, matching
	// the `tools.mcp_servers` precedent of not probing URLs at validate
	// time). Each entry must be non-empty and unique after
	// `filepath.Clean`; relative-path resolution against the config
	// file's directory happens at `Load` time, not here (loader.go).
	seenManifests := make(map[string]struct{}, len(c.Tools.HTTPManifests))
	for i, m := range c.Tools.HTTPManifests {
		trimmed := strings.TrimSpace(m)
		if trimmed == "" {
			return fieldError(fmt.Sprintf("tools.http_manifests[%d]", i), "must not be empty")
		}
		cleaned := filepath.Clean(trimmed)
		if _, dup := seenManifests[cleaned]; dup {
			return fieldError(fmt.Sprintf("tools.http_manifests[%d]", i),
				fmt.Sprintf("duplicate manifest path %q (already declared earlier in the list, after normalization)", cleaned))
		}
		seenManifests[cleaned] = struct{}{}
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
		// Separator safety. A server's tools are keyed `<name>_<tool>` in the
		// shared catalog, so two names where one is an underscore-extension of
		// the other make that key ambiguous — a key built by prefixing the
		// shorter name can resolve to a tool owned by the longer one, which is
		// how a prefix-based scoping boundary stops meaning what it claims.
		//
		// The runtime registry refuses the pairing at attach, so a config like
		// this would ABORT BOOT. Catching it here is the point: validate-time
		// is where an operator should learn that two names cannot coexist,
		// rather than discovering it when the runtime declines to start.
		// Same rule, same wording, checked one stage earlier.
		for existing := range names {
			if strings.HasPrefix(s.Name, existing+"_") || strings.HasPrefix(existing, s.Name+"_") {
				return fieldError(prefix+".name", fmt.Sprintf(
					"%q and %q are separator-ambiguous: one is an underscore-extension of the "+
						"other, so a `<name>_<tool>` catalog key would not identify one server. "+
						"The runtime refuses this pairing at attach (boot would fail). Rename one "+
						"— note that renaming changes every `<name>_<tool>` key referenced by "+
						"agent-YAML tool allow-lists, disabled_tools, paused_servers, and any "+
						"persisted agent-config revision",
					s.Name, existing))
			}
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
			if _, err := NormalizeMCPHTTPURL(s.URL); err != nil {
				return fieldError(prefix+".url", err.Error())
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
			if s.URL != "" {
				if _, err := NormalizeMCPHTTPURL(s.URL); err != nil {
					return fieldError(prefix+".url", err.Error())
				}
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
		// Egress substitution: the byte-eligibility declaration and the
		// per-tool artifact-parameter mapping. The rules are the SAME ones
		// the runtime-add door enforces, checked one stage earlier so an
		// operator learns at `harbor validate` rather than at boot.
		//
		// An `auto` transport carrying only a command auto-selects stdio at
		// connect, so it is treated as stdio here — otherwise an operator
		// would believe egress was on while the connection silently never
		// carried it, which is the degradation shape this refuses.
		stdioOrNoURL := mode == "stdio" || s.URL == ""
		if err := ValidateMCPArtifactParams(s.ArtifactParams); err != nil {
			return fieldError(prefix+".artifact_params", err.Error())
		}
		if len(s.ArtifactParams) > 0 && !s.ArtifactByteEligible {
			return fieldError(prefix+".artifact_params",
				"artifact_params requires artifact_byte_eligible: true on the same connection — a mapping on a connection an operator has not declared byte-eligible is refused rather than silently ignored, because the eligibility flag IS the containment boundary for egress substitution")
		}
		if stdioOrNoURL && s.ArtifactByteEligible {
			return fieldError(prefix+".artifact_byte_eligible",
				"must not be set on a connection without an http(s) url (stdio — explicit, or auto-selected from a command-only config): base64-encoded artifact bytes belong in an HTTP body, not a stdio frame, and declaring eligibility a connection can never exercise advertises a capability nothing services")
		}
		if stdioOrNoURL && len(s.ArtifactParams) > 0 {
			return fieldError(prefix+".artifact_params",
				"must not be set on a connection without an http(s) url (stdio — explicit, or auto-selected from a command-only config)")
		}
	}
	// The egress ceiling. Positive-or-unset: zero resolves to the
	// documented default through ResolvedMCPArtifactEgressMaxBytes, and a
	// NEGATIVE value is an operator mistake rather than a synonym for
	// "unbounded" — refused loud so a deployment never silently runs with
	// no ceiling on outbound content.
	if c.Tools.MCPArtifactEgressMaxBytes < 0 {
		return fieldError("tools.mcp_artifact_egress_max_bytes",
			fmt.Sprintf("must be > 0 when set (got %d); omit the key to take the %d-byte default",
				c.Tools.MCPArtifactEgressMaxBytes, DefaultMCPArtifactEgressMaxBytes))
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
	// MCP add-connection stdio allowlist. Empty list is valid (fail-closed:
	// every stdio add is rejected). Each entry must be a non-empty command /
	// path; duplicates are rejected so the operator's intent is unambiguous.
	if c.Tools.MCPAddConnection != nil {
		seenCmd := make(map[string]struct{}, len(c.Tools.MCPAddConnection.StdioAllowlist))
		for i, cmd := range c.Tools.MCPAddConnection.StdioAllowlist {
			field := fmt.Sprintf("tools.mcp_add_connection.stdio_allowlist[%d]", i)
			if strings.TrimSpace(cmd) == "" {
				return fieldError(field, "must not be empty")
			}
			if _, dup := seenCmd[cmd]; dup {
				return fieldError(field, fmt.Sprintf("duplicate command %q (must be unique)", cmd))
			}
			seenCmd[cmd] = struct{}{}
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
	// oauthProviderAllowedHosts maps each provider name to its
	// boot-declared downstream-sink allow-list (normalised), consulted by
	// the MCP southbound cross-reference pass below to enforce the
	// credential-plane invariant (no admin-writable field determines a
	// credential sink) fail-closed.
	oauthProviderAllowedHosts := make(map[string][]string, len(c.Tools.OAuthProviders))
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
		// Credential source: `env` (default) resolves the client
		// credential from ClientIDEnv / ClientSecretEnv at boot; `remote`
		// pulls it from a coordinator endpoint at first need. Exactly the
		// declared source's fields are required; declaring both is a
		// validation error (one source, no dual path — §13).
		source := p.CredentialSource
		if source == "" {
			source = "env"
		}
		if _, ok := allowedCredentialSources[source]; !ok {
			return fieldError(prefix+".credential_source",
				fmt.Sprintf("must be one of %s, got %q", sortedKeys(allowedCredentialSources), p.CredentialSource))
		}
		switch source {
		case "env":
			if p.ClientIDEnv == "" {
				return fieldError(prefix+".client_id_env",
					"must not be empty (env var name holding the client_id; §7 rule 2 — never hardcoded)")
			}
			if p.ClientSecretEnv == "" {
				return fieldError(prefix+".client_secret_env",
					"must not be empty (env var name holding the client_secret; §7 rule 2 — never hardcoded)")
			}
			if p.Remote != nil {
				return fieldError(prefix+".remote",
					"must not be set when credential_source is \"env\" (the remote block declares the coordinator pull; one source per entry — §13)")
			}
		case "remote":
			// The interactive `oauth2` driver bakes its credential into the
			// underlying provider at construction, so a lazy remote pull
			// cannot attach to an identity-bearing ctx for its audit event.
			// `remote` is restricted to the non-interactive `tokenexchange`
			// driver, which resolves lazily under a verified identity.
			if p.Driver != "tokenexchange" {
				return fieldError(prefix+".credential_source",
					fmt.Sprintf("\"remote\" is supported only by the \"tokenexchange\" driver (got driver %q); the interactive oauth2 flow resolves its credential at construction", p.Driver))
			}
			if p.ClientIDEnv != "" || p.ClientSecretEnv != "" {
				return fieldError(prefix+".credential_source",
					"must not set client_id_env / client_secret_env alongside \"remote\" (the credential is pulled from the coordinator, not the env; one source per entry — §13)")
			}
			if p.Remote == nil {
				return fieldError(prefix+".remote",
					"must be set when credential_source is \"remote\" (declares the coordinator url + auth_token_env)")
			}
			if p.Remote.URL == "" {
				return fieldError(prefix+".remote.url", "must not be empty (the coordinator credential endpoint)")
			}
			u, err := url.Parse(p.Remote.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fieldError(prefix+".remote.url",
					fmt.Sprintf("must be a well-formed http(s) URL with a host, got %q", p.Remote.URL))
			}
			// The fetch carries the runtime's service bearer token, so TLS
			// is mandatory (§7). The single carve-out is a loopback host
			// (127.0.0.0/8 / ::1 / localhost) for the local fixture / dev
			// case — a plaintext bearer never leaves the box there.
			if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
				return fieldError(prefix+".remote.url",
					fmt.Sprintf("must be https for non-loopback hosts (the fetch sends the runtime's service bearer token; plaintext http is allowed only for 127.0.0.1 / ::1 / localhost), got %q", p.Remote.URL))
			}
			if p.Remote.AuthTokenEnv == "" {
				return fieldError(prefix+".remote.auth_token_env",
					"must not be empty (env var name holding the runtime service token for the coordinator fetch; §7 rule 2 — never hardcoded)")
			}
			if p.Remote.CacheTTL < 0 {
				return fieldError(prefix+".remote.cache_ttl", "must be >= 0")
			}
			if p.Remote.Timeout < 0 {
				return fieldError(prefix+".remote.timeout", "must be >= 0")
			}
		}
		// Per-driver endpoint requirements. The `tokenexchange` driver
		// (pull-based external-credential provisioning) talks to a
		// credential broker's RFC-8693 token-exchange endpoint only —
		// `token_url` is mandatory; `auth_url` / `redirect_url` are
		// interactive-flow fields and are waived. The `oauth2` driver
		// validates its own endpoints at construction (it may discover
		// them), so no validate-time endpoint check applies there.
		if p.Driver == "tokenexchange" && p.TokenURL == "" {
			return fieldError(prefix+".token_url",
				"must not be empty for driver \"tokenexchange\" (the credential broker's RFC-8693 token-exchange endpoint; auth_url / redirect_url are not used by this driver)")
		}
		// resource_indicator (RFC 8707) and include_actor_token (RFC 8693) ride
		// the exchange REQUEST — they have meaning only for the brokered
		// `tokenexchange` driver. Setting either on the interactive `oauth2`
		// driver (which has no exchange request to carry them on) is a
		// misconfiguration, rejected fail-loud rather than silently ignored.
		if p.Driver != "tokenexchange" {
			if p.ResourceIndicator != "" {
				return fieldError(prefix+".resource_indicator",
					fmt.Sprintf("is meaningful only for driver \"tokenexchange\" (the RFC 8707 resource rides the token-exchange request), got driver %q", p.Driver))
			}
			if p.IncludeActorToken {
				return fieldError(prefix+".include_actor_token",
					fmt.Sprintf("is meaningful only for driver \"tokenexchange\" (the RFC 8693 actor_token rides the token-exchange request), got driver %q", p.Driver))
			}
			// allow_private_token_url is a DEV-ONLY opt-in that relaxes the
			// `tokenexchange` driver's private-dial guard on its exchange POST.
			// It has meaning only for that driver (the interactive `oauth2`
			// driver has no hardened exchange client to relax) — reject it on
			// any other driver fail-loud rather than silently ignore. The bool
			// itself needs no value validation.
			if p.AllowPrivateTokenURL {
				return fieldError(prefix+".allow_private_token_url",
					fmt.Sprintf("is meaningful only for driver \"tokenexchange\" (it relaxes that driver's private-dial guard on the RFC-8693 exchange POST), got driver %q", p.Driver))
			}
		}
		// Downstream-sink allow-list entries (the credential-plane
		// invariant). Each entry must be a well-formed host[:port]
		// with a non-empty host; empty entries are a typo. Membership +
		// the mandatory-non-empty-for-bindable rule is enforced in the MCP
		// cross-reference pass below (which knows which providers a
		// connection binds).
		normHosts := make([]string, 0, len(p.AllowedDownstreamHosts))
		for j, h := range p.AllowedDownstreamHosts {
			nh := NormalizeDownstreamHost(h)
			if nh == "" {
				return fieldError(fmt.Sprintf("%s.allowed_downstream_hosts[%d]", prefix, j),
					fmt.Sprintf("must be a non-empty host[:port], got %q", h))
			}
			normHosts = append(normHosts, nh)
		}
		oauthProviderAllowedHosts[p.Name] = normHosts
	}
	// `tools.oauth_credential_brokers[]` — the boot-declared, named
	// credential SINKS (the config home a Protocol-installed zero-URL
	// provider descriptor references by name). Validation: unique names; https (or
	// loopback) token_url; a non-empty allowed_downstream_hosts; a
	// non-empty auth_token_env; non-negative cache_ttl / timeout. The
	// inline `oauth_providers[].remote` block stays valid — this list is
	// additive.
	brokerNames := make(map[string]struct{}, len(c.Tools.OAuthCredentialBrokers))
	for i, b := range c.Tools.OAuthCredentialBrokers {
		prefix := fmt.Sprintf("tools.oauth_credential_brokers[%d]", i)
		if b.Name == "" {
			return fieldError(prefix+".name", "must not be empty")
		}
		if _, dup := brokerNames[b.Name]; dup {
			return fieldError(prefix+".name",
				fmt.Sprintf("duplicate broker name %q (must be unique within tools.oauth_credential_brokers[])", b.Name))
		}
		brokerNames[b.Name] = struct{}{}
		if b.TokenURL == "" {
			return fieldError(prefix+".token_url", "must not be empty (the credential broker's RFC-8693 token-exchange endpoint — the pinned credential sink)")
		}
		u, err := url.Parse(b.TokenURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fieldError(prefix+".token_url",
				fmt.Sprintf("must be a well-formed http(s) URL with a host, got %q", b.TokenURL))
		}
		// TLS mandatory off loopback — the POST carries the org's OAuth
		// client_id / client_secret (§7).
		if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
			return fieldError(prefix+".token_url",
				fmt.Sprintf("must be https for non-loopback hosts (the token POST carries client_id/client_secret; plaintext http is allowed only for 127.0.0.1 / ::1 / localhost), got %q", b.TokenURL))
		}
		if len(b.AllowedDownstreamHosts) == 0 {
			return fieldError(prefix+".allowed_downstream_hosts",
				"must declare at least one downstream host (a bearer-minting broker must say where its tokens may be injected — the credential-plane invariant; fail-closed)")
		}
		for j, h := range b.AllowedDownstreamHosts {
			if NormalizeDownstreamHost(h) == "" {
				return fieldError(fmt.Sprintf("%s.allowed_downstream_hosts[%d]", prefix, j),
					fmt.Sprintf("must be a non-empty host[:port], got %q", h))
			}
		}
		if b.AuthTokenEnv == "" {
			return fieldError(prefix+".auth_token_env",
				"must not be empty (env var name holding the runtime's broker credential; §7 rule 2 — never hardcoded)")
		}
		// CredentialURL is the boot-pinned coordinator credential-pull endpoint
		// (the `remote` source URL a Protocol-installed broker-pull provider
		// resolves its org client credential from). Optional at boot (a broker
		// referenced by an install without it fails loud at install time), but a
		// set value must be a well-formed http(s) URL with a host, and TLS is
		// mandatory off loopback (the GET returns the org client_id/secret; §7).
		if b.CredentialURL != "" {
			cu, cerr := url.Parse(b.CredentialURL)
			if cerr != nil || (cu.Scheme != "http" && cu.Scheme != "https") || cu.Host == "" {
				return fieldError(prefix+".credential_url",
					fmt.Sprintf("must be a well-formed http(s) URL with a host, got %q", b.CredentialURL))
			}
			if cu.Scheme == "http" && !isLoopbackHostname(cu.Hostname()) {
				return fieldError(prefix+".credential_url",
					fmt.Sprintf("must be https for non-loopback hosts (the pull returns the org client_id/client_secret; plaintext http is allowed only for 127.0.0.1 / ::1 / localhost), got %q", b.CredentialURL))
			}
		}
		for j, sc := range b.ScopeCeiling {
			if strings.TrimSpace(sc) == "" {
				return fieldError(fmt.Sprintf("%s.scope_ceiling[%d]", prefix, j), "must be a non-empty scope string")
			}
		}
		if b.CacheTTL < 0 {
			return fieldError(prefix+".cache_ttl", "must be >= 0")
		}
		if b.Timeout < 0 {
			return fieldError(prefix+".timeout", "must be >= 0")
		}
		if authority := b.SignedOAuthMCPCapabilityAuthority; authority != nil {
			authorityPrefix := prefix + ".signed_oauth_mcp_capability_authority"
			if !authority.Enabled {
				return fieldError(authorityPrefix+".enabled",
					"must be true when signed_oauth_mcp_capability_authority is configured (omit the block to keep signed capability registration disabled)")
			}
			if strings.TrimSpace(authority.Issuer) == "" {
				return fieldError(authorityPrefix+".issuer", "must not be empty")
			}
			if (authority.JWKSURL == "") == (authority.JWKSFile == "") {
				return fieldError(authorityPrefix,
					"must set exactly one of jwks_url or jwks_file")
			}
			if authority.JWKSURL != "" {
				u, err := url.Parse(authority.JWKSURL)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return fieldError(authorityPrefix+".jwks_url",
						fmt.Sprintf("must be a well-formed http(s) URL with a host, got %q", authority.JWKSURL))
				}
				if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
					return fieldError(authorityPrefix+".jwks_url",
						fmt.Sprintf("must be https for non-loopback hosts, got %q", authority.JWKSURL))
				}
			}
			if authority.MaxAuthorityLifetime <= 0 {
				return fieldError(authorityPrefix+".max_authority_lifetime",
					"must be > 0 (signed authority envelopes require an explicit bounded lifetime)")
			}
		}
	}
	if (len(c.Tools.OAuthProviders) > 0 || len(c.Tools.OAuthCredentialBrokers) > 0) && c.Tools.OAuthTokenKEKEnv == "" {
		return fieldError("tools.oauth_token_kek_env",
			"must not be empty when tools.oauth_providers[] or tools.oauth_credential_brokers[] is set (names env var holding the 32-byte hex KEK for AES-256-GCM token encryption at rest; a Protocol-installed broker-pull provider shares the same token store; §7 rule 2)")
	}

	// MCP southbound OAuth binding + `_meta` annotations. Validated in a
	// second pass over the MCP servers because it cross-references the
	// `tools.oauth_providers[]` names built above. Each binding names a
	// declared provider (per-identity bearer injection), lives on an HTTP
	// transport, does NOT double up with a static `Authorization` header, and
	// carries no reserved / spec-prefixed / empty annotation key.
	for i, s := range c.Tools.MCPServers {
		prefix := fmt.Sprintf("tools.mcp_servers[%d]", i)
		mode := s.TransportMode
		if mode == "" {
			mode = "auto"
		}
		if s.OAuthProvider != "" {
			if _, ok := oauthProviderNames[s.OAuthProvider]; !ok {
				return fieldError(prefix+".oauth_provider",
					fmt.Sprintf("references unknown OAuth provider %q (declared providers: %s; declare via tools.oauth_providers[])",
						s.OAuthProvider, sortedKeysFromSet(oauthProviderNames)))
			}
			// The binding needs an HTTP request to inject into. An explicit
			// stdio transport is rejected, and so is ANY connection without
			// a url — an omitted/auto transport with only a command
			// auto-selects stdio at connect, which would silently skip
			// injection while the operator believes per-identity auth is on
			// (§13 silent degradation).
			if mode == "stdio" || s.URL == "" {
				return fieldError(prefix+".oauth_provider",
					"must not be set on a connection without an http(s) url (a stdio connection — explicit or auto-selected from a command-only config — carries no HTTP request to inject Authorization into; the binding is a misconfiguration)")
			}
			for k := range s.Headers {
				if strings.EqualFold(k, "authorization") {
					return fieldError(prefix+".headers",
						"must not carry a static \"Authorization\" header alongside oauth_provider (one auth mode per connection — the bearer is injected per-identity)")
				}
			}
			// Downstream-sink allow-list enforcement (the credential-plane
			// invariant). A provider that injects a bearer MUST
			// declare its allowed downstream hosts — an empty allow-list on
			// a bound provider is a fail-closed boot error. The connection's
			// own host must be listed (normalised, default-port
			// equivalence), so the bearer is never injected into an
			// undeclared sink.
			allowed := oauthProviderAllowedHosts[s.OAuthProvider]
			if len(allowed) == 0 {
				return fieldError(prefix+".oauth_provider",
					fmt.Sprintf("provider %q declares no allowed_downstream_hosts, but this connection binds it — a bearer-injecting provider must declare its downstream sinks (add allowed_downstream_hosts to tools.oauth_providers[]; the credential-plane invariant is fail-closed)", s.OAuthProvider))
			}
			connHost := NormalizeDownstreamHost(s.URL)
			if connHost == "" || !containsHost(allowed, connHost) {
				return fieldError(prefix+".url",
					fmt.Sprintf("connection host %q is not in provider %q's allowed_downstream_hosts %v — the bearer may only be injected into a boot-declared downstream sink", connHost, s.OAuthProvider, allowed))
			}
		}
		// `meta_annotations` keys are declared `_meta` PATHS (a dotted key
		// nests, exactly like `injection.meta_key`), so each key is validated
		// as a path — whole-key AND per-segment reserved guard, no empty
		// segment, depth-capped — and the declared set is checked for
		// collisions against itself and against the injection mapping's own
		// `_meta` path. One authority, shared by all four doors.
		annotationField := prefix + ".meta_annotations"
		for k := range s.MetaAnnotations {
			if err := ValidateMCPMetaAnnotationKey(k); err != nil {
				return fieldError(annotationField, err.Error())
			}
		}
		if err := ValidateMCPMetaPathCollisions(mapKeys(s.MetaAnnotations), injectionMetaPath(s.Injection)); err != nil {
			return fieldError(annotationField, err.Error())
		}
		// Per-tool oauth_provider overrides (CallTool granularity). Each entry
		// re-enforces exactly the same binding rules as the connection-level
		// oauth_provider (unknown name / stdio transport / static-Authorization
		// conflict / downstream-host allow-list), keyed by the MCP server-side
		// tool name. A tool_oauth_providers map on a connection without an
		// http(s) url is a misconfiguration (the same fail-closed reasoning as
		// the connection-level binding).
		for toolName, providerName := range s.ToolOAuthProviders {
			field := fmt.Sprintf("%s.tool_oauth_providers[%q]", prefix, toolName)
			if strings.TrimSpace(toolName) == "" {
				return fieldError(prefix+".tool_oauth_providers",
					"override key (tool name) must not be empty")
			}
			if strings.TrimSpace(providerName) == "" {
				return fieldError(field, "provider name must not be empty")
			}
			if _, ok := oauthProviderNames[providerName]; !ok {
				return fieldError(field,
					fmt.Sprintf("references unknown OAuth provider %q (declared providers: %s; declare via tools.oauth_providers[])",
						providerName, sortedKeysFromSet(oauthProviderNames)))
			}
			if mode == "stdio" || s.URL == "" {
				return fieldError(field,
					"must not be set on a connection without an http(s) url (a stdio connection carries no HTTP request to inject Authorization into)")
			}
			for k := range s.Headers {
				if strings.EqualFold(k, "authorization") {
					return fieldError(prefix+".headers",
						"must not carry a static \"Authorization\" header alongside tool_oauth_providers (one auth mode per connection — the bearer is injected per-identity)")
				}
			}
			allowed := oauthProviderAllowedHosts[providerName]
			if len(allowed) == 0 {
				return fieldError(field,
					fmt.Sprintf("provider %q declares no allowed_downstream_hosts, but this tool binds it — a bearer-injecting provider must declare its downstream sinks (add allowed_downstream_hosts to tools.oauth_providers[]; the credential-plane invariant is fail-closed)", providerName))
			}
			connHost := NormalizeDownstreamHost(s.URL)
			if connHost == "" || !containsHost(allowed, connHost) {
				return fieldError(field,
					fmt.Sprintf("connection host %q is not in provider %q's allowed_downstream_hosts %v — the bearer may only be injected into a boot-declared downstream sink", connHost, providerName, allowed))
			}
		}
		// Per-user credential INJECTION for a receiver-style server. The mapping
		// is non-secret; it names a declared broker and declares WHERE the
		// per-user pulled value is placed (header / Basic / `_meta`). It is
		// mutually exclusive with the bearer/oauth mode and a static
		// `Authorization` header (one auth mode per connection), needs an HTTP
		// request (the pulled credential leaves to a network host — the same
		// downstream-sink allow-list applies), and may only target a
		// redaction-covered key (fail-closed: the redactor must be able to hold
		// the injected value to `***`).
		if s.Injection != nil {
			inj := s.Injection
			field := prefix + ".injection"
			if s.OAuthProvider != "" {
				return fieldError(field,
					"must not be set alongside oauth_provider (one auth mode per connection — injection and bearer are mutually exclusive)")
			}
			if len(s.ToolOAuthProviders) > 0 {
				return fieldError(field,
					"must not be set alongside tool_oauth_providers (one auth mode per connection — injection and bearer are mutually exclusive)")
			}
			for k := range s.Headers {
				if strings.EqualFold(k, "authorization") {
					return fieldError(prefix+".headers",
						"must not carry a static \"Authorization\" header alongside injection (one auth mode per connection — the credential is injected per-identity)")
				}
			}
			if strings.TrimSpace(inj.Provider) == "" {
				return fieldError(field+".provider", "must name a declared tools.oauth_providers[] broker")
			}
			if _, ok := oauthProviderNames[inj.Provider]; !ok {
				return fieldError(field+".provider",
					fmt.Sprintf("references unknown OAuth provider %q (declared providers: %s; declare via tools.oauth_providers[])",
						inj.Provider, sortedKeysFromSet(oauthProviderNames)))
			}
			// The credential leaves to a network host: needs an http(s) url and
			// the connection host must be a boot-declared downstream sink of the
			// named broker (the credential-plane invariant, fail-closed).
			if mode == "stdio" || s.URL == "" {
				return fieldError(field,
					"must not be set on a connection without an http(s) url (a receiver-style server is reached over HTTP; the pulled credential is delivered on the outbound request)")
			}
			allowed := oauthProviderAllowedHosts[inj.Provider]
			if len(allowed) == 0 {
				return fieldError(field+".provider",
					fmt.Sprintf("provider %q declares no allowed_downstream_hosts, but this connection injects its credential — a credential-sourcing provider must declare its downstream sinks (add allowed_downstream_hosts to tools.oauth_providers[]; the credential-plane invariant is fail-closed)", inj.Provider))
			}
			connHost := NormalizeDownstreamHost(s.URL)
			if connHost == "" || !containsHost(allowed, connHost) {
				return fieldError(field+".provider",
					fmt.Sprintf("connection host %q is not in provider %q's allowed_downstream_hosts %v — the pulled credential may only be injected into a boot-declared downstream sink", connHost, inj.Provider, allowed))
			}
			switch inj.Form {
			case MCPInjectionFormHeader:
				if strings.TrimSpace(inj.Header) == "" {
					return fieldError(field+".header", "must name a target request header for form=header")
				}
				if strings.EqualFold(inj.Header, "authorization") {
					return fieldError(field+".header",
						"must not target the \"Authorization\" header (use form=basic for an Authorization: Basic value)")
				}
				if !IsReceiverInjectionCredentialKey(inj.Header) {
					return fieldError(field+".header",
						fmt.Sprintf("header %q is not a redaction-covered credential key — the audit redactor could not hold the injected value to *** (name it with a credential segment such as -api-key / -token / -secret so it is always redacted)", inj.Header))
				}
			case MCPInjectionFormBasic:
				// Authorization: Basic — the header key is the reserved
				// Authorization lane the redactor already covers; no target key
				// to validate. BasicUsername is non-secret and optional.
			case MCPInjectionFormMeta:
				if strings.TrimSpace(inj.MetaKey) == "" {
					return fieldError(field+".meta_key", "must name a target _meta key path for form=meta")
				}
				segs := SplitMCPMetaPath(inj.MetaKey)
				// The depth cap is the SAME cap the annotation path and the
				// wire door apply — one constant, every door. Before it was
				// hoisted it existed only at the wire door, so a boot-declared
				// over-deep path was accepted where the identical
				// wire-declared one was refused.
				if len(segs) > MaxMCPMetaKeyDepth {
					return fieldError(field+".meta_key",
						fmt.Sprintf("has %d path segments, exceeding the cap of %d — a credential nested that deep can push an audit payload past the redactor's deep-walk ceiling and turn every audit emit for this connection into a hard redaction failure", len(segs), MaxMCPMetaKeyDepth))
				}
				for _, seg := range segs {
					if strings.TrimSpace(seg) == "" {
						return fieldError(field+".meta_key", "must not contain an empty path segment")
					}
					if IsReservedMCPMetaKey(seg) {
						return fieldError(field+".meta_key",
							fmt.Sprintf("segment %q is reserved (tenant/user/session/agent_id/traceparent/tracestate and io.modelcontextprotocol/-prefixed keys are runtime-stamped)", seg))
					}
				}
				leaf := segs[len(segs)-1]
				if !IsReceiverInjectionCredentialKey(leaf) {
					return fieldError(field+".meta_key",
						fmt.Sprintf("leaf key %q is not a redaction-covered credential key — the audit redactor could not hold the injected value to *** (name the leaf with a credential segment such as api_key / token / secret)", leaf))
				}
			case "":
				return fieldError(field+".form", "must be set (header / basic / meta)")
			default:
				return fieldError(field+".form",
					fmt.Sprintf("unknown form %q (must be header / basic / meta)", inj.Form))
			}
		}
		// OAuth-requirement discovery cross-origin allowances: each
		// entry must be a well-formed https origin (scheme://host[:port], no
		// path). The runtime additionally refuses private-range / IP-literal
		// destinations at fetch time; validation catches the format typo.
		for j, o := range s.OAuthDiscoveryAllowedOrigins {
			field := fmt.Sprintf("%s.oauth_discovery_allowed_origins[%d]", prefix, j)
			if err := validateDiscoveryOrigin(o); err != nil {
				return fieldError(field, err.Error())
			}
		}
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
	// `skill_create_draft` (HA-62) — the draft-only personal-skill
	// proposer: a deliberate operator opt-in absent from every
	// recommended default, like skill_propose. It additionally requires
	// the composed LLM client at registration (the assembly threads it),
	// so a catalog listing it on an LLM-less runtime fails the boot loud.
	"skill_create_draft": {},
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
	"oauth2":        {},
	"tokenexchange": {},
}

// allowedCredentialSources mirrors the `internal/tools/auth/credsource`
// source registry. V1.11 ships `env` (boot-time process-env
// resolution, the default) and `remote` (coordinator-served pull). New
// sources under `internal/tools/auth/credsource/drivers/<name>/` add a
// row here in the same PR. Same duplication rationale as
// `allowedOAuthDrivers` — the `internal/config` package MUST NOT import a
// concrete driver package (§4.4). The credsource-package test
// `TestRegisteredSourcesMatchConfigAllowlist` asserts no drift between
// the two surfaces.
var allowedCredentialSources = map[string]struct{}{
	"env":    {},
	"remote": {},
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
	if c.Planner.MaxBatchSpawns < 0 {
		return fieldError("planner.max_batch_spawns",
			fmt.Sprintf("must be >= 0 (0 = use dev-runtime default of 5), got %d",
				c.Planner.MaxBatchSpawns))
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

// nativeBifrostProviders mirrors `bfschemas.StandardProviders` — the
// bifrost SDK's built-in (non-custom) provider list. It is the set
// `llm.provider` may name without a matching `llm.custom_providers`
// entry, and the set a custom-provider name may NOT collide with.
//
// It is a MIRROR, not a copy of convenience, and the distinction is the
// whole point: a hand-maintained mirror of an upstream list drifts
// silently on every dependency bump, so this one is not trusted to stay
// correct by discipline. `TestNativeBifrostProviders_LockstepWithSDK`
// enumerates `bfschemas.StandardProviders` and asserts set equality in
// BOTH directions, so a bump that adds or removes an upstream provider
// fails the build rather than mis-reporting at an operator's boot.
//
// Why the map is not simply DERIVED from `bfschemas` here, which would
// remove the drift structurally rather than detect it: importing the
// bifrost schemas package into this one takes the config package's
// dependency closure from 93 packages to roughly 310, pulling a JIT
// assembly JSON codec and a JSON-schema reflector into the package every
// binary and every embedder loads merely to parse `harbor.yaml`. The
// runtime side already resolves providers against the live list (the
// bifrost LLM driver enumerates `StandardProviders` directly), so the
// SDK is consulted where it is already linked; this validator holds a
// leaf-cheap mirror, and the test — which links the SDK anyway — is
// what keeps the two honest.
var nativeBifrostProviders = map[string]struct{}{
	"openai":         {},
	"azure":          {},
	"anthropic":      {},
	"bedrock":        {},
	"bedrock_mantle": {},
	"cohere":         {},
	"vertex":         {},
	"mistral":        {},
	"ollama":         {},
	"opencode-go":    {},
	"opencode-zen":   {},
	"groq":           {},
	"sgl":            {},
	"parasail":       {},
	"perplexity":     {},
	"cerebras":       {},
	"deepseek":       {},
	"gemini":         {},
	"openrouter":     {},
	"elevenlabs":     {},
	"huggingface":    {},
	"nebius":         {},
	"xai":            {},
	"replicate":      {},
	"vllm":           {},
	"runway":         {},
	"runware":        {},
	"fireworks":      {},
	"sarvam":         {},
	"wafer":          {},
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

// reservedMCPMetaAnnotationKeys is the closed set of `_meta` keys the runtime
// owns: the isolation triple, the agent-provenance stamp, and the W3C
// trace-context carrier keys (the `_meta` idiom). An operator's
// `meta_annotations` may not use them.
var reservedMCPMetaAnnotationKeys = map[string]struct{}{
	"tenant":      {},
	"user":        {},
	"session":     {},
	"agent_id":    {},
	"traceparent": {},
	"tracestate":  {},
}

// mcpSpecMetaAnnotationPrefix is the spec-reserved `_meta` namespace prefix;
// operator annotations may not use it.
const mcpSpecMetaAnnotationPrefix = "io.modelcontextprotocol/"

// IsReservedMCPMetaKey reports whether k is a runtime-reserved or
// spec-reserved MCP `_meta` key an operator annotation must not carry: the
// isolation triple keys (`tenant`/`user`/`session`), the agent-provenance
// stamp (`agent_id`), the W3C trace-context carrier keys
// (`traceparent`/`tracestate`), and any `io.modelcontextprotocol/`-prefixed
// key (the spec-reserved namespace). This is the SINGLE authority every
// surface that validates or merges MCP `_meta` annotations consults — config
// validation, the runtime add-connection validation, and the MCP driver's
// merge-time re-check all call it, so the reserved set cannot drift between
// them.
func IsReservedMCPMetaKey(k string) bool {
	if _, ok := reservedMCPMetaAnnotationKeys[k]; ok {
		return true
	}
	return strings.HasPrefix(k, mcpSpecMetaAnnotationPrefix)
}

// MaxMCPMetaKeyDepth caps the dot-segment count of a declared `_meta` key PATH
// — an operator annotation key AND a credential-injection `meta_key` alike, at
// every door that validates one.
//
// The cap exists so a declared path can never push an audit payload past the
// audit redactor's deep-walk ceiling (`audit.MaxDepth` = 64). The redactor
// FAILS LOUD past that ceiling (it returns a redaction-depth error rather than
// emitting anything), so an over-deep path does not leak — it turns every audit
// emit for that connection into a hard redaction failure. 16 leaves comfortable
// headroom for the `_meta` base depth plus any wrapper a payload adds around
// the injected map, while never constraining a real receiver key (which nests
// one or two segments, e.g. `vendor.api_key`).
//
// It lives here, in the package both the boot validator and the wire validator
// already depend on, because the reverse direction is an import cycle:
// `internal/runtime/agentcfg/protocol` imports this package, not the other way
// round. One constant, one value, every door.
const MaxMCPMetaKeyDepth = 16

// SplitMCPMetaPath splits a declared `_meta` key into its dot-separated path
// segments. A key with no `.` yields a single-segment path (a top-level `_meta`
// key). This is the SINGLE place the dot-as-path-separator convention is
// applied, so an annotation key and a credential-injection `meta_key` can never
// disagree about what a dotted key means.
func SplitMCPMetaPath(k string) []string {
	return strings.Split(k, ".")
}

// ReservedMCPMetaPathToken reports the reserved token in a declared `_meta` key
// PATH, and true when one is present.
//
// The check is WHOLE-KEY **and** PER-SEGMENT, in that order — a strict superset
// of `IsReservedMCPMetaKey` on both of that predicate's arms. Both arms are
// load-bearing and neither subsumes the other:
//
//   - The whole-key arm is the only one that can see the spec-reserved
//     `io.modelcontextprotocol/` namespace. Splitting
//     `io.modelcontextprotocol/ui` on `.` yields `["io",
//     "modelcontextprotocol/ui"]` — NEITHER segment carries the prefix, so a
//     per-segment-only check would ADMIT a spec-reserved key.
//   - The per-segment arm is the only one that can see a reserved key used as a
//     PATH component (`tenant.foo` nests under the runtime-owned `tenant`
//     node), which the whole-key arm reads as the unreserved literal
//     `"tenant.foo"`.
//
// The returned token is the offending whole key or segment, for an error
// message that names what was refused.
func ReservedMCPMetaPathToken(k string) (string, bool) {
	if IsReservedMCPMetaKey(k) {
		return k, true
	}
	for _, seg := range SplitMCPMetaPath(k) {
		if IsReservedMCPMetaKey(seg) {
			return seg, true
		}
	}
	return "", false
}

// ValidateMCPArtifactParams validates the bounded SHAPE of an egress-substitution
// artifact-parameter mapping: every tool name non-empty, every tool
// mapping at least one parameter, every parameter name non-empty, and no
// parameter named twice for one tool.
//
// It returns a bare, field-agnostic error every door wraps with its own
// sentinel + field prefix, exactly as [ValidateMCPMetaAnnotationKey]
// does — so the RULE has one implementation and the boot validator, the
// two Protocol persistence doors and the driver's attach differ only in
// how they report it.
//
// It validates and bounds SHAPE only. Two further rules are the caller's, because
// each needs state this function does not have: the eligibility and
// transport rules (the caller knows the connection), and the check that
// each mapped parameter is declared string-typed in the server's own
// DISCOVERED input schema (only the driver has seen the schema).
//
// A nil / empty mapping is valid — it is the carry-forward case, and a
// connection with no mapping takes an outbound path byte-identical to a
// build without the feature.
func ValidateMCPArtifactParams(params MCPArtifactParams) error {
	_, err := NormalizeMCPArtifactParams(params)
	return err
}

// NormalizeMCPArtifactParams returns the one canonical representation used by
// boot config, Protocol admission, signed-envelope comparison, persistence,
// and attach. Names are trimmed, parameters are sorted, and every resource
// ceiling is measured on that canonical representation.
func NormalizeMCPArtifactParams(params MCPArtifactParams) (MCPArtifactParams, error) {
	if len(params) == 0 {
		return nil, nil
	}
	if len(params) > MaxMCPArtifactMethods {
		return nil, fmt.Errorf("maps %d methods, exceeding the cap of %d", len(params), MaxMCPArtifactMethods)
	}
	canonical := make(MCPArtifactParams, len(params))
	for tool, names := range params {
		canonicalTool := strings.TrimSpace(tool)
		if canonicalTool == "" {
			return nil, errors.New("tool name must not be empty")
		}
		if len(canonicalTool) > MaxMCPArtifactNameBytes {
			return nil, fmt.Errorf("tool name is %d bytes, exceeding the cap of %d", len(canonicalTool), MaxMCPArtifactNameBytes)
		}
		if _, duplicate := canonical[canonicalTool]; duplicate {
			return nil, fmt.Errorf("maps canonical tool %q more than once", canonicalTool)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("tool %q maps no parameter names (remove the entry rather than declaring an empty one)", canonicalTool)
		}
		if len(names) > MaxMCPArtifactParamsPerMethod {
			return nil, fmt.Errorf("tool %q maps %d parameters, exceeding the cap of %d", canonicalTool, len(names), MaxMCPArtifactParamsPerMethod)
		}
		seen := make(map[string]struct{}, len(names))
		canonicalNames := make([]string, 0, len(names))
		for _, name := range names {
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				return nil, fmt.Errorf("tool %q maps an empty parameter name", canonicalTool)
			}
			if len(trimmed) > MaxMCPArtifactNameBytes {
				return nil, fmt.Errorf("tool %q parameter name is %d bytes, exceeding the cap of %d", canonicalTool, len(trimmed), MaxMCPArtifactNameBytes)
			}
			if _, dup := seen[trimmed]; dup {
				return nil, fmt.Errorf("tool %q maps parameter %q twice (must be unique)", canonicalTool, trimmed)
			}
			seen[trimmed] = struct{}{}
			canonicalNames = append(canonicalNames, trimmed)
		}
		sort.Strings(canonicalNames)
		canonical[canonicalTool] = canonicalNames
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("encode canonical artifact mapping: %w", err)
	}
	if len(encoded) > MaxMCPArtifactParamsJSONBytes {
		return nil, fmt.Errorf("canonical artifact mapping is %d bytes, exceeding the cap of %d", len(encoded), MaxMCPArtifactParamsJSONBytes)
	}
	return canonical, nil
}

// ValidateMCPMetaAnnotationKey validates ONE `meta_annotations` key as a
// declared `_meta` path: non-empty, no empty path segment, no reserved token
// (whole-key or per-segment, per ReservedMCPMetaPathToken), and no deeper than
// MaxMCPMetaKeyDepth segments. It returns a bare, field-agnostic error every
// door wraps with its own sentinel + field prefix, so the RULE has one
// implementation and the four doors differ only in how they report it.
func ValidateMCPMetaAnnotationKey(k string) error {
	if strings.TrimSpace(k) == "" {
		return errors.New("annotation key must not be empty")
	}
	segs := SplitMCPMetaPath(k)
	if len(segs) > MaxMCPMetaKeyDepth {
		return fmt.Errorf("annotation key %q has %d path segments, exceeding the cap of %d — a declared _meta path that deep can push an audit payload past the redactor's deep-walk ceiling and turn every audit emit for this connection into a hard redaction failure", k, len(segs), MaxMCPMetaKeyDepth)
	}
	for _, seg := range segs {
		if strings.TrimSpace(seg) == "" {
			return fmt.Errorf("annotation key %q must not contain an empty path segment", k)
		}
	}
	if tok, reserved := ReservedMCPMetaPathToken(k); reserved {
		return fmt.Errorf("annotation key %q is reserved: %q is a runtime-stamped or spec-reserved _meta key (tenant/user/session/agent_id/traceparent/tracestate and any io.modelcontextprotocol/-prefixed key; a dotted key is a PATH, so a reserved SEGMENT is refused too) — choose a non-reserved key", k, tok)
	}
	return nil
}

// ValidateMCPMetaPathCollisions rejects a connection whose declared `_meta`
// paths overlap. The declared set is every `meta_annotations` key plus the
// credential-injection `meta_key` when the connection injects into `_meta`
// (pass "" when it does not). No two declared paths may be EQUAL, and no
// declared path may be a proper PREFIX of another.
//
// Two reasons this rule is mandatory rather than a nicety:
//
//  1. Without it a colliding pair is resolved SILENTLY at merge time — the
//     nesting walk overwrites a non-map intermediate with no error and no log,
//     so a flat `vendor` annotation alongside `injection.meta_key:
//     vendor.api_key` discards the operator's annotation without telling
//     anyone.
//  2. With it, the merge is order-INDEPENDENT by construction. Distinct
//     non-prefixing paths write disjoint leaves, so the randomised iteration
//     order of the annotation map cannot change the merged result. Determinism
//     falls out of the validation rule; it does not need a sort at merge time.
//
// Keys are compared in sorted order so the error message is stable.
func ValidateMCPMetaPathCollisions(annotationKeys []string, injectionMetaKey string) error {
	declared := make([]string, 0, len(annotationKeys)+1)
	for _, k := range annotationKeys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		declared = append(declared, k)
	}
	if strings.TrimSpace(injectionMetaKey) != "" {
		declared = append(declared, injectionMetaKey)
	}
	if len(declared) < 2 {
		return nil
	}
	sort.Strings(declared)
	paths := make([][]string, len(declared))
	for i, k := range declared {
		paths[i] = SplitMCPMetaPath(k)
	}
	for i := range paths {
		for j := i + 1; j < len(paths); j++ {
			if mcpMetaPathsCollide(paths[i], paths[j]) {
				return fmt.Errorf("declared _meta paths %q and %q collide (equal, or one is a prefix of the other) — a dotted key is a PATH, so these two would write into the same node and one would silently overwrite the other; rename one of them", declared[i], declared[j])
			}
		}
	}
	return nil
}

// mapKeys returns m's keys (nil for an empty map). Order is unspecified;
// ValidateMCPMetaPathCollisions sorts what it is given.
func mapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// injectionMetaPath returns the connection's credential-injection `_meta` key
// path, or "" when the connection declares no injection or injects somewhere
// other than `_meta` (a header / an `Authorization: Basic` value writes no
// `_meta` node, so it cannot collide with an annotation path).
func injectionMetaPath(inj *MCPCredentialInjectionConfig) string {
	if inj == nil || strings.TrimSpace(inj.Form) != MCPInjectionFormMeta {
		return ""
	}
	return strings.TrimSpace(inj.MetaKey)
}

// mcpMetaPathsCollide reports whether a and b are equal or one is a proper
// prefix of the other — i.e. whether writing both would make one a scalar leaf
// and an intermediate node at once.
func mcpMetaPathsCollide(a, b []string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := range n {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// receiverInjectionCredentialSegments is the set of trailing key SEGMENTS that
// mark a header / `_meta` key as carrying a credential. It is the SINGLE
// authority both the receiver-injection mapping validation (which target keys an
// operator may declare) and the audit redactor's injection rule (which keys it
// holds to `***`) consult, so validation permits a target key IFF the redactor
// is guaranteed to redact it — the fail-closed guarantee that an injected
// credential can never reach an audit payload uncredacted. Bare `auth` is
// deliberately EXCLUDED (an `Authorization` header is covered by the dedicated
// authorization rule, and a plain `auth` key is often a non-secret sub-object);
// the tokens here are unambiguously credential-bearing.
var receiverInjectionCredentialSegments = map[string]struct{}{
	"key":        {},
	"apikey":     {},
	"token":      {},
	"secret":     {},
	"credential": {},
	"password":   {},
}

// IsReceiverInjectionCredentialKey reports whether a header or `_meta` leaf key
// is redaction-covered for receiver-style credential injection: it is true when
// the LAST `-`/`_`/`.`-separated segment of the key (case-insensitive) is a
// known credential token. Both the injection-mapping validation and the audit
// redactor's injection rule call it, so a target key an operator may declare is
// exactly a target key the redactor holds to `***` (fail-closed). Matching only
// the TRAILING segment keeps legitimate observability fields whose key merely
// CONTAINS a token word — `token_type`, `token_url`, `access_token_count` — out
// of the redaction net, while catching genuine credential keys. Examples:
// `x-vendor-api-key`, `x_github_token`, `vendor.api_key`, `user_password` →
// true; `x-request-id`, `content-type`, `token_url`, `token_type` → false.
func IsReceiverInjectionCredentialKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return false
	}
	segs := strings.FieldsFunc(k, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(segs) == 0 {
		return false
	}
	_, ok := receiverInjectionCredentialSegments[segs[len(segs)-1]]
	return ok
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

// ValidateLoopbackAddr reports whether addr is a loopback "host:port"
// suitable for the pprof debug listener. It is the single gate shared by
// server.debug_addr validation (config.Load) and the HARBOR_DEBUG_ADDR
// env override (cmd/harbor) so the two paths cannot diverge. A non-nil
// error means the address is malformed or not loopback — callers fail
// closed (CLAUDE.md §7: a profiler is never exposed off-box). Loopback
// is the numeric 127.0.0.0/8 or ::1 (matching the runtime's isLoopback
// check); a hostname like "localhost" is intentionally rejected.
func ValidateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port, got %q (%w)", addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("must be a loopback address (127.0.0.0/8 or ::1), got host %q — the pprof debug listener is never exposed off-box", host)
	}
	return nil
}

// isLoopbackHostname reports whether a URL hostname is loopback for the
// purposes of the credential-source TLS carve-out: a numeric loopback IP
// (127.0.0.0/8 or ::1) or the literal "localhost". Unlike
// ValidateLoopbackAddr (the pprof gate, which deliberately rejects
// hostnames), "localhost" is accepted here — the carve-out exists for
// the local fixture / dev case where the name is conventional. The
// `remote` credential-source driver mirrors this check at ValidateAtBoot
// (same duplication rationale as the driver allowlists — §4.4: config
// must not import driver packages).
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NormalizeDownstreamHost canonicalises a downstream-credential-sink host
// for allow-list comparison. It accepts either a bare `host[:port]` (an
// allow-list entry) or a full URL (a connection `url` — the scheme's
// default port is folded so `https://example.com` and
// `https://example.com:443` compare equal). The result is lowercased and a
// well-known default port (`:80`, `:443`) is stripped. It is the ONE
// normaliser both the config allow-list validation and the runtime
// `resolveOAuthBinding` check use so the two never diverge.
func NormalizeDownstreamHost(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// A full URL (has a scheme) → take its host[:port]; a bare host[:port]
	// is used as-is. url.Parse on a bare "host:port" mis-reads the host as
	// a scheme, so only parse when a "//" or a scheme delimiter is present.
	host := s
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			host = u.Host
		}
	}
	host = strings.ToLower(host)
	// Strip a well-known default port so default-port equivalence holds.
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port == "80" || port == "443" {
			return h
		}
		return net.JoinHostPort(h, port)
	}
	return host
}

// containsHost reports whether normalised host h is present in the
// normalised allow-list.
func containsHost(allow []string, h string) bool {
	for _, a := range allow {
		if a == h {
			return true
		}
	}
	return false
}

// validateDiscoveryOrigin is the unexported boot-config caller of
// [ValidateDiscoveryOrigin] — one implementation, two call sites (boot
// validation + the Protocol discovery-allowance write).
func validateDiscoveryOrigin(o string) error {
	return ValidateDiscoveryOrigin(o)
}

// ValidateDiscoveryOrigin checks that o is a well-formed OAuth-discovery
// cross-origin allowance: an https origin of the form scheme://host[:port]
// with no path, query, or fragment. It is intentionally permissive on
// host shape — the runtime enforces the private-range / IP-literal refusal at
// fetch time; this catches only the operator format typo pre-boot. It is the
// SINGLE origin validator shared by boot-config validation and the
// `agent_config.set_mcp_discovery_origins` Protocol write, so the two call
// sites can never drift (CLAUDE.md §17.6).
func ValidateDiscoveryOrigin(o string) error {
	trimmed := strings.TrimSpace(o)
	if trimmed == "" {
		return errors.New("origin must not be empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("must be a valid origin URL: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("origin must use the https scheme (scheme://host[:port])")
	}
	if u.Host == "" {
		return errors.New("origin must include a host (scheme://host[:port])")
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("origin must be scheme://host[:port] only — no path, query, or fragment")
	}
	// Reject IP-literal hosts fail-fast: an allowance names a PUBLIC origin,
	// never a bare IP (the runtime SSRF guard refuses private-range / IP-literal
	// destinations anyway; catching it at config load fails faster).
	if net.ParseIP(u.Hostname()) != nil {
		return errors.New("origin must be a hostname, not an IP literal (an allowance names a public origin; bare IPs are refused)")
	}
	return nil
}

// validateVirtualAgents validates the `virtual_agents:` boot block: a
// present block must name the owning top-level agent (the block is
// inert without one — there is no implicit owner), every profile must
// decode to a canonical profile whose parent equals the owner, and a
// duplicate key / invalid overlay fails boot LOUD (never a silent
// drop). The owner-vs-runtime-default-agent equality is checked at the
// run-loop boundary (the config package does not know the runtime's
// configured agent id); the canonical shape + bounds are enforced here.
func (c *Config) validateVirtualAgents() error {
	if c.VirtualAgents.IsZero() {
		return nil
	}
	if strings.TrimSpace(c.VirtualAgents.Owner) == "" {
		return fieldError("virtual_agents.owner", "must name the configured top-level agent that owns these profiles")
	}
	cap := c.VirtualAgents.MaxProfiles
	if cap <= 0 {
		cap = virtualagent.DefaultMaxProfiles
	}
	if len(c.VirtualAgents.Profiles) > cap {
		return fieldError("virtual_agents.profiles", fmt.Sprintf("profile count %d exceeds max_profiles %d", len(c.VirtualAgents.Profiles), cap))
	}
	if len(c.VirtualAgents.Profiles) == 0 {
		return fieldError("virtual_agents.profiles", "a virtual_agents block with no profiles is a misconfiguration (omit the block instead)")
	}
	seen := make(map[string]struct{}, len(c.VirtualAgents.Profiles))
	for i, p := range c.VirtualAgents.Profiles {
		key := strings.TrimSpace(p.Key)
		if key == "" {
			return fieldError(fmt.Sprintf("virtual_agents.profiles[%d].key", i), "profile key is required")
		}
		if _, dup := seen[key]; dup {
			return fieldError(fmt.Sprintf("virtual_agents.profiles[%d].key", i), fmt.Sprintf("duplicate profile key %q", key))
		}
		seen[key] = struct{}{}
		if _, err := p.ToProfile(c.VirtualAgents.Owner); err != nil {
			return fieldError(fmt.Sprintf("virtual_agents.profiles[%d]", i), err.Error())
		}
	}
	return nil
}
