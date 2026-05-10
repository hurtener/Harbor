package artifacts

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/config"
)

// DefaultDriver is the Phase 17 floor — the in-memory driver. Phase 18
// (SQLite-blob, Postgres-blob) and Phase 19 (S3-style) register
// additional names; `Open` switches on `cfg.Driver`.
const DefaultDriver = "inmem"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under name. Drivers self-register
// from their package init(); cmd/harbor blank-imports the production
// drivers to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-configuration.
func Register(name string, factory Factory) {
	if name == "" {
		panic("artifacts: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("artifacts: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("artifacts: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns the ArtifactStore built by the factory whose name
// matches cfg.Driver (defaults to DefaultDriver when cfg.Driver is
// empty).
func Open(_ context.Context, cfg config.ArtifactsConfig) (ArtifactStore, error) {
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg)
}

// OpenDriver opens a specific driver by name; useful for tests that
// want to exercise the registry against a non-default driver.
func OpenDriver(name string, cfg config.ArtifactsConfig) (ArtifactStore, error) {
	return open(name, cfg)
}

func open(name string, cfg config.ArtifactsConfig) (ArtifactStore, error) {
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
