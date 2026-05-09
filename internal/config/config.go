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
	Server     ServerConfig     `yaml:"server"`
	Identity   IdentityConfig   `yaml:"identity"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	State      StateConfig      `yaml:"state"`
	LLM        LLMConfig        `yaml:"llm"`
	Governance GovernanceConfig `yaml:"governance"`

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
	LogFormat   string `yaml:"log_format"`
	LogLevel    string `yaml:"log_level"`
	OTelEndpoint string `yaml:"otel_endpoint,omitempty"`
	ServiceName string `yaml:"service_name"`
}

// StateConfig selects the StateStore driver and its connection.
type StateConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn,omitempty" secret:"true"`
}

// LLMConfig is the default LLM client surface for the runtime.
type LLMConfig struct {
	Provider string        `yaml:"provider"`
	Model    string        `yaml:"model"`
	APIKey   string        `yaml:"api_key" secret:"true"`
	BaseURL  string        `yaml:"base_url,omitempty"`
	Timeout  time.Duration `yaml:"timeout"`
}

// GovernanceConfig holds the V1 governance policy surface (cost
// ceilings, rate limits, default MaxTokens). Hot-reload candidates
// (CostCeilingUSD, RateLimitTPS) will opt in via `reload:"live"` when
// the governance subsystem ships its hot-reload code path; not in V1.
type GovernanceConfig struct {
	DefaultMaxTokens int     `yaml:"default_max_tokens"`
	CostCeilingUSD   float64 `yaml:"cost_ceiling_usd,omitempty"`
	RateLimitTPS     float64 `yaml:"rate_limit_tps,omitempty"`
	RepairAttempts   int     `yaml:"repair_attempts"`
}

// Reserved sub-structs. Each owning phase will populate fields and
// validators. Until then the struct is zero-valued and the loader
// passes them through unchanged.

// RuntimeConfig is owned by runtime/* phases (engine, streaming,
// cancellation, backpressure).
type RuntimeConfig struct{}

// MemoryConfig is owned by the memory subsystem phases.
type MemoryConfig struct{}

// SkillsConfig is owned by the skills subsystem phases.
type SkillsConfig struct{}

// TasksConfig is owned by the tasks/scheduler phases.
type TasksConfig struct{}

// SessionsConfig is owned by the session lifecycle phases.
type SessionsConfig struct{}

// ArtifactsConfig is owned by the artifact-store phases.
type ArtifactsConfig struct{}

// EventsConfig is owned by the event-bus phases.
type EventsConfig struct{}

// AuditConfig is owned by Phase 03 + later audit phases.
type AuditConfig struct{}

// ProtocolConfig is owned by the protocol-server phases.
type ProtocolConfig struct{}

// CLIConfig is owned by the CLI phases.
type CLIConfig struct{}
