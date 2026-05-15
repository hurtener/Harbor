package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
)

// Deps carries the runtime dependencies the LLM client subsystem
// consumes. Both are mandatory — fail-loudly at construction.
//
//   - `Artifacts` is the auto-materialize target (D-022). Inline
//     `DataURL` content above the heavy-output threshold is rewritten
//     as an `Artifact` whose bytes live in the store.
//   - `Bus` is the canonical event bus. The safety pass publishes
//     `llm.image.materialized` / `llm.context_leak` /
//     `llm.context_window_exceeded`; the request-emit path (Phase 36a
//     subscriber lands here) publishes `llm.cost.recorded`.
//
// The package does NOT depend on `state.StateStore` — the LLM client
// is stateless across calls (D-025).
type Deps struct {
	Artifacts artifacts.ArtifactStore
	Bus       events.EventBus
}

// ConfigSnapshot is the strict subset of `config.LLMConfig` the LLM
// package consumes. Keeping a snapshot decouples drivers from the
// config package's type evolution (mirrors `internal/memory`'s
// pattern).
//
//   - `Driver` selects the §4.4 factory. Empty defaults to
//     `DefaultDriver` (Phase 32 = "mock"; Phase 33 will leave the
//     default explicit at the caller — operator must opt-in to
//     `bifrost`).
//   - `ContextWindowReserve` is the safety-net token-budget margin
//     (default 0.05 / 5%). Range [0.0, 1.0); validated at the config
//     layer + at construction.
//   - `HeavyOutputThreshold` mirrors
//     `config.ArtifactsConfig.HeavyOutputThresholdBytes` so the LLM
//     package does not re-import the artifact-config struct. Default
//     32 KiB.
//   - `ModelProfiles` is keyed by canonical model name. The safety
//     net's token-budget guard requires a profile entry for the
//     model in the `CompleteRequest`; missing → `ErrUnsupportedModel`.
//
// `Provider` / `Model` / `APIKey` / `BaseURL` / `Timeout` are the
// Phase-33 bifrost-driver knobs. Phase 32 stores them so the
// snapshot's shape is stable across phases; the mock driver ignores
// them. Phase 33's bifrost driver will read them.
type ConfigSnapshot struct {
	Driver               string
	ContextWindowReserve float64
	HeavyOutputThreshold int
	ModelProfiles        map[string]ModelProfile

	// DisableCorrections opts OUT of the Phase 34 per-provider
	// correction layer. Zero-value (false) = corrections enabled —
	// production callers wire `corrections.Wrap(safetyClient(driver))`
	// so quirks like NIM message reordering, OpenAI strict-schema
	// mode, thinking-class reasoning routing, Anthropic envelope
	// translation, and usage backfill all apply automatically. Tests
	// that need to exercise the safety pass in isolation set this to
	// true.
	//
	// Inverse-named so the zero-value matches the production default
	// — direct callers (tests, programmatic snapshot construction)
	// don't have to flip an extra knob to get correct behaviour. The
	// config loader resolves the operator-facing `corrections.enabled`
	// yaml field (default true) into this inverse.
	DisableCorrections bool

	// DisableDowngrade opts OUT of the Phase 35 structured-output
	// downgrade chain. Zero-value (false) = enabled. Inverse-named so
	// production callers get the right behaviour by default.
	DisableDowngrade bool

	// DisableRetry opts OUT of the Phase 36 retry-with-feedback
	// wrapper. Zero-value (false) = enabled. The wrapper is a no-op
	// when `CompleteRequest.Validator` is nil, so disabling is only
	// useful for tests that need to isolate the downgrade layer.
	DisableRetry bool

	// DisableGovernance opts OUT of the Phase 36a/36b governance
	// wrapper. Zero-value (false) = enabled — but the wrapper is also
	// a no-op pass-through when no `governance.Factory` has been
	// registered, so the latent default (Wave 7b scoping) requires
	// neither flag flip nor factory wiring. Tests that want to bypass
	// even a registered factory flip this true.
	DisableGovernance bool

	// Bifrost-driver knobs (Phase 33).
	Provider string
	Model    string
	APIKey   string
	BaseURL  string
	Timeout  time.Duration

	// CustomProviders is the operator-declared registry of
	// OpenAI-compatible providers (Phase 33a). When `Provider`
	// matches a custom entry's `Name`, the entry's `BaseURL` /
	// `APIKeyEnvVar` / `Models` / network knobs apply (legacy
	// `APIKey` / `BaseURL` / `Timeout` ignored for that case). The
	// list is keyed only by `Name`; the bifrost driver iterates and
	// registers all entries with bifrost's `Account`.
	CustomProviders []CustomProviderSpec

	// NetworkDefaults applies to every provider when the per-provider
	// override is absent. Zero-valued fields fall through to
	// bifrost's package-level defaults at construction. Restart-
	// required.
	NetworkDefaults NetworkDefaults
}

