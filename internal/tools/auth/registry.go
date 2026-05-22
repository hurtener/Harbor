package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// D-095 — OAuth provider driver registry.
//
// The §4.4 seam pattern applied to OAuth flow strategies. The V1
// default driver is `oauth2` (generic OAuth2/PKCE Authorization Code
// flow). New flow types (device-code, client-credentials, vendor-
// specific extensions) add a new driver under
// `internal/tools/auth/drivers/<name>/` without changing this
// registry's shape.
//
// The driver registry exists so operators declare named providers in
// `tools.oauth_providers[]` without writing Go wiring code. The dev
// stack walks the operator config at boot, looks up each entry's
// `Driver` in this registry, and constructs the `OAuthProvider` via
// the registered `Factory`.

// ProviderConfig is the boundary type the registry exposes to drivers.
// It mirrors `config.ToolOAuthProviderConfig` (the operator-facing
// YAML shape) but lives in `internal/tools/auth` so concrete drivers
// can depend on it without forcing the `internal/config` package to
// import driver internals. The dev stack maps the YAML struct onto
// this struct at the boundary.
//
// Credentials enter via env-var indirection (§7 rule 2): `ClientID` /
// `ClientSecret` are the **already-resolved** secret values (the
// dev-stack reads `os.Getenv(cfg.ClientIDEnv)` etc. before calling
// the factory). A driver that finds them empty fails closed.
type ProviderConfig struct {
	Extra        map[string]string
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	RedirectURL  string
	Scopes       []string
}

// FactoryDeps bundles the shared collaborators every OAuth provider
// driver consumes. The dev stack constructs the TokenStore + Sealer
// ONCE (one KEK env var per binary; see
// `config.ToolsConfig.OAuthTokenKEKEnv`) and passes the same instances
// into every factory call — so N declared providers share one token
// store + sealer + bus + redactor + coordinator. This matches Phase
// 30's architecture where one `*Provider` consumes N `OAuthConfig`
// entries (the `oauth2` driver constructs one `*Provider` per
// registry entry; future per-vendor drivers may pool).
type FactoryDeps struct {
	// Store is the shared TokenStore (one per binary). Mandatory.
	Store TokenStore
	// Bus is the shared event bus. Mandatory.
	Bus events.EventBus
	// Redactor is the shared audit redactor. Mandatory.
	Redactor audit.Redactor
	// Coordinator is the shared unified pause/resume primitive.
	// Mandatory.
	Coordinator pauseresume.Coordinator
	// HTTPClient is the HTTP client drivers use to talk to the
	// authorization server. Optional — defaults to
	// `&http.Client{Timeout: 30s}`.
	HTTPClient *http.Client
	// Clock is the wall-clock source. Optional — defaults to
	// `time.Now`.
	Clock func() time.Time
}

// Factory builds an OAuthProvider from a ProviderConfig + FactoryDeps.
// Drivers self-register one Factory each via init() → Register.
//
// A factory MUST fail closed on missing required deps (Store / Bus /
// Redactor / Coordinator); the `internal/tools/auth.NewProvider`
// constructor already enforces this for the `oauth2` driver, but
// custom drivers MUST honour the same contract.
type Factory func(cfg ProviderConfig, deps FactoryDeps) (OAuthProvider, error)

// Sentinel errors specific to the registry. Driver-internal errors
// continue to use the package's existing sentinels (`ErrConfigInvalid`,
// `ErrKEKMissing`, etc.).
var (
	// ErrDriverUnknown — Resolve was called with a name no driver has
	// registered for. The error message lists the registered driver
	// names so the operator sees the typo.
	ErrDriverUnknown = errors.New("auth: oauth provider driver not registered")
	// ErrDriverEmptyName — Register was called with an empty driver
	// name. Build-time configuration bug.
	ErrDriverEmptyName = errors.New("auth: oauth provider driver registration: empty name")
	// ErrDriverNilFactory — Register was called with a nil Factory.
	// Build-time configuration bug.
	ErrDriverNilFactory = errors.New("auth: oauth provider driver registration: nil factory")
	// ErrDriverDuplicate — Register was called twice for the same
	// driver name. Build-time configuration bug (typically a
	// double-blank-import).
	ErrDriverDuplicate = errors.New("auth: oauth provider driver registration: duplicate name")
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
// driver-side idiom: `init() { auth.MustRegister("oauth2", New) }`.
// A duplicate-name panic at init signals a build bug (probably two
// drivers chose the same canonical name); the panic message names the
// offending driver so the operator's stack trace points at the fix.
func MustRegister(name string, factory Factory) {
	if err := Register(name, factory); err != nil {
		panic(fmt.Sprintf("auth.MustRegister(%q): %v", name, err))
	}
}

// Resolve constructs the OAuthProvider for cfg by dispatching to the
// driver named in `driver`. Returns wrapped `ErrDriverUnknown` when
// the driver has not registered; otherwise delegates to the driver's
// Factory (whose own validation surfaces fail-closed errors).
//
// `ctx` is honoured for the construction itself; drivers should
// observe `ctx.Err()` between long phases of work (e.g. HTTP
// discovery probes).
func Resolve(ctx context.Context, driver string, cfg ProviderConfig, deps FactoryDeps) (OAuthProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("auth: Resolve cancelled: %w", err)
	}
	factoriesMu.RLock()
	f, ok := factories[driver]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)",
			ErrDriverUnknown, driver, registeredDriverNames())
	}
	return f(cfg, deps)
}

// RegisteredDrivers returns a sorted list of registered driver names.
// Useful for boot-log output and `auth.Resolve` error messages.
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
