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
// The factory is installed by the production assembly
// (`internal/runtime/assemble.Assemble`, Phase 111a / D-198) whenever
// the operator config declares identity tiers, and cleared when it
// declares none. If unset when `llm.Open` runs, the governance hook is
// a no-op pass-through — preserving the latent default (D-044) for
// callers (especially test code) that don't wire governance explicitly.
package governance

import (
	"log/slog"
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
// twice is allowed (the second call wins) — the production assembly
// (`assemble.Assemble`, the seam's first production caller per Phase
// 111a / D-198) calls this once at boot when `Config.IdentityTiers` is
// non-empty; tests may swap it repeatedly. Concurrent-safe.
//
// Multi-runtime limitation (D-198): the factory is PROCESS-GLOBAL. An
// embedder assembling two runtime stacks with different tier maps in
// one process would collide here — the second SetFactory wins for
// every subsequent `llm.Open`. The binary assembles exactly one stack,
// so the zero-ceremony seam stays; multi-runtime embedders skip
// SetFactory entirely and compose per stack instead:
//
//	sub, err := governance.NewSubsystemFromConfig(cfg, store, bus)
//	client = governance.Wrap(llmClient, sub) // governance outermost, D-043
//
// See `Wrap` and `NewSubsystemFromConfig` for the documented headless
// path.
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
		if err != nil {
			// In production this branch is unreachable: the assembly
			// builds the Subsystem EAGERLY at boot via
			// NewSubsystemFromConfig (a construction failure fails the
			// boot loud, §13) and hands the already-built Subsystem to
			// the factory closure. Only a test-installed factory can
			// error here; warn loudly rather than degrade silently.
			slog.Warn("governance: factory error — composing LLM client WITHOUT enforcement",
				slog.String("error", err.Error()))
			return inner
		}
		if sub == nil {
			// nil Subsystem = the sanctioned latent default (D-044).
			return inner
		}
		return Wrap(inner, sub)
	})
}