// CustomProviderSpec is one operator-declared OpenAI-compatible
// provider (Phase 33a). The bifrost driver maps each entry to a
// `bfschemas.ProviderConfig` with `CustomProviderConfig.BaseProviderType =
// schemas.OpenAI`. Zero-valued network knobs fall through to
// `ConfigSnapshot.NetworkDefaults`, which itself falls through to
// bifrost's package-level defaults.
//
// `APIKeyEnvVar` is the environment-variable NAME (no `env.` prefix);
// the driver resolves `os.Getenv(name)` at construction. Missing →
// `ErrMissingAPIKey` with the env var named.
//
// `RequestPathOverrides` maps `bfschemas.RequestType` (string-coded
// at this layer to avoid the import) to a custom URL path; the
// bifrost driver translates the keys when wiring the config. Used
// for OpenAI-compatible endpoints that host e.g. `/chat/completions`
// at the root.
type CustomProviderSpec struct {
	Name                 string
	BaseURL              string
	APIKeyEnvVar         string
	Models               []string
	BaseProviderType     string
	Timeout              time.Duration
	MaxRetries           int
	RetryBackoffInitial  time.Duration
	RetryBackoffMax      time.Duration
	Concurrency          int
	BufferSize           int
	RequestPathOverrides map[string]string
}

// NetworkDefaults are the operator-tunable defaults bifrost applies
// to every provider (native + custom) when the per-provider override
// is absent (Phase 33a). Zero-valued fields fall through to
// bifrost's package-level defaults.
type NetworkDefaults struct {
	Timeout             time.Duration
	MaxRetries          int
	RetryBackoffInitial time.Duration
	RetryBackoffMax     time.Duration
	Concurrency         int
	BufferSize          int
}

// Factory builds a `Driver` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (Driver, error)

// DefaultDriver names the production LLM driver Phase 64 (D-089)
// flipped this constant to point at — `"bifrost"`, the pure-Go LLM
// gateway shipped by Phase 33. Before Phase 64 this was `"mock"`; the
// flip closes the §13 "test stubs as production defaults" amendment
// for the LLM seam.
//
// Operators in production set `llm.driver` explicitly to `"bifrost"`
// (the same value the config defaults to). The `mock` driver still
// self-registers via init() — its package init runs when an importer
// (a test that builds a deterministic LLM stack) blank-imports it —
// but the production `cmd/harbor` binary never imports the mock
// package, so a config that lists `driver: mock` in a production
// build hits `ErrUnknownDriver: "mock" (registered: bifrost)` rather
// than silently routing through a stub.
//
// Dev-only escape hatch (D-089): the `harbor dev` subcommand reads
// `HARBOR_DEV_ALLOW_MOCK=1` and, when set, blank-imports the mock
// driver itself (the conditional blank-import lives at the
// subcommand boundary, not in `main.go`) AND prints a stderr banner
// `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` on every boot.
// Outside that one explicit dev path, the mock is unreachable.
const DefaultDriver = "bifrost"

