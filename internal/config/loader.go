package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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

// envPrefix is the env-var override prefix per the layering
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
// surfaced when a removed YAML key appears in a config). A
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
	cfg, err := loadFromBytesNamed(ctx, data, path, filepath.Dir(path), opts...)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadFromBytes parses raw YAML bytes (typically from tests). It
// applies the same env-var overrides and validation pipeline as Load,
// but does not record a filesystem source — error messages will
// include "(source: <bytes>)" instead of a path. Options mirror Load.
//
// Because there is no source file, `tools.http_manifests` relative
// entries and `skills.boot_agent_packs[].directory` relative values
// are NOT resolved against a config directory here (there is none) —
// they pass through unresolved, exactly like a hand-built `*Config` an
// embedder constructs in Go, so the later boot loader can fail loud
// rather than resolving against the process CWD.
func LoadFromBytes(ctx context.Context, data []byte, opts ...LoadOption) (*Config, error) {
	return loadFromBytesNamed(ctx, data, "<bytes>", "", opts...)
}

// LoadFromBytesAt parses raw YAML bytes the caller already read from
// path, without re-reading the file. It applies the same env-var
// overrides, `tools.http_manifests` relative-path resolution (against
// `filepath.Dir(path)`, §7 rule 5), `skills.boot_agent_packs`
// relative-directory resolution (the same provenance — the config
// file's directory, never CWD), and validation pipeline as Load —
// the only difference from Load is that the caller supplies the bytes
// instead of this function reading them. Error messages record path
// as the source, exactly as Load's do.
//
// Intended for callers that need the raw bytes for their own purposes
// (e.g. `harbor validate`'s YAML-AST-based line lookup) and would
// otherwise have to read the file twice to get Load's path-aware
// behavior.
//
// An empty path degrades to LoadFromBytes semantics: no config
// directory is derived (`filepath.Dir("")` would be "." — the process
// CWD, which is NOT the config file's directory), so relative
// `tools.http_manifests` entries and relative
// `skills.boot_agent_packs` directories pass through unresolved and
// error messages record "<bytes>" as the source.
func LoadFromBytesAt(ctx context.Context, data []byte, path string, opts ...LoadOption) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return LoadFromBytes(ctx, data, opts...)
	}
	return loadFromBytesNamed(ctx, data, path, filepath.Dir(path), opts...)
}

// loadFromBytesNamed is the shared implementation. The name is used
// only for error messages; it has no effect on parsing. configDir is
// the directory `tools.http_manifests` relative entries and
// `skills.boot_agent_packs` relative directories resolve against;
// empty when there is no real config-file source (LoadFromBytes).
func loadFromBytesNamed(ctx context.Context, data []byte, source, configDir string, opts ...LoadOption) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lc := resolveLoadConfig(opts)
	// Strip deprecated governance keys from the byte stream BEFORE the
	// strict decode would reject them. Each stripped key emits a single
	// `config.deprecated_field` slog.Warn. Operators migrating
	// from a pre-tiered config rebuild the values under
	// `governance.identity_tiers`.
	cleaned, err := stripDeprecatedGovernanceKeys(data, source, lc.logger)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: parse: %w", ErrConfigInvalid, source, err)
	}
	cfg := Defaults()
	if err := yaml.UnmarshalWithOptions(cleaned, cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("%w: %s: parse: %w", ErrConfigInvalid, source, err)
	}
	cfg.source = source
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, fmt.Errorf("%w: %s: env override: %w", ErrConfigInvalid, source, err)
	}
	if err := resolveHTTPManifestPaths(cfg, configDir); err != nil {
		// Wrapped through cfg.wrapValidationError so the message shape
		// ("config.<path>: <reason> (source: <name>)") matches every
		// other semantic-validation error — callers that parse the
		// field path out of the error string (e.g. `harbor validate`'s
		// classifyLoaderError) don't need a second code path for this
		// one check.
		return nil, fmt.Errorf("%w: %w", ErrConfigInvalid, cfg.wrapValidationError(err))
	}
	if err := resolveBootAgentPackDirectories(cfg, configDir); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfigInvalid, cfg.wrapValidationError(err))
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfigInvalid, err)
	}
	return cfg, nil
}

