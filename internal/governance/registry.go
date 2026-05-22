// Package governance — registry.
//
// Governance composes OUTSIDE the rest of the LLM-edge chain via the
// `llm.RegisterGovernanceWrapper` hook. The wrapper is INSTALLED here
// (from `init()`), but only FIRES when a registered factory has been
// supplied via `SetFactory`. The factory takes a `llm.ConfigSnapshot` +
// `llm.Deps` and returns the per-`llm.Open` Subsystem (or nil to leave
// governance latent for this caller).
//
// Why a factory instead of a global singleton: each `llm.Open` may run
// with different `Config` (different tier maps, different StateStore for
// tests). Phase 36a's accumulator is a long-lived sibling of the
// LLMClient — operators build one per Open and dispose with the client.
//
// The factory is set ONCE per process by the runtime bootstrap. If unset
// when `llm.Open` runs, the governance hook is a no-op pass-through —
// preserving the latent default for callers (especially test code) that
// don't wire governance explicitly.
package governance

import (
	"sync"

	"github.com/hurtener/Harbor/internal/llm"
)

var (
	factoryMu sync.RWMutex
	factory   Factory
)

// Factory builds a `Subsystem` from the same `(cfg, deps)` pair that
// `llm.Open` consumes. Returning a nil Subsystem disables governance
// for this Open invocation (the wrapper passes through unchanged) —
// useful for tests that share a single `cmd/harbor` blank-import with
// production code but don't want governance to fire.
type Factory func(cfg llm.ConfigSnapshot, deps llm.Deps) (Subsystem, error)

// SetFactory installs the per-`llm.Open` factory. Calling SetFactory
// twice is allowed (the second call wins) — Phase 36a's bootstrap path
// typically calls this once at process start; tests may swap it
// repeatedly. Concurrent-safe.
func SetFactory(f Factory) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factory = f
}

// ClearFactory removes the registered factory. Restores the latent
// default behaviour. Concurrent-safe.
func ClearFactory() {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factory = nil
}

func currentFactory() Factory {
	factoryMu.RLock()
	defer factoryMu.RUnlock()
	return factory
}

// init installs the wrapper hook on the LLM registry. The hook checks
// the registered factory each call; absence → pass-through.
func init() {
	llm.RegisterGovernanceWrapper(func(inner llm.LLMClient, cfg llm.ConfigSnapshot, deps llm.Deps) llm.LLMClient {
		f := currentFactory()
		if f == nil {
			return inner
		}
		sub, err := f(cfg, deps)
		if err != nil || sub == nil {
			// Construction failure is operator-visible at the factory
			// site; Open's registry path should never see a nil
			// factory. We fall back to pass-through rather than fail
			// the entire LLM client open — Wave 7b ships LATENT.
			return inner
		}
		return Wrap(inner, sub)
	})
}