// Defaults applied when the snapshot's corresponding field is zero.
// Kept here (not in `validate.go`) so an operator who constructs a
// snapshot programmatically still gets reasonable behaviour without
// every test wiring also touching the config layer.
const (
	DefaultContextWindowReserve = 0.05   // 5%
	DefaultHeavyOutputThreshold = 32_768 // 32 KiB; matches D-022 / RFC §6.10
	// DefaultMaxRetries (Phase 36) — the retry-with-feedback bound
	// when `ModelProfile.MaxRetries` is zero. Conservative: one
	// corrective re-ask after the original attempt.
	DefaultMaxRetries = 1
)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}

	// correctionsWrapperMu guards correctionsWrapper. Phase 34's
	// corrections package self-registers via init() — the hook
	// pattern avoids a package import cycle (corrections imports
	// llm). Callers that don't import the corrections package see
	// nil and Open() returns the safetyClient verbatim.
	correctionsWrapperMu sync.RWMutex
	correctionsWrapper   func(LLMClient, ConfigSnapshot) LLMClient

	// downgradeWrapperMu guards downgradeWrapper. Phase 35's output
	// package self-registers via init(); blank-imported in
	// `cmd/harbor/main.go`.
	downgradeWrapperMu sync.RWMutex
	downgradeWrapper   func(LLMClient, ConfigSnapshot, Deps) LLMClient

	// retryWrapperMu guards retryWrapper. Phase 36's retry package
	// self-registers via init().
	retryWrapperMu sync.RWMutex
	retryWrapper   func(LLMClient, ConfigSnapshot, Deps) LLMClient

	// governanceWrapperMu guards governanceWrapper. Phase 36a/36b's
	// governance package self-registers via init() (D-044). The
	// governance wrapper composes OUTSIDE the rest of the chain —
	// outermost layer in `Open` — so PreCall fires before retry /
	// downgrade / corrections / safety / driver and PostCall fires
	// after the whole stack returns.
	governanceWrapperMu sync.RWMutex
	governanceWrapper   func(LLMClient, ConfigSnapshot, Deps) LLMClient

	// defaultOutputModeResolverMu guards defaultOutputModeResolver. The
	// resolver returns the per-known-provider `OutputMode` default for
	// a model name; `internal/llm/corrections` registers it via
	// `RegisterDefaultOutputModeResolver`. The hook pattern keeps
	// `applyDefaults` free of an `internal/llm/corrections` import
	// (corrections imports `internal/llm`, so the reverse would cycle).
	// Wave-end audit FAIL #1: without this hook, `corrections.DefaultOutputModeFor`
	// was dead code and three doc sites lied about per-known-provider
	// fallback.
	defaultOutputModeResolverMu sync.RWMutex
	defaultOutputModeResolver   func(model string) OutputMode
)

// RegisterCorrectionsWrapper installs the Phase 34 corrections
// wrapper hook. Called once from `internal/llm/corrections.init()`;
// the production binary picks up the registration by blank-importing
// the corrections package.
//
// The hook signature mirrors `corrections.Wrap` — given the inner
// `LLMClient` (the safety wrapper) and the config snapshot, returns
// the corrections-wrapped client.
//
// Re-registering panics — the registration model is write-once-at-
// init and a duplicate signals a build misconfig.
func RegisterCorrectionsWrapper(fn func(LLMClient, ConfigSnapshot) LLMClient) {
	if fn == nil {
		panic("llm: RegisterCorrectionsWrapper called with nil hook")
	}
	correctionsWrapperMu.Lock()
	defer correctionsWrapperMu.Unlock()
	if correctionsWrapper != nil {
		panic("llm: corrections wrapper already registered")
	}
	correctionsWrapper = fn
}

// resetCorrectionsWrapperForTesting clears the registered corrections
// hook. Used only by package-internal tests that exercise the
// corrections-disabled code path; the hook is otherwise write-once.
//
//nolint:unused // referenced by tests in same package.
func resetCorrectionsWrapperForTesting() {
	correctionsWrapperMu.Lock()
	defer correctionsWrapperMu.Unlock()
	correctionsWrapper = nil
}

// RegisterDowngradeWrapper installs the Phase 35 structured-output
// downgrade wrapper hook. Called once from
// `internal/llm/output.init()`; the production binary blank-imports
// `internal/llm/output` so the registration fires at boot.
//
// The hook receives the inner `LLMClient` (typically `corrections(safety(driver))`),
// the config snapshot, and the Deps so the wrapper can emit events
// on the shared bus.
//
// Re-registering panics — write-once-at-init.
func RegisterDowngradeWrapper(fn func(LLMClient, ConfigSnapshot, Deps) LLMClient) {
	if fn == nil {
		panic("llm: RegisterDowngradeWrapper called with nil hook")
	}
	downgradeWrapperMu.Lock()
	defer downgradeWrapperMu.Unlock()
	if downgradeWrapper != nil {
		panic("llm: downgrade wrapper already registered")
	}
	downgradeWrapper = fn
}

