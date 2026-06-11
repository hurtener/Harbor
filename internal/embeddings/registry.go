package embeddings

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Deps carries the runtime dependencies an embedding driver
// consumes. Deliberately empty at this revision: the embedding seam
// is dependency-light by design — an embeddings-only consumer should
// not have to wire an artifact store or an event bus the way a chat
// consumer does. The struct exists so the factory signature
// (`Open(ctx, cfg, deps)`) stays stable when future revisions add a
// dependency (e.g. governance metering).
type Deps struct{}

// ConfigSnapshot is the strict subset of `config.EmbeddingsConfig`
// the embeddings package consumes. Keeping a snapshot decouples
// drivers from the config package's type evolution (the same
// pattern the chat client and memory subsystems use). Callers
// either project the operator config via `SnapshotFromConfig` or
// construct the snapshot directly — both paths are first-class.
//
//   - `Driver` selects the §4.4 factory. Empty defaults to
//     `DefaultDriver`.
//   - `Provider` / `Model` / `APIKey` / `BaseURL` / `Timeout` are
//     the provider-gateway knobs, configured separately from the
//     chat model: the embedding model is its own operator choice.
//     `APIKey` accepts a literal value or an `env.NAME` reference,
//     matching the chat client's convention.
//   - `Dimensions`, when > 0, requests a reduced output dimension
//     from providers that support it. Zero leaves the model's
//     native dimension.
type ConfigSnapshot struct {
	Driver     string
	Provider   string
	Model      string
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	Dimensions int
}

// Factory builds an `Embedder` from a `ConfigSnapshot` + `Deps`.
// Drivers expose one `Factory` each via `init()` → `Register`.
type Factory func(cfg ConfigSnapshot, deps Deps) (Embedder, error)

// DefaultDriver is the production embedding driver name. There is
// no mock or stub default: a consumer that enables an
// embedding-backed mode without a configured provider fails loudly
// at construction (AGENTS.md §13 — no test stubs as production
// defaults).
const DefaultDriver = "bifrost"

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a driver factory under `name`. Drivers self-
// register from their package `init()`; the production driver
// aggregator (`internal/drivers/prod`) blank-imports them so the
// binary and embedders pick the registrations up at boot. Per
// AGENTS.md §4.4.
//
// Re-registering the same name panics — the registration model is
// write-once-at-init and a duplicate signals a build misconfig.
func Register(name string, factory Factory) {
	if name == "" {
		panic("embeddings: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("embeddings: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("embeddings: driver %q already registered", name))
	}
	factories[name] = factory
}

// RegisteredDrivers returns a sorted list of driver names. Useful
// for boot-log emission and for surfacing in error messages.
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

// Open returns the `Embedder` built by the factory whose name
// matches `cfg.Driver` (defaults to `DefaultDriver` when empty).
//
// The returned client enforces the embedding-edge contract by
// construction — every `Embed` through the registry path:
//
//   - fails closed with `ErrIdentityMissing` when `ctx` carries no
//     Harbor identity (mirroring the chat edge);
//   - rejects empty input (`ErrEmptyInput`) before any provider
//     traffic;
//   - honours `ctx` cancellation between phases of work.
//
// Misconfiguration errors name the offending field; an unknown
// driver error lists the registered drivers.
func Open(_ context.Context, cfg ConfigSnapshot, deps Deps) (Embedder, error) {
	if err := validateSnapshot(cfg); err != nil {
		return nil, err
	}
	name := cfg.Driver
	if name == "" {
		name = DefaultDriver
	}
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownDriver, name, registeredNames())
	}
	drv, err := f(cfg, deps)
	if err != nil {
		return nil, fmt.Errorf("embeddings: driver %q construction failed: %w", name, err)
	}
	return &guardedEmbedder{inner: drv}, nil
}

// validateSnapshot checks the structural invariants every driver
// depends on. Driver-specific requirements (provider / API key)
// live in the driver constructor so the error can name the exact
// provider convention.
func validateSnapshot(cfg ConfigSnapshot) error {
	if cfg.Dimensions < 0 {
		return fmt.Errorf("%w: Dimensions=%d must be >= 0", ErrInvalidConfig, cfg.Dimensions)
	}
	if cfg.Timeout < 0 {
		return fmt.Errorf("%w: Timeout=%s must be >= 0", ErrInvalidConfig, cfg.Timeout)
	}
	return nil
}

// guardedEmbedder is the registry-path wrapper that makes the
// embedding-edge contract unbypassable: identity-mandatory,
// non-empty input, cancellation-aware. Drivers re-check identity
// defensively, but callers constructed through `Open` cannot reach
// a driver without passing this guard.
//
// Concurrent reuse: the wrapper is stateless; the inner driver owns
// its own synchronisation.
type guardedEmbedder struct {
	inner Embedder
}

// Embed implements Embedder.
func (g *guardedEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !HasIdentity(ctx) {
		return nil, ErrIdentityMissing
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w: no texts supplied", ErrEmptyInput)
	}
	for i, t := range texts {
		if t == "" {
			return nil, fmt.Errorf("%w: texts[%d] is empty", ErrEmptyInput, i)
		}
	}
	out, err := g.inner.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embeddings: driver returned %d vectors for %d texts", len(out), len(texts))
	}
	return out, nil
}

// Close implements Embedder.
func (g *guardedEmbedder) Close(ctx context.Context) error {
	return g.inner.Close(ctx)
}

// Compile-time assertion.
var _ Embedder = (*guardedEmbedder)(nil)
