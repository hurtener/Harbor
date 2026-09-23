package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/persistence/sqlmigrate"
	"github.com/hurtener/Harbor/internal/state"
)

// Deps carries the runtime dependencies a memory driver needs.
//
// The `State` field is mandatory (typed wrapper writes
// opaque bytes through the generic surface). The `Bus` field is
// mandatory so identity-rejection emits land on the audit pipeline.
// Drivers MUST NOT accept missing deps silently; the registry
// rejects an `Open` call whose Deps omits either with a wrapped
// error.
type Deps struct {
	State state.StateStore
	Bus   events.EventBus
	// Redactor and RetentionTTL are the same execution-memory dependencies
	// supplied by runtime composition. Put refuses a missing redactor.
	Redactor     audit.Redactor
	RetentionTTL time.Duration
}

// ConfigSnapshot is the strict subset of `config.MemoryConfig` the
// memory package consumes. Keeping a snapshot decouples drivers
// from the config package's type evolution. Callers (typically
// `cmd/harbor/main.go`'s bootstrap or a test wiring helper)
// translate `config.MemoryConfig` → `ConfigSnapshot` at the seam.
//
// `DSN` is consumed by the SQLite + Postgres drivers; the
// InMem driver ignores it. Validation of "DSN required for
// persistent drivers" lives at the config layer (`validateMemory`
// in `internal/config/validate.go`) and at the driver constructor
// itself — fail-loudly twice so a misconfiguration surfaces early.
//
// RecentTurns bounds detailed recent execution evidence. Zero selects twenty.
type ConfigSnapshot struct {
	Driver        string
	DSN           string
	MigrationMode sqlmigrate.Mode
	Strategy      Strategy
	BudgetTokens  int
	RecentTurns   int
}

// Factory builds a `MemoryStore` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (MemoryStore, error)

// DefaultDriver is the production driver name. The SQL drivers
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
	return ValidateStrategy(cfg.Strategy)
}

// ValidateStrategy rejects removed and unknown memory strategies at construction.
// An omitted strategy selects cumulative rolling memory, like the YAML default.
func ValidateStrategy(strategy Strategy) error {
	switch strategy {
	case "", StrategyRollingSummary, StrategyNone:
		return nil
	default:
		return fmt.Errorf("%w: %q; use rolling_summary or none", ErrStrategyNotImplemented, strategy)
	}
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
