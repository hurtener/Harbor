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

	// Bifrost-driver knobs (Phase 33).
	Provider string
	Model    string
	APIKey   string
	BaseURL  string
	Timeout  time.Duration
}

// Factory builds a `Driver` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (Driver, error)

// DefaultDriver is Phase 32's mock driver. Operators wire `bifrost`
// (Phase 33) explicitly; the default is the test-grade driver so a
// missing config doesn't silently route real LLM traffic.
const DefaultDriver = "mock"

// Defaults applied when the snapshot's corresponding field is zero.
// Kept here (not in `validate.go`) so an operator who constructs a
// snapshot programmatically still gets reasonable behaviour without
// every test wiring also touching the config layer.
const (
	DefaultContextWindowReserve = 0.05   // 5%
	DefaultHeavyOutputThreshold = 32_768 // 32 KiB; matches D-022 / RFC §6.10
)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

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
	return newSafetyClient(drv, cfg, deps), nil
}

// applyDefaults populates zero-valued fields with the Phase 32
// defaults. Cheap; idempotent.
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
	return cfg
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
	}
	return nil
}