// resolveHTTPManifestPaths normalizes each `tools.http_manifests`
// entry against configDir (CLAUDE.md §7 rule 5). Replicates the
// `internal/skills/importer/path_safety.go` posture (Clean + Join +
// canonical-prefix check, then a symlink-evaluation pass) rather than
// importing it — `internal/config` must not grow a dependency on the
// skills subsystem.
//
// An empty configDir (LoadFromBytes / a hand-built *Config) is a
// no-op: relative entries pass through unresolved, matching every
// other operator-declared path an embedder sets by hand.
//
// An ABSOLUTE entry is `filepath.Clean`ed and accepted unconditionally
// — the documented `/etc/harbor/tools/*.yaml` deployment shape, the
// same trust posture as `artifacts.fs_root`. A RELATIVE entry is
// resolved against configDir; one that lexically escapes it is a loud
// `fieldError` naming `tools.http_manifests[i]`. Symlinks INSIDE the
// directory are followed but never crossed to a destination outside
// it: when the joined path exists, `filepath.EvalSymlinks` re-checks
// containment against the symlink-resolved directory; when it does
// not exist (legitimate — the validator is I/O-free and boot is the
// existence-enforcement home), the lexical check carries alone. An
// empty-string entry is left untouched so `validateTools` reports the
// clean "must not be empty" message rather than a confusing
// path-escape one.
func resolveHTTPManifestPaths(cfg *Config, configDir string) error {
	if configDir == "" || len(cfg.Tools.HTTPManifests) == 0 {
		return nil
	}
	canonicalDir, err := filepath.Abs(filepath.Clean(configDir))
	if err != nil {
		return fmt.Errorf("tools.http_manifests: resolve config directory %q: %w", configDir, err)
	}
	resolved := make([]string, len(cfg.Tools.HTTPManifests))
	for i, raw := range cfg.Tools.HTTPManifests {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			resolved[i] = raw
			continue
		}
		if filepath.IsAbs(trimmed) {
			resolved[i] = filepath.Clean(trimmed)
			continue
		}
		joined := filepath.Clean(filepath.Join(canonicalDir, trimmed))
		if !pathHasPrefixWithinRoot(joined, canonicalDir) {
			return fieldError(fmt.Sprintf("tools.http_manifests[%d]", i),
				fmt.Sprintf("%q escapes the config directory %q", trimmed, canonicalDir))
		}
		// Symlink-evaluation pass (the second half of the
		// path_safety.go posture). EvalSymlinks errors when any
		// component does not exist; that's fine — the manifest may
		// legitimately not exist yet at Load time, and the lexical
		// check above already carried. When the path DOES resolve,
		// a symlink inside the config directory pointing outside it
		// (e.g. cfgdir/evil -> /etc, entry "evil/passwd") is caught
		// here.
		if evaluated, evalErr := filepath.EvalSymlinks(joined); evalErr == nil {
			evaluatedDir, dirErr := filepath.EvalSymlinks(canonicalDir)
			if dirErr != nil {
				// The directory must be resolvable if a path under it
				// just was; surface the error rather than silently
				// skipping the containment check.
				return fmt.Errorf("tools.http_manifests: resolve config directory %q: %w", canonicalDir, dirErr)
			}
			if !pathHasPrefixWithinRoot(evaluated, evaluatedDir) {
				return fieldError(fmt.Sprintf("tools.http_manifests[%d]", i),
					fmt.Sprintf("%q escapes the config directory %q via symlink (resolves to %q)",
						trimmed, evaluatedDir, evaluated))
			}
		}
		resolved[i] = joined
	}
	cfg.Tools.HTTPManifests = resolved
	return nil
}

