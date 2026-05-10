package config

import (
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
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
func (c *Config) Validate() error {
	validators := []func() error{
		c.validateServer,
		c.validateIdentity,
		c.validateTelemetry,
		c.validateState,
		c.validateLLM,
		c.validateGovernance,
		c.validateEvents,
		c.validateSessions,
		c.validateArtifacts,
		c.validateTasks,
	}
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

func (c *Config) validateLLM() error {
	if c.LLM.Provider == "" {
		return fieldError("llm.provider", "must not be empty")
	}
	if c.LLM.Model == "" {
		return fieldError("llm.model", "must not be empty")
	}
	if c.LLM.APIKey == "" {
		return fieldError("llm.api_key", "must not be empty")
	}
	if c.LLM.Timeout <= 0 {
		return fieldError("llm.timeout", "must be > 0")
	}
	return nil
}

func (c *Config) validateGovernance() error {
	if c.Governance.DefaultMaxTokens <= 0 {
		return fieldError("governance.default_max_tokens", "must be > 0")
	}
	if c.Governance.RepairAttempts < 0 {
		return fieldError("governance.repair_attempts", "must be >= 0")
	}
	if c.Governance.CostCeilingUSD < 0 {
		return fieldError("governance.cost_ceiling_usd", "must be >= 0 (omit to disable)")
	}
	if c.Governance.RateLimitTPS < 0 {
		return fieldError("governance.rate_limit_tps", "must be >= 0 (omit to disable)")
	}
	return nil
}

var allowedEventDrivers = map[string]struct{}{"inmem": {}}

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

// allowedArtifactsDrivers is the V1 artifacts-driver allowlist. Phase
// 18 will add `sqlite-blob` and `postgres-blob`; Phase 19 adds an
// S3-style driver. The validator only checks shape; the registry
// surfaces the matching factory at Open time.
var allowedArtifactsDrivers = map[string]struct{}{"inmem": {}, "fs": {}}

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
	if c.Artifacts.HeavyOutputThresholdBytes < 0 {
		return fieldError("artifacts.heavy_output_threshold_bytes", "must be >= 0")
	}
	return nil
}

// allowedTasksDrivers is the V1 tasks-driver allowlist. Phase 20
// ships only `inprocess`; later post-V1 phases (e.g. a durable
// queue-backed driver) extend this list.
var allowedTasksDrivers = map[string]struct{}{"inprocess": {}}

func (c *Config) validateTasks() error {
	if c.Tasks.Driver == "" {
		return fieldError("tasks.driver", "must not be empty")
	}
	if _, ok := allowedTasksDrivers[c.Tasks.Driver]; !ok {
		return fieldError("tasks.driver",
			fmt.Sprintf("must be one of %s, got %q",
				sortedKeys(allowedTasksDrivers), c.Tasks.Driver))
	}
	return nil
}

// fieldError formats a validation error with the offending path so
// the operator can grep for the key in their YAML.
func fieldError(path, reason string) error {
	return fmt.Errorf("config.%s: %s", path, reason)
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

// LiveReloadable returns dotted YAML paths for every field tagged
// `reload:"live"`. Phase 02 ships zero live fields so this returns
// an empty slice; later phases that opt in extend it automatically.
func (c *Config) LiveReloadable() []string {
	var paths []string
	v := reflect.ValueOf(c).Elem()
	collectLiveReload(v, nil, &paths)
	return paths
}

func collectLiveReload(v reflect.Value, prefix []string, out *[]string) {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := yamlName(f)
		if name == "" || name == "-" {
			continue
		}
		path := append(prefix, name)
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
