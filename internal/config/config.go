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

// TasksConfig configures the TaskRegistry driver. `Driver` selects
// the registered driver name; Phase 20 ships only `"inprocess"`.
// Phase 21 will extend this with retain-turn timeout + continuation
// hop limit; do not add those fields here. Restart-required (no
// `reload:"live"` tag).
type TasksConfig struct {
	Driver string `yaml:"driver"`
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
// the tool catalog; the field is the runtime-wide default.
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
// in-memory ring buffer that backs Replayer. Later phases
// (durable-log Phase 57) register additional driver names without
// changing the field shape.
//
// ReplayBufferSize=0 disables replay entirely on the inmem driver
// (Replay returns ErrReplayUnavailable immediately). The default
// applied by Load is 10000.
type EventsConfig struct {
	Driver                   string        `yaml:"driver"`
	MaxSubscribersPerSession int           `yaml:"max_subscribers_per_session"`
	SubscriberBufferSize     int           `yaml:"subscriber_buffer_size"`
	IdleTimeout              time.Duration `yaml:"idle_timeout"`
	DropWindow               time.Duration `yaml:"drop_window"`
	ReplayBufferSize         int           `yaml:"replay_buffer_size"`
}

// AuditConfig is owned by Phase 03 + later audit phases.
type AuditConfig struct{}

// ProtocolConfig is owned by the protocol-server phases.
type ProtocolConfig struct{}

// CLIConfig is owned by the CLI phases.
type CLIConfig struct{}