// resolveBootAgentPackDirectories resolves each
// `skills.boot_agent_packs[].directory` against configDir — the loaded
// config file's directory, NEVER the process CWD — using the same loader
// provenance pattern `resolveHTTPManifestPaths` establishes (CLAUDE.md §7
// rule 5, the skills-importer path-safety posture).
//
// Unlike the HTTP-manifest resolver this pass is PURELY LEXICAL and
// performs NO filesystem reads: no filepath.EvalSymlinks re-check. The
// `skills.boot_agent_packs` declaration is a boot-time file SOURCE whose
// directory may legitimately not exist yet at Load time, and the phase
// contract pins validation/normalization to zero filesystem reads — the
// lexical Clean+prefix containment check is the §7 rule 5 half that
// matters for a not-yet-existing path, and the boot resolver owns
// existence + symlink checks on its own read path.
//
// An empty configDir (LoadFromBytes / a hand-built *Config) is a no-op:
// relative directories pass through UNRESOLVED, retaining their explicit
// relative state so the later boot loader can fail loud rather than
// silently resolving against the process CWD. An ABSOLUTE directory is
// `filepath.Clean`ed and accepted unconditionally — the documented
// `/etc/harbor/skills` operator deployment shape, the same trust posture
// as `tools.http_manifests` absolute entries.
//
// The RAW directory shape is enforced HERE, BEFORE any Clean/Join
// normalization, via the same shared helper the validator uses
// (validateBootAgentPackDirectoryShape): an empty or whitespace-
// surrounded value is rejected outright, and the rune ceiling applies
// to the raw value — never to a trimmed or Clean-ed copy. filepath.Clean
// collapses an over-bound `a/../` path below the ceiling and Join
// resolves a relative value against the config directory, so bounding
// only the normalized value would let a raw value validation must
// refuse slip through shortened; a trimmed-and-resolved copy would
// likewise let an arbitrary run of spaces pad a directory past the
// rune ceiling.
func resolveBootAgentPackDirectories(cfg *Config, configDir string) error {
	if configDir == "" || len(cfg.Skills.BootAgentPacks) == 0 {
		return nil
	}
	canonicalDir, err := filepath.Abs(filepath.Clean(configDir))
	if err != nil {
		return fmt.Errorf("skills.boot_agent_packs: resolve config directory %q: %w", configDir, err)
	}
	for i := range cfg.Skills.BootAgentPacks {
		p := &cfg.Skills.BootAgentPacks[i]
		// The raw-value shape check runs FIRST — before filepath.Clean /
		// filepath.Join can shorten the value. A raw over-bound
		// absolute or relative `a/../` path Clean-s to a short value
		// that a bound on the normalized form would accept, so the
		// bound must be enforced on the raw stored value with the same
		// predicate validation uses (no drift possible between passes).
		if err := validateBootAgentPackDirectoryShape(p.Directory); err != nil {
			return fieldError(fmt.Sprintf("skills.boot_agent_packs[%d].directory", i), err.Error())
		}
		if filepath.IsAbs(p.Directory) {
			p.Directory = filepath.Clean(p.Directory)
			continue
		}
		joined := filepath.Clean(filepath.Join(canonicalDir, p.Directory))
		if !pathHasPrefixWithinRoot(joined, canonicalDir) {
			return fieldError(fmt.Sprintf("skills.boot_agent_packs[%d].directory", i),
				fmt.Sprintf("%q escapes the config directory %q", p.Directory, canonicalDir))
		}
		p.Directory = joined
	}
	return nil
}

