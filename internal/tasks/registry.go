package tasks

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// DefaultDriver is the Phase 20 production driver name. Phase 87+
// post-V1 work may register additional names; Open switches on
// `cfg.Driver`.
const DefaultDriver = "inprocess"

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
		panic("tasks: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("tasks: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("tasks: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the TaskRegistry built by the factory whose name
// matches deps.Cfg.Driver (defaults to DefaultDriver when empty).
func Open(_ context.Context, deps Dependencies) (TaskRegistry, error) {
	name := deps.Cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, deps)
}

// OpenDriver opens a specific driver by name; useful for tests that
// want to exercise the registry against a non-default driver.
func OpenDriver(name string, deps Dependencies) (TaskRegistry, error) {
	return open(name, deps)
}

func open(name string, deps Dependencies) (TaskRegistry, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames())
	}
	return f(deps)
}

// RegisteredDrivers returns a sorted list of driver names. Useful for
// boot-log output and surfacing in error messages.
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
