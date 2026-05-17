package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Sentinel errors. Callers compare against these via errors.Is.
var (
	// ErrConfigInvalid wraps any failure to parse, override, or
	// validate a configuration source. Callers should errors.Is on
	// this sentinel to distinguish "config layer rejected the input"
	// from upstream filesystem / IO errors.
	ErrConfigInvalid = errors.New("config: invalid configuration")
	// ErrConfigNotFound is returned when Load is given a path that
	// does not exist. It wraps the originating fs error so callers
	// can still errors.Is(err, fs.ErrNotExist).
	ErrConfigNotFound = errors.New("config: file not found")
)

// envPrefix is the env-var override prefix per the brief 06 layering
// rule: HARBOR_<SECTION>_<FIELD> (case-insensitive on the right of
// the prefix, single-level nesting). Two-level nesting is supported
// by joining sub-paths with another underscore.
const envPrefix = "HARBOR_"

// LoadOption customises a Load / LoadFromBytes call. Options compose
// in declaration order; later options override earlier ones for the
// same setting.
type LoadOption func(*loadConfig)

// loadConfig is the internal bundle a chain of LoadOption populates.
type loadConfig struct {
	logger *slog.Logger
}

// resolveLoadConfig walks the options chain and fills defaults for
// any setting an option did not supply.
func resolveLoadConfig(opts []LoadOption) loadConfig {
	cfg := loadConfig{logger: slog.Default()}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}
	return cfg
}

// WithLogger overrides the slog.Logger the config loader emits
// structured warnings on (e.g. the `config.deprecated_field` warning
// surfaced when a removed YAML key appears in a config — D-081). A
// nil logger keeps the default (`slog.Default()`); callers that want
// to capture the warnings in a test build a logger over a bytes
// buffer and pass it here.
func WithLogger(l *slog.Logger) LoadOption {
	return func(c *loadConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// Load reads a YAML configuration file at path, applies
// HARBOR_-prefixed environment overrides, runs Validate, and returns
// an immutable *Config. The returned error is wrapped under either
// ErrConfigNotFound (if the file is missing) or ErrConfigInvalid
// (parse / override / validate failure).
//
// Options customise the load (e.g. WithLogger to redirect the
// deprecation-warning surface). No-option calls log via slog.Default().
func Load(ctx context.Context, path string, opts ...LoadOption) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s: %w", ErrConfigNotFound, path, err)
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := loadFromBytesNamed(ctx, data, path, opts...)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadFromBytes parses raw YAML bytes (typically from tests). It
// applies the same env-var overrides and validation pipeline as Load,
// but does not record a filesystem source — error messages will
// include "(source: <bytes>)" instead of a path. Options mirror Load.
func LoadFromBytes(ctx context.Context, data []byte, opts ...LoadOption) (*Config, error) {
	return loadFromBytesNamed(ctx, data, "<bytes>", opts...)
}

// loadFromBytesNamed is the shared implementation. The name is used
// only for error messages; it has no effect on parsing.
func loadFromBytesNamed(ctx context.Context, data []byte, source string, opts ...LoadOption) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lc := resolveLoadConfig(opts)
	// Strip deprecated governance keys from the byte stream BEFORE the
	// strict decode would reject them. Each stripped key emits a single
	// `config.deprecated_field` slog.Warn (D-081). Operators migrating
	// from a pre-Phase-36a config rebuild the values under
	// `governance.identity_tiers`.
	cleaned, err := stripDeprecatedGovernanceKeys(data, source, lc.logger)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: parse: %w", ErrConfigInvalid, source, err)
	}
	cfg := defaults()
	if err := yaml.UnmarshalWithOptions(cleaned, cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%w: %s: parse: %w", ErrConfigInvalid, source, err)
	}
	cfg.source = source
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("%w: %s: env override: %w", ErrConfigInvalid, source, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfigInvalid, err)
	}
	return cfg, nil
}

// WithOverrides applies a flat key->string override map to a
// previously-loaded *Config and re-validates. Keys are dotted paths
// matching the YAML field names ("server.bind_addr", "llm.model").
// This is the seam for CLI flag layering (Phase 64) and Console
// pushed config (post-V1); Phase 02 ships only the mechanism.
func WithOverrides(c *Config, overrides map[string]string) (*Config, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: WithOverrides called with nil *Config", ErrConfigInvalid)
	}
	clone := *c
	for key, val := range overrides {
		if err := setByPath(&clone, splitPath(key), val); err != nil {
			return nil, fmt.Errorf("%w: override %s: %w", ErrConfigInvalid, key, err)
		}
	}
	if err := clone.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfigInvalid, err)
	}
	return &clone, nil
}