//nolint:unused // referenced by tests in same package.
func resetDowngradeWrapperForTesting() {
	downgradeWrapperMu.Lock()
	defer downgradeWrapperMu.Unlock()
	downgradeWrapper = nil
}

// RegisterRetryWrapper installs the Phase 36 retry-with-feedback
// wrapper hook. Called once from `internal/llm/retry.init()`; the
// production binary blank-imports `internal/llm/retry`.
//
// The hook signature mirrors `RegisterDowngradeWrapper`.
//
// Re-registering panics — write-once-at-init.
func RegisterRetryWrapper(fn func(LLMClient, ConfigSnapshot, Deps) LLMClient) {
	if fn == nil {
		panic("llm: RegisterRetryWrapper called with nil hook")
	}
	retryWrapperMu.Lock()
	defer retryWrapperMu.Unlock()
	if retryWrapper != nil {
		panic("llm: retry wrapper already registered")
	}
	retryWrapper = fn
}

//nolint:unused // referenced by tests in same package.
func resetRetryWrapperForTesting() {
	retryWrapperMu.Lock()
	defer retryWrapperMu.Unlock()
	retryWrapper = nil
}

// RegisterGovernanceWrapper installs the Phase 36a/36b governance
// wrapper hook. Called once from `internal/governance.init()`; the
// production binary blank-imports the package so the hook lands at
// boot. Governance composes OUTSIDE the entire downstream chain
// (D-043 + D-044) — the wrapper sits at the outermost layer in `Open`
// so `PreCall` fires before retry / downgrade / corrections / safety
// even reach the driver.
//
// The hook receives the inner `LLMClient` (typically
// `retry(downgrade(corrections(safety(driver))))`), the config snapshot,
// and the Deps so the wrapper can build its Subsystem if a factory has
// been registered via `governance.SetFactory`. Latent default: with no
// factory set, the hook returns `inner` unchanged.
//
// Re-registering panics — write-once-at-init.
func RegisterGovernanceWrapper(fn func(LLMClient, ConfigSnapshot, Deps) LLMClient) {
	if fn == nil {
		panic("llm: RegisterGovernanceWrapper called with nil hook")
	}
	governanceWrapperMu.Lock()
	defer governanceWrapperMu.Unlock()
	if governanceWrapper != nil {
		panic("llm: governance wrapper already registered")
	}
	governanceWrapper = fn
}

//nolint:unused // referenced by tests in same package.
func resetGovernanceWrapperForTesting() {
	governanceWrapperMu.Lock()
	defer governanceWrapperMu.Unlock()
	governanceWrapper = nil
}

// Register installs a driver factory under `name`. Drivers self-
// register from their package `init()`; `cmd/harbor` blank-imports
// the production driver to trigger registration (Phase 33+).
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build misconfig.
func Register(name string, factory Factory) {
	if name == "" {
		panic("llm: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("llm: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("llm: driver %q already registered", name))
	}
	factories[name] = factory
}

// RegisteredDrivers returns a sorted list of driver names. Useful
// for boot-log emission and for surfacing in error messages.
func RegisteredDrivers() []string {
	factoriesMu.RLock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	factoriesMu.RUnlock()
	sort.Strings(names)
	return names
}

func registeredNames() string {
	names := RegisteredDrivers()
	if len(names) == 0 {
		return "<none>"
	}
	return strings.Join(names, ",")
}

