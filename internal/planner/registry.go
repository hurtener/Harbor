package planner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hurtener/Harbor/internal/llm"
)

// D-103 — planner driver registry.
//
// The §4.4 seam pattern applied to the planner concrete. The V1 default
// driver is `react` (the reference LLM-driven ReAct planner — Phase 45 /
// D-051). New concretes (Plan-Execute, Workflow, Graph, Deterministic,
// Supervisor, MultiAgent, HumanApproval per RFC §6.2) add a new driver
// under `internal/planner/<name>/` without changing this registry's
// shape. The shape mirrors `internal/tools/auth/registry.go` (D-095)
// for OAuth-provider drivers; the structural precedent is deliberate.
//
// CLAUDE.md §1.3 names the swappable planner one of the three
// non-negotiable product properties. D-097 noted "future phases will
// read a planner choice from cfg.Planner and switch concretes" — this
// registry closes that note.
//
// The driver registry exists so operators declare a planner choice in
// `cfg.Planner.Driver` without writing Go wiring code. The dev stack
// walks the operator config at boot, looks up the entry's `Driver` in
// this registry, and constructs the `Planner` via the registered
// `Factory`.

// PlannerConfig is the boundary type the registry exposes to drivers.
// It mirrors `config.PlannerConfig` (the operator-facing YAML shape)
// but lives in `internal/planner` so concrete drivers can depend on it
// without forcing the `internal/config` package to import driver
// internals. The dev stack maps the YAML struct onto this struct at the
// boundary (D-095 precedent).
//
// `MaxSteps` is the planner-side circuit-breaker cap. Zero (the
// loader-side and registry-side default) means "use the driver's
// internal default" — e.g. `react.DefaultMaxSteps` (12). Negative is
// rejected at the validator edge (`internal/config/validate.go`).
//
// `Extra` is the per-driver opaque config map reserved for future
// per-flow knobs (e.g. a deterministic planner's scripted step
// sequence, a supervisor planner's sub-agent list). The V1 `react`
// driver ignores it. Future drivers consume their entries from `Extra`
// without changing the boundary's signature.
type PlannerConfig struct {
	// Driver names the registered planner driver to resolve. Required;
	// the validator rejects empty + unknown values pre-boot.
	Driver string

	// MaxSteps is the optional circuit-breaker step cap. Zero =
	// driver default.
	MaxSteps int

	// Extra is the driver-specific extras map. Reserved for future
	// drivers' per-flow knobs; unused by the V1 `react` driver.
	Extra map[string]string
}

// FactoryDeps bundles the shared collaborators every planner driver
// consumes at construction. The dev stack constructs the LLM client
// ONCE (one provider per binary; see `cmd/harbor/cmd_dev.go::bootDevStack`)
// and passes the same instance into every factory call.
//
// Future drivers may need additional collaborators (a tasks subsystem
// handle for the deterministic planner, a sub-planner registry for the
// supervisor planner). Add fields here as drivers land; keep the
// boundary narrow so a planner driver never reaches into runtime
// internals (the §13 import-graph rule for `internal/planner/...`).
type FactoryDeps struct {
	// LLM is the composed LLM client (retry → downgrade → corrections →
	// safety → driver per D-043). The V1 `react` driver consumes this
	// directly; drivers that don't use an LLM ignore the field.
	LLM llm.LLMClient
}

// Factory builds a Planner from a PlannerConfig + FactoryDeps. Drivers
// self-register one Factory each via init() → MustRegister.
//
// A factory MUST fail closed on missing required deps (drivers that
// need an LLM MUST reject a nil `deps.LLM`); the V1 `react` driver's
// constructor panics on nil LLM per its existing contract. Custom
// drivers MUST honour the same fail-loud contract — silent fallback is
// forbidden per §13.
type Factory func(cfg PlannerConfig, deps FactoryDeps) (Planner, error)

