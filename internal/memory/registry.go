package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/state"
)

// Deps carries the runtime dependencies a memory driver needs.
//
// The `State` field is mandatory (D-027 — typed wrapper writes
// opaque bytes through the generic surface). The `Bus` field is
// mandatory so identity-rejection emits land on the audit pipeline.
// Drivers MUST NOT accept missing deps silently; the registry
// rejects an `Open` call whose Deps omits either with a wrapped
// error.
//
// The `Summarizer` field (Phase 25a, D-174) is the injectable
// LLM-edge callable the `rolling_summary` strategy consumes. It is
// OPTIONAL — required only when `cfg.Strategy == StrategyRollingSummary`,
// ignored by `none` / `truncation`. The registry routes it into the
// driver factory, which threads it into the strategy executor. A
// `rolling_summary` config without a `Summarizer` fails loudly at
// `Open` (mirroring `strategy.New`'s rejection) — never a stub
// fallback (AGENTS.md §13). Existing callers that construct
// `Deps{State, Bus}` keep compiling: the zero value is nil, valid for
// the non-summarising strategies.
type Deps struct {
	State      state.StateStore
	Bus        events.EventBus
	Summarizer Summarizer
}

// ConfigSnapshot is the strict subset of `config.MemoryConfig` the
// memory package consumes. Keeping a snapshot decouples drivers
// from the config package's type evolution. Callers (typically
// `cmd/harbor/main.go`'s bootstrap or a test wiring helper)
// translate `config.MemoryConfig` → `ConfigSnapshot` at the seam.
//
// `DSN` is consumed by the SQLite + Postgres drivers (Phase 25); the
// InMem driver ignores it. Validation of "DSN required for
// persistent drivers" lives at the config layer (`validateMemory`
// in `internal/config/validate.go`) and at the driver constructor
// itself — fail-loudly twice so a misconfiguration surfaces early.
//
// `RecoveryBacklogMax` is consumed by the `rolling_summary`
// strategy executor only; other strategies ignore the field.
// Default (zero) → strategy.DefaultRecoveryBacklogMax.
type ConfigSnapshot struct {
	Driver             string
	DSN                string
	Strategy           Strategy
	BudgetTokens       int
	RecoveryBacklogMax int
}

// Factory builds a `MemoryStore` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (MemoryStore, error)

// DefaultDriver is the Phase 23 production driver name. Phase 25
// (SQLite + Postgres) registers additional names.
const DefaultDriver = "inmem"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under `name`. Drivers self-
// register from their package `init()`; `cmd/harbor` blank-imports
// the production driver to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-config.
func Register(name string, factory Factory) {
	if name == "" {
		panic("memory: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("memory: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("memory: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the `MemoryStore` built by the factory whose name
// matches `cfg.Driver` (defaults to `DefaultDriver` when empty).
//
// Deps are validated: a missing StateStore or EventBus returns a
// wrapped error before the factory runs — fail loudly, never
// silently degrade.
func Open(_ context.Context, cfg ConfigSnapshot, deps Deps) (MemoryStore, error) {
	if err := validateDeps(cfg, deps); err != nil {
		return nil, err
	}
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg, deps)
}

// OpenDriver opens a specific driver by name; useful for tests
// that want to exercise the registry against a non-default driver.
func OpenDriver(name string, cfg ConfigSnapshot, deps Deps) (MemoryStore, error) {
	if err := validateDeps(cfg, deps); err != nil {
		return nil, err
	}
	return open(name, cfg, deps)
}

func validateDeps(cfg ConfigSnapshot, d Deps) error {
	if d.State == nil {
		return fmt.Errorf("memory: Deps.State is required (state.StateStore)")
	}
	if d.Bus == nil {
		return fmt.Errorf("memory: Deps.Bus is required (events.EventBus)")
	}
	// Fail loudly at the registry boundary when rolling_summary is
	// configured without a Summarizer (Phase 25a, D-174). The driver
	// factory + strategy.New also reject this, but catching it here
	// surfaces the misconfiguration before any DB connection is
	// opened — and never silently falls back to a stub (AGENTS.md §13).
	if cfg.Strategy == StrategyRollingSummary && d.Summarizer == nil {
		return fmt.Errorf("memory: Deps.Summarizer is required for strategy %q (no stub fallback)", StrategyRollingSummary)
	}
	return nil
}

func open(name string, cfg ConfigSnapshot, deps Deps) (MemoryStore, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames())
	}
	return f(cfg, deps)
}

// RegisteredDrivers returns a sorted list of driver names. Useful
// for boot-log emission ("memory drivers available: inmem") and
// for surfacing in error messages.
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