// defaults returns a *Config pre-populated with the documented
// non-security defaults. Security-relevant fields (JWT algorithms,
// audit redaction patterns) are intentionally absent so Validate
// fails loudly when an operator omits them.
func defaults() *Config {
	return &Config{
		Server: ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Telemetry: TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor",
		},
		State: StateConfig{
			Driver: "inmem",
		},
		LLM: LLMConfig{
			// Phase 64 / D-089 flipped the default from "mock" to
			// "bifrost". A config with an empty `llm.driver` now
			// defaults to the production driver — missing config keys
			// fail loud at registry resolution (mock isn't blank-
			// imported in cmd/harbor/main.go, so it's not in the
			// process registry) rather than silently routing through
			// a stub. The §13 "test stubs as production defaults"
			// amendment is closed for the LLM seam.
			Driver:               "bifrost",
			Timeout:              60 * time.Second,
			ContextWindowReserve: 0.05, // 5% safety margin (Phase 32 / D-026)
			Corrections: LLMCorrectionsConfig{
				// Phase 34 — corrections enabled by default. Operators
				// who omit the field get the production behaviour.
				// `*bool` distinguishes "operator didn't set" (nil →
				// loader fills with true) from "operator explicitly
				// disabled" (*false). The loader's `defaults()` runs
				// BEFORE yaml merge, so the explicit `false` from yaml
				// survives.
				Enabled: boolPtr(true),
			},
		},
		Governance: GovernanceConfig{
			RepairAttempts: 3,
		},
		Events: EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     256,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         10000,
		},
		Sessions: SessionsConfig{
			IdleTTL:       24 * time.Hour,
			HardCap:       720 * time.Hour,
			SweepInterval: 15 * time.Minute,
		},
		Artifacts: ArtifactsConfig{
			Driver:                    "inmem",
			FSRoot:                    "",
			HeavyOutputThresholdBytes: 32 * 1024,
			// Phase 19: S3-style driver defaults. Region defaults to
			// us-east-1 (covers MinIO + plain R2); UsePathStyle
			// defaults to false (AWS native — operators flip on for
			// MinIO / older R2 endpoints).
			S3Region:       "us-east-1",
			S3UsePathStyle: false,
		},
		Tasks: TasksConfig{
			Driver:               "inprocess",
			RetainTurnTimeout:    5 * time.Minute,
			ContinuationHopLimit: 8,
		},
		Distributed: DistributedConfig{
			BusDriver:    "loopback",
			RemoteDriver: "loopback",
		},
		Memory: MemoryConfig{
			Driver:             "inmem",
			Strategy:           "none",
			RecoveryBacklogMax: 16,
		},
		// D-103 — closes issue #126. The V1 planner-driver default is
		// "react" (the reference LLM-driven ReAct concrete — Phase 45 /
		// D-051). A config with an empty `planner.driver` boots with
		// the reference planner unchanged; operators opt into
		// alternates explicitly when later phases land them (Plan-
		// Execute, Workflow, Graph, Deterministic, Supervisor,
		// MultiAgent, HumanApproval per RFC §6.2). MaxSteps=0 means
		// "use the driver's internal default" (react.DefaultMaxSteps
		// = 12).
		Planner: PlannerConfig{
			Driver: "react",
		},
		CLI: CLIConfig{
			// Phase 65 / D-099 — `harbor dev` hot-reload defaults. The
			// block is opt-out: Enabled defaults to true; the `--no-hot-
			// reload` CLI flag is the operator-facing escape hatch.
			// Policy `drain` waits for in-flight RunLoops to finish up to
			// DrainTimeout before restarting. WatchRoots defaults to the
			// Phase 66 project-local drafts directory.
			DevHotReload: DevHotReloadConfig{
				Enabled:      boolPtr(true),
				Policy:       DevHotReloadPolicyDrain,
				DrainTimeout: 5 * time.Second,
				WatchRoots:   []string{".harbor/agents"},
			},
		},
	}
}

// boolPtr returns a pointer to b. Used by `defaults()` to populate
// pointer-bool fields (e.g. `LLMCorrectionsConfig.Enabled`) where the
// loader needs to distinguish "operator didn't set" from "operator
// set false."
func boolPtr(b bool) *bool { return &b }