// Sentinel errors specific to the registry. Driver-internal errors
// continue to use the driver's own sentinels.
var (
	// ErrDriverUnknown — Resolve was called with a name no driver has
	// registered for. The error message lists the registered driver
	// names so the operator sees the typo.
	ErrDriverUnknown = errors.New("planner: driver not registered")
	// ErrDriverEmptyName — Register was called with an empty driver
	// name. Build-time configuration bug.
	ErrDriverEmptyName = errors.New("planner: driver registration: empty name")
	// ErrDriverNilFactory — Register was called with a nil Factory.
	// Build-time configuration bug.
	ErrDriverNilFactory = errors.New("planner: driver registration: nil factory")
	// ErrDriverDuplicate — Register was called twice for the same
	// driver name. Build-time configuration bug (typically a
	// double-blank-import).
	ErrDriverDuplicate = errors.New("planner: driver registration: duplicate name")
)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a Factory under name. Drivers self-register from
// their package init(); the binary entry point (`cmd/harbor/main.go`)
// blank-imports each driver to fire the registration.
//
// Re-registering the same name returns `ErrDriverDuplicate` (the
// caller, an init() function, should panic on this — it signals a
// build mis-configuration). The function does NOT panic itself so the
// test suite can exercise the duplicate-name path without bringing the
// process down.
func Register(name string, factory Factory) error {
	if name == "" {
		return ErrDriverEmptyName
	}
	if factory == nil {
		return fmt.Errorf("%w: %q", ErrDriverNilFactory, name)
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		return fmt.Errorf("%w: %q", ErrDriverDuplicate, name)
	}
	factories[name] = factory
	return nil
}

// MustRegister wraps Register and panics on error. The typical
// driver-side idiom: `init() { planner.MustRegister("react", New) }`.
// A duplicate-name panic at init signals a build bug (probably two
// drivers chose the same canonical name); the panic message names the
// offending driver so the operator's stack trace points at the fix.
func MustRegister(name string, factory Factory) {
	if err := Register(name, factory); err != nil {
		panic(fmt.Sprintf("planner.MustRegister(%q): %v", name, err))
	}
}

// Resolve constructs the Planner for cfg by dispatching to the driver
// named in `cfg.Driver`. Returns wrapped `ErrDriverUnknown` when the
// driver has not registered; otherwise delegates to the driver's
// Factory (whose own validation surfaces fail-closed errors).
//
// `ctx` is honoured for the construction itself; drivers should
// observe `ctx.Err()` between long phases of work (e.g. HTTP discovery
// probes — none for V1 react).
func Resolve(ctx context.Context, cfg PlannerConfig, deps FactoryDeps) (Planner, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("planner: Resolve cancelled: %w", err)
	}
	if cfg.Driver == "" {
		return nil, fmt.Errorf("%w: <empty> (registered: %s)",
			ErrDriverUnknown, registeredDriverNames())
	}
	factoriesMu.RLock()
	f, ok := factories[cfg.Driver]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrDriverUnknown, cfg.Driver, registeredDriverNames())
	}
	return f(cfg, deps)
}

// RegisteredDrivers returns a sorted list of registered driver names.
// Useful for boot-log output and `planner.Resolve` error messages.
func RegisteredDrivers() []string {
	factoriesMu.RLock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	factoriesMu.RUnlock()
	sort.Strings(out)
	return out
}

// unregisterForTest removes a driver registration. Exists solely for
// in-package test cleanup so registration tests can re-run without
// leaking driver-table state into sibling subtests (the registry is
// process-global per §4.4 — a left-behind registration corrupts the
// next test's misconfiguration assertions). Unexported so no
// production caller can reach for it; production code never
// unregisters.
func unregisterForTest(name string) {
	factoriesMu.Lock()
	delete(factories, name)
	factoriesMu.Unlock()
}

func registeredDriverNames() string {
	names := RegisteredDrivers()
	if len(names) == 0 {
		return "<none>"
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += n
	}
	return out
}
