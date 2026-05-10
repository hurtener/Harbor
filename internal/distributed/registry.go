package distributed

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
)

// DefaultDriver is the Phase 22 production driver name for BOTH the
// MessageBus and the RemoteTransport registries. Post-V1 durable bus
// drivers (NATS / Postgres-as-queue at phase 86) and the A2A wire
// driver (phase 29) register additional names; Open switches on
// `cfg.BusDriver` / `cfg.RemoteDriver`.
const DefaultDriver = "loopback"

// BusFactory builds a MessageBus from a Dependencies struct. Drivers
// expose one BusFactory via init() → RegisterBus.
type BusFactory func(deps Dependencies) (MessageBus, error)

// RemoteFactory builds a RemoteTransport from a Dependencies struct.
// Drivers expose one RemoteFactory via init() → RegisterRemoteTransport.
type RemoteFactory func(deps Dependencies) (RemoteTransport, error)

// Dependencies bundles the wiring inputs every distributed driver
// receives. EventBus is the optional projection target for the bus
// loopback (drivers free to ignore when not in-process); Cfg carries
// the driver names + any future per-driver tuning fields.
type Dependencies struct {
	// EventBus is the typed event bus the loopback MessageBus projects
	// envelopes through. Optional for drivers that do not project;
	// the loopback bus REQUIRES it.
	EventBus events.EventBus
	// Cfg carries Phase 22's DistributedConfig (driver names today).
	Cfg config.DistributedConfig
}

var (
	busFactoriesMu sync.RWMutex
	busFactories   = map[string]BusFactory{}

	remoteFactoriesMu sync.RWMutex
	remoteFactories   = map[string]RemoteFactory{}
)

// RegisterBus installs a BusFactory under name. Drivers self-register
// from their package init(); cmd/harbor blank-imports the production
// driver to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-configuration.
func RegisterBus(name string, factory BusFactory) {
	if name == "" {
		panic("distributed: RegisterBus called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("distributed: RegisterBus(%q) called with nil factory", name))
	}
	busFactoriesMu.Lock()
	defer busFactoriesMu.Unlock()
	if _, exists := busFactories[name]; exists {
		panic(fmt.Sprintf("distributed: bus driver %q already registered", name))
	}
	busFactories[name] = factory
}

// RegisterRemoteTransport installs a RemoteFactory under name. Same
// contract as RegisterBus.
func RegisterRemoteTransport(name string, factory RemoteFactory) {
	if name == "" {
		panic("distributed: RegisterRemoteTransport called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("distributed: RegisterRemoteTransport(%q) called with nil factory", name))
	}
	remoteFactoriesMu.Lock()
	defer remoteFactoriesMu.Unlock()
	if _, exists := remoteFactories[name]; exists {
		panic(fmt.Sprintf("distributed: remote driver %q already registered", name))
	}
	remoteFactories[name] = factory
}

// OpenBus returns the MessageBus built by the factory whose name
// matches deps.Cfg.BusDriver (defaults to DefaultDriver when empty).
func OpenBus(_ context.Context, deps Dependencies) (MessageBus, error) {
	name := deps.Cfg.BusDriver
	if name == "" {
		name = DefaultDriver
	}
	return openBus(name, deps)
}

// OpenBusDriver opens a specific bus driver by name; useful for tests.
func OpenBusDriver(name string, deps Dependencies) (MessageBus, error) {
	return openBus(name, deps)
}

func openBus(name string, deps Dependencies) (MessageBus, error) {
	busFactoriesMu.RLock()
	f, ok := busFactories[name]
	busFactoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: bus %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames(RegisteredBusDrivers()))
	}
	return f(deps)
}

// OpenRemoteTransport returns the RemoteTransport built by the factory
// whose name matches deps.Cfg.RemoteDriver (defaults to DefaultDriver
// when empty).
func OpenRemoteTransport(_ context.Context, deps Dependencies) (RemoteTransport, error) {
	name := deps.Cfg.RemoteDriver
	if name == "" {
		name = DefaultDriver
	}
	return openRemote(name, deps)
}

// OpenRemoteTransportDriver opens a specific remote driver by name; useful for tests.
func OpenRemoteTransportDriver(name string, deps Dependencies) (RemoteTransport, error) {
	return openRemote(name, deps)
}

func openRemote(name string, deps Dependencies) (RemoteTransport, error) {
	remoteFactoriesMu.RLock()
	f, ok := remoteFactories[name]
	remoteFactoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: remote %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames(RegisteredRemoteTransportDrivers()))
	}
	return f(deps)
}

// RegisteredBusDrivers returns a sorted list of bus driver names.
func RegisteredBusDrivers() []string {
	busFactoriesMu.RLock()
	names := make([]string, 0, len(busFactories))
	for n := range busFactories {
		names = append(names, n)
	}
	busFactoriesMu.RUnlock()
	sort.Strings(names)
	return names
}

// RegisteredRemoteTransportDrivers returns a sorted list of remote driver names.
func RegisteredRemoteTransportDrivers() []string {
	remoteFactoriesMu.RLock()
	names := make([]string, 0, len(remoteFactories))
	for n := range remoteFactories {
		names = append(names, n)
	}
	remoteFactoriesMu.RUnlock()
	sort.Strings(names)
	return names
}

func registeredNames(names []string) string {
	if len(names) == 0 {
		return "<none>"
	}
	return strings.Join(names, ",")
}