// pathHasPrefixWithinRoot is the canonical prefix check (CLAUDE.md §7
// rule 5): true when p equals root or lies strictly under it,
// avoiding the false-positive where root=/a would otherwise match
// /abc.
func pathHasPrefixWithinRoot(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// WithOverrides applies a flat key->string override map to a
// previously-loaded *Config and re-validates. Keys are dotted paths
// matching the YAML field names ("server.bind_addr", "llm.model").
// This is the seam for CLI flag layering and Console
// pushed config (post-V1); Harbor ships only the mechanism.
//
// Constraint: `tools.http_manifests` entries and
// `skills.boot_agent_packs[].directory` values injected here skip the
// Load-time relative-path resolution (the config file's directory is
// not retained on *Config, so there is nothing to resolve against —
// only the structural re-validation runs). Overrides that set manifest
// paths or pack directories must use ABSOLUTE paths.
//
// A RELATIVE `tools.http_manifests` entry passes through unresolved
// and resolves against the process CWD at boot via the HTTP driver's
// own Clean+Abs, exactly like a hand-built *Config's would (the
// documented HTTP-manifest posture).
//
// A RELATIVE `skills.boot_agent_packs[].directory` injected here is
// DIFFERENT: HA-66 forbids any CWD fallback for boot pack directories.
// With no retained config-file provenance the relative value remains
// UNRESOLVED, and the later boot loader must FAIL LOUD on it — the
// override must use an absolute directory (the same rule a hand-built
// *Config obeys).
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

// Defaults returns a *Config pre-populated with the documented
// non-security defaults. Security-relevant fields (JWT algorithms,
// audit redaction patterns) are intentionally absent so Validate
// fails loudly when an operator omits them.
//
// Exported in a later phase: before then this baseline was
// loader-private, so a YAML-loaded config and a hand-built config got
// DIFFERENT baselines and factories compensated inconsistently (events
// fails loud on zero values; sessions self-defaults). `Load` starts
// from this same function; a headless Go consumer building a config
// programmatically starts here too:
//
//	cfg := config.Defaults()
//	cfg.LLM.Provider = "openrouter"   // required-for-core
//	cfg.LLM.Model = "anthropic/claude-sonnet-4"
//	cfg.LLM.APIKey = "env.OPENROUTER_API_KEY"
//	if err := cfg.ValidateCore(); err != nil { ... }
//
// Required-for-core fields a zero-config consumer must set before
// `ValidateCore` passes: `LLM.Provider`, `LLM.Model`, `LLM.APIKey`
// (the production `bifrost` driver demands a real provider — CLAUDE.md
// §13 "no test stubs as production defaults"). Everything else carries
// a working default. The full-binary `Validate()` additionally demands
// the Identity (JWT) block — see ValidateCore's godoc for the split.
func Defaults() *Config {
	return &Config{
		Postgres: PostgresConfig{
			Pool: PostgresPoolConfig{
				MaxOpen:         3,
				MaxIdle:         1,
				ConnMaxLifetime: 5 * time.Minute,
				ConnMaxIdleTime: 30 * time.Second,
			},
		},
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
			// flipped the default from "mock" to
			// "bifrost". A config with an empty `llm.driver` now
			// defaults to the production driver — missing config keys
			// fail loud rather than silently routing through a stub.
			// In the `harbor` binary the mock driver IS registry-
			// present (cmd/harbor/devmock.go blank-imports it at the
			// dev-cmd boundary, never main.go) but fail-closed: the
			// `validateLLMProvider` gate refuses `driver: mock`
			// unless HARBOR_DEV_ALLOW_MOCK=1 fires its stderr banner.
			// Headless embedders importing only `internal/drivers/
			// prod` never register the mock at all. The §13 "test
			// stubs as production defaults" amendment is closed for
			// the LLM seam.
			Driver:               "bifrost",
			Timeout:              60 * time.Second,
			ContextWindowReserve: 0.05, // 5% safety margin
			Corrections: LLMCorrectionsConfig{
				// corrections enabled by default. Operators
				// who omit the field get the production behaviour.
				// `*bool` distinguishes "operator didn't set" (nil →
				// loader fills with true) from "operator explicitly
				// disabled" (*false). The loader's `Defaults()` runs
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
		// pause lifecycle. MaxParkDuration 0 = pauses
		// never expire and no sweeper is started (the documented default);
		// SweepInterval is the sweeper cadence once an operator opts in.
		PauseResume: PauseResumeConfig{
			MaxParkDuration: 0,
			SweepInterval:   time.Minute,
		},
		Artifacts: ArtifactsConfig{
			Driver:                    "inmem",
			FSRoot:                    "",
			HeavyOutputThresholdBytes: DefaultHeavyOutputThresholdBytes,
			// The read-back bound: what an operator tunes alongside the
			// offload threshold above. Seeded here so `harbor validate`
			// prints the effective values rather than two zeroes an
			// operator has to know resolve elsewhere.
			FetchDefaultMaxBytes: DefaultArtifactFetchMaxBytes,
			FetchHardMaxBytes:    DefaultArtifactFetchHardMaxBytes,
			// S3-style driver defaults. Region defaults to
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
		// closes issue #126. The V1 planner-driver default is
		// "react" (the reference LLM-driven ReAct concrete — /).
		// A config with an empty `planner.driver` boots with
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
			// `harbor dev` hot-reload defaults. The
			// block is opt-out: Enabled defaults to true; the `--no-hot-
			// reload` CLI flag is the operator-facing escape hatch.
			// Policy `drain` waits for in-flight RunLoops to finish up to
			// DrainTimeout before restarting. WatchRoots defaults to the
			// project-local drafts directory.
			DevHotReload: DevHotReloadConfig{
				Enabled:      boolPtr(true),
				Policy:       DevHotReloadPolicyDrain,
				DrainTimeout: 5 * time.Second,
				WatchRoots:   []string{".harbor/agents"},
			},
		},
	}
}

// boolPtr returns a pointer to b. Used by `Defaults()` to populate
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
	for i := range v.NumField() {
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
