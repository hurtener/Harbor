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
		c.validateDistributed,
		c.validateMemory,
		c.validateTools,
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
// 17 ships `inmem` + `fs`; Phase 18 adds `sqlite` and `postgres`;
// Phase 19 adds the S3-style driver. The validator only checks
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
	// Phase 21: backgroundtasks-config knobs. Defaults are applied in
	// `defaults()`; the validator rejects negative / zero values so an
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

// allowedDistributedBusDrivers is the V1 distributed bus driver
// allowlist. Phase 22 ships only `loopback`; post-V1 phase 86 adds
// durable backends (NATS / Redis Streams / Postgres-as-queue).
var allowedDistributedBusDrivers = map[string]struct{}{"loopback": {}}

// allowedDistributedRemoteDrivers is the V1 RemoteTransport driver
// allowlist. Phase 22 ships `loopback`; Phase 29 adds the `a2a` wire
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
	return nil
}

// allowedMemoryDrivers is the V1 memory-driver allowlist. Phase 23
// shipped `inmem`; Phase 25 adds `sqlite` and `postgres`.
var allowedMemoryDrivers = map[string]struct{}{
	"inmem":    {},
	"sqlite":   {},
	"postgres": {},
}

// memoryDriversRequiringDSN names the drivers whose `DSN` field must
// be non-empty. Phase 25's persistent drivers need explicit DSNs;
// `inmem` does not.
var memoryDriversRequiringDSN = map[string]struct{}{
	"sqlite":   {},
	"postgres": {},
}

// allowedMemoryStrategies is the V1 memory-strategy allowlist.
// Phase 24 added `truncation` and `rolling_summary` alongside the
// Phase 23 `none`. The allowlist tracks the operational set so an
// operator-set unsupported strategy is rejected at config
// validation rather than later at memory.Open — fail fast.
var allowedMemoryStrategies = map[string]struct{}{
	"none":            {},
	"truncation":      {},
	"rolling_summary": {},
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

// validateTools checks the Phase 26+ tools configuration: Phase 27's
// HTTP manifest paths + Phase 28's MCP servers. Later phases extend
// (Phase 29 A2A peers, Phase 30 OAuth token stores, etc.). The
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
	for i, p := range c.Tools.HTTPManifests {
		if strings.TrimSpace(p) == "" {
			return fieldError(fmt.Sprintf("tools.http_manifests[%d]", i),
				"path must not be empty")
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
	}
	// Phase 29 A2A peers. Empty list is valid. Each entry must
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