// Open returns the `LLMClient` built by the factory whose name
// matches `cfg.Driver` (defaults to `DefaultDriver` when empty).
//
// Identity is mandatory at every method on the returned client; the
// safety pass enforces. Deps are validated at construction —
// `nil Artifacts` / `nil Bus` return wrapped errors immediately.
//
// The returned client is a `*safetyClient` wrapping the registered
// driver: every `Complete` runs through `enforceContextSafety` BEFORE
// the driver sees the request. This is mandatory by construction —
// drivers cannot bypass it through the registry path.
func Open(_ context.Context, cfg ConfigSnapshot, deps Deps) (LLMClient, error) {
	if deps.Artifacts == nil {
		return nil, fmt.Errorf("%w: Deps.Artifacts is required (artifacts.ArtifactStore)", ErrInvalidConfig)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: Deps.Bus is required (events.EventBus)", ErrInvalidConfig)
	}

	cfg = applyDefaults(cfg)
	if err := validateSnapshot(cfg); err != nil {
		return nil, err
	}

	name := cfg.Driver
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownDriver, name, registeredNames())
	}

	drv, err := f(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("llm: driver %q construction failed: %w", name, err)
	}

	client := LLMClient(newSafetyClient(drv, cfg, deps))

	// Phase 34: compose corrections OUTSIDE the safety wrapper so the
	// safety pass sees the post-correction (final outgoing) payload.
	// D-041 settled the order.
	if !cfg.DisableCorrections {
		correctionsWrapperMu.RLock()
		wrap := correctionsWrapper
		correctionsWrapperMu.RUnlock()
		if wrap != nil {
			client = wrap(client, cfg)
		}
	}

	// Phase 35: downgrade composes OUTSIDE corrections. A downgrade
	// rewrites `ResponseFormat`; the corrections layer must re-apply
	// its per-provider envelope shaping to the new format on each
	// downgraded attempt. D-043 settles this order.
	if !cfg.DisableDowngrade {
		downgradeWrapperMu.RLock()
		wrap := downgradeWrapper
		downgradeWrapperMu.RUnlock()
		if wrap != nil {
			client = wrap(client, cfg, deps)
		}
	}

	// Phase 36: retry-with-feedback composes OUTSIDE downgrade. A
	// validator-driven retry adds a fresh corrective turn to the
	// messages; the new message sequence flows through downgrade +
	// corrections + safety on each attempt. D-043 settles this order.
	if !cfg.DisableRetry {
		retryWrapperMu.RLock()
		wrap := retryWrapper
		retryWrapperMu.RUnlock()
		if wrap != nil {
			client = wrap(client, cfg, deps)
		}
	}

	// Phase 36a / 36b (D-044): governance composes OUTSIDE retry —
	// PreCall fires before the entire chain reaches the driver, so
	// `ErrBudgetExceeded` / `ErrRateLimited` / `ErrMaxTokensExceeded`
	// short-circuit without burning a retry attempt or downgrade pass.
	// The hook is a no-op pass-through when no governance factory has
	// been registered (latent default).
	if !cfg.DisableGovernance {
		governanceWrapperMu.RLock()
		wrap := governanceWrapper
		governanceWrapperMu.RUnlock()
		if wrap != nil {
			client = wrap(client, cfg, deps)
		}
	}
	return client, nil
}

// applyDefaults populates zero-valued fields with the Phase 32
// defaults. Cheap; idempotent. Also normalises Phase 35's
// `JSONSchemaMode` string into the typed `OutputMode` so the rest of
// the stack reads one source of truth.
func applyDefaults(cfg ConfigSnapshot) ConfigSnapshot {
	if cfg.Driver == "" {
		cfg.Driver = DefaultDriver
	}
	if cfg.ContextWindowReserve == 0 {
		cfg.ContextWindowReserve = DefaultContextWindowReserve
	}
	if cfg.HeavyOutputThreshold == 0 {
		cfg.HeavyOutputThreshold = DefaultHeavyOutputThreshold
	}
	if cfg.ModelProfiles != nil {
		// Read the registered default-OutputMode resolver once per
		// snapshot — the corrections package wires this at init when
		// its blank-import lands; tests that don't blank-import
		// corrections see the resolver nil and fall through to the
		// JSONSchemaMode legacy mapping only.
		resolver := getDefaultOutputModeResolver()
		// Normalise per-profile fields. ModelProfile is value-typed
		// so we copy before mutating to preserve the caller's map
		// values.
		normalised := make(map[string]ModelProfile, len(cfg.ModelProfiles))
		for name, p := range cfg.ModelProfiles {
			if p.OutputMode == OutputModeUnset && p.JSONSchemaMode != "" {
				switch p.JSONSchemaMode {
				case "native":
					p.OutputMode = OutputModeNative
				case "tools":
					p.OutputMode = OutputModeTools
				case "prompted":
					p.OutputMode = OutputModePrompted
				}
			}
			// Per-known-provider default fallback (Wave 7b audit FAIL
			// #1): if the operator did not declare an OutputMode AND
			// the legacy JSONSchemaMode mapping above did not resolve
			// it, ask the corrections package for the per-model
			// canonical default (openai/o1* → Prompted, anthropic/*
			// → Native, etc.). Unrecognized models still fall through
			// to OutputModeUnset; the downgrade chain skips them.
			if p.OutputMode == OutputModeUnset && resolver != nil {
				p.OutputMode = resolver(name)
			}
			if p.MaxRetries == 0 {
				p.MaxRetries = DefaultMaxRetries
			}
			normalised[name] = p
		}
		cfg.ModelProfiles = normalised
	}
	return cfg
}

