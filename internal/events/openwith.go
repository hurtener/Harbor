// openwith.go — the deps-aware factory entry point.
//
// The `Factory` signature deliberately carries no
// dependencies beyond `(EventsConfig, audit.Redactor)`. That left the
// durable driver with no factory-path way to SHARE the
// runtime's StateStore: the registry-path factory opens a private
// store from `events.state_driver` / `events.state_dsn`, and the only
// way to share the runtime's store was cmd-only direct construction
// (`durable.New(ctx, cfg, r, store)`), bypassing the §4.4 registry.
//
// `OpenWith` adds a PARALLEL entry point rather than breaking the
// registered `Factory` signature: drivers that want deps
// register a second, deps-aware factory via `RegisterWithDeps`;
// drivers that ignore deps register nothing extra and `OpenWith`
// falls back to their plain factory. `Open` keeps its signature and
// behaviour byte-for-byte. If a third deps-aware driver appears
// post-V1, revisiting the Factory shape itself is an RFC-level
// follow-up — not this seam.
package events

import (
	"context"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
)

// Deps bundles the runtime collaborators a deps-aware event-bus driver
// may consume. Every field is optional at this layer — each driver
// documents which deps it reads and what happens when they are absent
// (the durable driver fails loud when it can neither share a store nor
// open its own; see drivers/durable).
type Deps struct {
	// StartupContext carries the caller's construction lifetime into
	// deps-aware drivers that perform boot-time I/O. OpenWith fills it from
	// its ctx argument when the caller leaves it nil; the field exists so a
	// shared-store durable driver never has to recover under an unbounded
	// context.Background().
	StartupContext context.Context

	// State is the runtime's already-open StateStore. A deps-aware
	// driver that persists (the durable log) uses it INSTEAD of opening
	// a private store, so the event log and the rest of the runtime
	// share one persistence backend. The caller retains ownership:
	// the bus's Close never closes a shared store.
	State state.StateStore
}

// DepsFactory builds an EventBus from EventsConfig + an audit.Redactor
// + the runtime Deps. Deps-aware drivers expose one via init() →
// RegisterWithDeps, ALONGSIDE their plain Factory registration.
type DepsFactory func(config.EventsConfig, audit.Redactor, Deps) (EventBus, error)

var (
	depsFactoriesMu sync.RWMutex
	depsFactories   = map[string]DepsFactory{}
)

// RegisterWithDeps installs a deps-aware driver factory under name.
// Same write-once-at-init contract as Register: re-registering panics.
// A driver registers under the SAME name as its plain factory; the
// plain registration remains mandatory so `Open` keeps working.
func RegisterWithDeps(name string, factory DepsFactory) {
	if name == "" {
		panic("events: RegisterWithDeps called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("events: RegisterWithDeps(%q) called with nil factory", name))
	}
	depsFactoriesMu.Lock()
	defer depsFactoriesMu.Unlock()
	if _, exists := depsFactories[name]; exists {
		panic(fmt.Sprintf("events: deps-aware driver %q already registered", name))
	}
	depsFactories[name] = factory
}

// OpenWith returns an EventBus built by the deps-aware factory whose
// name matches cfg.Driver (defaults to DefaultDriver when empty),
// passing the runtime Deps through. Drivers that registered no
// deps-aware factory fall back to their plain Factory — so OpenWith
// is a strict superset of Open and callers can use it unconditionally.
func OpenWith(ctx context.Context, cfg config.EventsConfig, r audit.Redactor, deps Deps) (EventBus, error) {
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	if r == nil {
		return nil, fmt.Errorf("events: OpenWith requires an audit.Redactor (got nil)")
	}
	if deps.StartupContext == nil {
		deps.StartupContext = ctx
	}
	depsFactoriesMu.RLock()
	df, ok := depsFactories[name]
	depsFactoriesMu.RUnlock()
	if ok {
		return df(cfg, r, deps)
	}
	// No deps-aware registration — the driver ignores deps by
	// construction; route through the plain factory path (which also
	// produces the canonical unknown-driver error listing registrations).
	return open(name, cfg, r)
}
