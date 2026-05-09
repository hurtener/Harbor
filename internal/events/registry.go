package events

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
)

// Factory builds an EventBus from EventsConfig + an audit.Redactor.
// Drivers expose one Factory each via init() → Register.
type Factory func(config.EventsConfig, audit.Redactor) (EventBus, error)

// DefaultDriver is the Phase 05 production driver name. Phase 06
// replay-equipped drivers and Phase 57 durable-log drivers will
// register additional names; Open switches on cfg.Driver once
// EventsConfig.Driver is populated by Phase 02.
const DefaultDriver = "inmem"

// ErrUnknownDriver — the requested driver name is not in the registry.
var ErrUnknownDriver = errors.New("events: unknown driver")

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
	// registerForTestCounter makes RegisterForTest names unique across
	// `go test -count=N` iterations of the same test.
	registerForTestCounter atomic.Uint64
)

// Register installs a driver factory under name. Drivers self-register
// from their package init(); cmd/harbor blank-imports the production
// driver to trigger registration. Per AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build mis-configuration.
func Register(name string, factory Factory) {
	if name == "" {
		panic("events: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("events: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("events: driver %q already registered", name))
	}
	factories[name] = factory
}

// RegisterForTest installs a driver factory under a per-test unique
// name and registers a t.Cleanup that removes it when the test ends.
// Use this in tests that need to register a sentinel driver at
// runtime — production code path is still Register from init().
//
// The function returns the actual registered name (suffixed with the
// test's name and a counter so two `-count=N` iterations don't
// collide). The returned name is what callers should set on
// EventsConfig.Driver to route Open through the sentinel factory.
//
// Without this helper, runtime Register calls leak entries into the
// process-wide map and panic on the second invocation under
// `go test -count=N`. The cleanup hook closes that gap.
func RegisterForTest(t interface {
	Helper()
	Name() string
	Cleanup(func())
}, name string, factory Factory) string {
	t.Helper()
	uniq := fmt.Sprintf("%s::%s::%d", name, t.Name(), registerForTestCounter.Add(1))
	Register(uniq, factory)
	t.Cleanup(func() { unregister(uniq) })
	return uniq
}

// unregister removes a driver factory by name. Unexported because it
// is only meant for the test-cleanup path; production code must never
// remove a driver mid-flight (callers may hold an EventBus that the
// driver constructed). Idempotent on a missing name.
func unregister(name string) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	delete(factories, name)
}

// Open returns an EventBus built by the factory whose name matches
// cfg.Driver (defaults to DefaultDriver when cfg.Driver is empty).
// The audit.Redactor is mandatory: every Publish runs payloads
// through it before enqueueing.
func Open(_ context.Context, cfg config.EventsConfig, r audit.Redactor) (EventBus, error) {
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	return open(name, cfg, r)
}

// OpenDriver opens a specific driver by name; useful for tests that
// want to exercise the registry against a non-default driver.
func OpenDriver(name string, cfg config.EventsConfig, r audit.Redactor) (EventBus, error) {
	return open(name, cfg, r)
}

func open(name string, cfg config.EventsConfig, r audit.Redactor) (EventBus, error) {
	if r == nil {
		return nil, fmt.Errorf("events: Open requires an audit.Redactor (got nil)")
	}
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrUnknownDriver, name, registeredNames())
	}
	return f(cfg, r)
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