// applyEnvOverrides walks every leaf field of *Config and, when the
// corresponding HARBOR_<PATH> env var is set, parses and applies it.
// Unset env vars are no-ops (zero or default value remains). Slice
// fields accept comma-separated values.
func applyEnvOverrides(cfg *Config) error {
	v := reflect.ValueOf(cfg).Elem()
	return walkLeaves(v, nil, func(path []string, leaf reflect.Value) error {
		envName := envPrefix + strings.ToUpper(strings.Join(path, "_"))
		raw, ok := os.LookupEnv(envName)
		if !ok {
			return nil
		}
		if err := setLeaf(leaf, raw); err != nil {
			return fmt.Errorf("config.%s: %w", strings.Join(path, "."), err)
		}
		return nil
	})
}

// setByPath resolves a dotted key path against *Config and sets the
// leaf value. Used by WithOverrides.
func setByPath(cfg *Config, path []string, raw string) error {
	v := reflect.ValueOf(cfg).Elem()
	for i, segment := range path {
		field, ok := findFieldByYAML(v, segment)
		if !ok {
			return fmt.Errorf("unknown key segment %q at depth %d", segment, i)
		}
		v = field
	}
	if !v.CanSet() {
		return fmt.Errorf("path is not settable")
	}
	return setLeaf(v, raw)
}

// splitPath turns "server.bind_addr" into ["server", "bind_addr"].
func splitPath(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, ".")
}

// findFieldByYAML returns the field of struct v whose yaml tag (name
// portion) matches segment.
func findFieldByYAML(v reflect.Value, segment string) (reflect.Value, bool) {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}, false
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		name := yamlName(t.Field(i))
		if name == segment {
			return v.Field(i), true
		}
	}
	return reflect.Value{}, false
}

// walkLeaves descends struct v, invoking visit on every primitive
// leaf with the dotted path of yaml names. Empty reserved sub-structs
// (no exported fields) are skipped. Unexported fields are skipped.
func walkLeaves(v reflect.Value, prefix []string, visit func(path []string, leaf reflect.Value) error) error {
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
		switch fv.Kind() {
		case reflect.Struct:
			// time.Duration is an int64 alias, but the typical leaf
			// case here is real sub-structs. Recurse.
			if err := walkLeaves(fv, path, visit); err != nil {
				return err
			}
		default:
			if err := visit(path, fv); err != nil {
				return err
			}
		}
	}
	return nil
}

// yamlName returns the field's yaml name (the part before the first
// comma in the tag), or the lowercased Go field name as a fallback.
// Returns "-" verbatim so the caller can suppress those fields.
func yamlName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	if comma := strings.IndexByte(tag, ','); comma >= 0 {
		return tag[:comma]
	}
	return tag
}

// setLeaf parses raw and assigns it to the typed leaf value. Supports
// string, bool, int (all sizes), float (32/64), time.Duration, and
// []string (comma-separated).
func setLeaf(leaf reflect.Value, raw string) error {
	if !leaf.CanSet() {
		return fmt.Errorf("leaf is not settable")
	}
	switch leaf.Kind() {
	case reflect.String:
		leaf.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("parse bool %q: %w", raw, err)
		}
		leaf.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		// time.Duration is an int64 — treat it specially.
		if leaf.Type() == reflect.TypeOf(time.Duration(0)) {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("parse duration %q: %w", raw, err)
			}
			leaf.SetInt(int64(d))
			return nil
		}
		n, err := strconv.ParseInt(raw, 10, leaf.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse int %q: %w", raw, err)
		}
		leaf.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, leaf.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse uint %q: %w", raw, err)
		}
		leaf.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, leaf.Type().Bits())
		if err != nil {
			return fmt.Errorf("parse float %q: %w", raw, err)
		}
		leaf.SetFloat(f)
	case reflect.Slice:
		if leaf.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported slice element kind %s", leaf.Type().Elem().Kind())
		}
		parts := splitCSV(raw)
		out := reflect.MakeSlice(leaf.Type(), len(parts), len(parts))
		for i, p := range parts {
			out.Index(i).SetString(p)
		}
		leaf.Set(out)
	default:
		return fmt.Errorf("unsupported leaf kind %s", leaf.Kind())
	}
	return nil
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