// RegisterDefaultOutputModeResolver installs the per-known-provider
// `OutputMode` resolver from `internal/llm/corrections`. Called once
// from `corrections.init()`; the production binary blank-imports the
// corrections package so the registration fires at boot. Re-registering
// panics — write-once-at-init.
func RegisterDefaultOutputModeResolver(fn func(model string) OutputMode) {
	if fn == nil {
		panic("llm: RegisterDefaultOutputModeResolver called with nil resolver")
	}
	defaultOutputModeResolverMu.Lock()
	defer defaultOutputModeResolverMu.Unlock()
	if defaultOutputModeResolver != nil {
		panic("llm: default-OutputMode resolver already registered")
	}
	defaultOutputModeResolver = fn
}

// resetDefaultOutputModeResolverForTesting clears the registered
// resolver. Used only by package-internal tests that exercise the
// resolver-absent code path; the hook is otherwise write-once.
//
//nolint:unused // referenced by tests in same package.
func resetDefaultOutputModeResolverForTesting() {
	defaultOutputModeResolverMu.Lock()
	defer defaultOutputModeResolverMu.Unlock()
	defaultOutputModeResolver = nil
}

// getDefaultOutputModeResolver returns the registered resolver under a
// read lock. Nil if no resolver has been registered.
func getDefaultOutputModeResolver() func(model string) OutputMode {
	defaultOutputModeResolverMu.RLock()
	defer defaultOutputModeResolverMu.RUnlock()
	return defaultOutputModeResolver
}

// validateSnapshot checks the structural invariants the safety pass
// depends on. The config-layer validator (`internal/config`'s
// `validateLLM`) performs the same checks at boot — this is the
// last-resort guard for programmatic snapshot construction (tests,
// future Protocol-driven setters).
func validateSnapshot(cfg ConfigSnapshot) error {
	if cfg.ContextWindowReserve < 0 || cfg.ContextWindowReserve >= 1 {
		return fmt.Errorf("%w: ContextWindowReserve=%g must be in [0, 1)", ErrInvalidConfig, cfg.ContextWindowReserve)
	}
	if cfg.HeavyOutputThreshold <= 0 {
		return fmt.Errorf("%w: HeavyOutputThreshold=%d must be > 0", ErrInvalidConfig, cfg.HeavyOutputThreshold)
	}
	for name, p := range cfg.ModelProfiles {
		if p.ContextWindowTokens <= 0 {
			return fmt.Errorf("%w: ModelProfiles[%q].ContextWindowTokens=%d must be > 0",
				ErrInvalidConfig, name, p.ContextWindowTokens)
		}
		if p.MaxRetries < 0 {
			return fmt.Errorf("%w: ModelProfiles[%q].MaxRetries=%d must be >= 0",
				ErrInvalidConfig, name, p.MaxRetries)
		}
		switch p.OutputMode {
		case OutputModeUnset, OutputModeNative, OutputModeTools, OutputModePrompted:
		default:
			return fmt.Errorf("%w: ModelProfiles[%q].OutputMode=%q is unknown",
				ErrInvalidConfig, name, p.OutputMode)
		}
	}
	return nil
}
