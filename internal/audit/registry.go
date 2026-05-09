package audit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/config"
)

// Factory builds a Redactor from an AuditConfig slice. Drivers expose
// one Factory each via init() → Register.
type Factory func(config.AuditConfig) (Redactor, error)

// DefaultDriver is the production driver name. Phase 03 ships only
// `patterns`; later phases may register additional drivers (PII
// tokenizer, semantic redactor) and Open will switch on a
// `cfg.Driver` field once AuditConfig grows one.
const DefaultDriver = "patterns"

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
		panic("audit: Register called with empty name")
	}
	if factory == nil {
		panic(fmt.Sprintf("audit: Register(%q) called with nil factory", name))
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("audit: driver %q already registered", name))
	}
	factories[name] = factory
}

// Open returns a Redactor built by the default driver factory. Phase 03
// always picks DefaultDriver; later phases will read a `cfg.Driver`
// field once AuditConfig grows one. The error wraps ErrUnknownDriver
// when no factory matches and lists registered drivers.
func Open(_ context.Context, cfg config.AuditConfig) (Redactor, error) {
	return open(DefaultDriver, cfg)
}

// OpenDriver builds a Redactor from a specific driver name. Useful for
// tests that want to exercise the registry against a non-default driver
// without round-tripping through config.
func OpenDriver(name string, cfg config.AuditConfig) (Redactor, error) {
	return open(name, cfg)
}

func open(name string, cfg config.AuditConfig) (Redactor, error) {
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
// boot-log output ("audit drivers available: patterns") and for
// surfacing in error messages.
func RegisteredDrivers() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
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
