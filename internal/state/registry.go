package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/config"
)

// Factory builds a StateStore from a StateConfig. Drivers expose one
// Factory each via init() → Register.
type Factory func(config.StateConfig) (StateStore, error)

// DefaultDriver is the Phase 07 production driver name. Phase 15
// (SQLite) and Phase 16 (Postgres) register additional names; Open
// switches on cfg.Driver.
const DefaultDriver = "inmem"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under name. Drivers self-register
// from their package init(); cmd/harbor blank-imports the production
// driver to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-configuration.
func Register(name string, factory Factory) {
	if name == "" {
		panic("state: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("state: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("state: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the StateStore built by the factory whose name matches
// cfg.Driver (defaults to DefaultDriver when cfg.Driver is empty).
func Open(_ context.Context, cfg config.StateConfig) (StateStore, error) {
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg)
}

// OpenDriver opens a specific driver by name; useful for tests that
// want to exercise the registry against a non-default driver.
func OpenDriver(name string, cfg config.StateConfig) (StateStore, error) {
	return open(name, cfg)
}

func open(name string, cfg config.StateConfig) (StateStore, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames())
	}
	return f(cfg)
}

// RegisteredDrivers returns a sorted list of driver names. Useful for
// boot-log output and for surfacing in error messages.
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
