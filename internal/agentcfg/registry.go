package agentcfg

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/state"
)

// Deps carries the runtime dependencies an agentcfg driver needs. State
// (the persistence floor) and Bus (the audit/observability bus) are
// mandatory; a nil either fails the factory loud with ErrInvalidConfig.
// Clock is optional (defaults to time.Now).
type Deps struct {
	State state.StateStore
	Bus   events.EventBus
	Clock func() time.Time
}

// Config selects the agentcfg driver. The StateStore-backed driver takes
// its persistence from Deps.State and ignores any DSN — the registry
// reuses the runtime's StateStore rather than opening its own.
type Config struct {
	// Driver names the registered factory. Empty → DefaultDriver.
	Driver string
}

// Factory builds a Registry from a Config + Deps. Drivers expose one
// Factory each via init() → Register.
type Factory func(cfg Config, deps Deps) (Registry, error)

// DefaultDriver is the production driver name. The StateStore-backed
// driver registers under it; it inherits the StateStore's own driver
// triad (in-mem / SQLite / Postgres) for identity isolation.
const DefaultDriver = "statestore"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under name. Drivers self-register
// from their package init(); the production driver aggregator
// blank-imports them. Re-registering the same name panics — the
// registration model is write-once-at-init.
func Register(name string, factory Factory) {
	if name == "" {
		panic("agentcfg: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("agentcfg: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("agentcfg: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the Registry built by the factory whose name matches
// cfg.Driver (defaults to DefaultDriver when empty). Deps are validated:
// a missing StateStore or EventBus returns a wrapped ErrInvalidConfig
// before the factory runs — fail loudly, never silently degrade.
func Open(_ context.Context, cfg Config, deps Deps) (Registry, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required", ErrInvalidConfig)
	}
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg, deps)
}

func open(name string, cfg Config, deps Deps) (Registry, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownDriver, name, registeredNames())
	}
	return f(cfg, deps)
}

// RegisteredDrivers returns a sorted list of registered driver names.
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
