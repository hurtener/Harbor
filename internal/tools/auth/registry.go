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
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// OAuth provider driver registry.
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
// Credentials enter through the [credsource.Source] seam (§7 rule 2 —
// env-var indirection or an authenticated remote pull; never hardcoded,
// never logged). `BuildProviders` constructs the source from the
// provider's declared `credential_source` and threads it here as
// `CredentialSource`; the driver calls `CredentialSource.Resolve(ctx)`
// at its credential-need point. A driver that resolves an empty
// credential fails closed. Direct constructors (headless embedders,
// tests) supply `credsource.Static(id, secret)` so there is exactly ONE
// credential-resolution path (§13, no dual "static fields" path).
type ProviderConfig struct {
	// Name is the operator-facing provider name. Surfaced in error
	// messages.
	Name string
	// CredentialSource resolves the provider's OWN client credential
	// (`client_id` / `client_secret`) — either at boot (env) or lazily
	// at first use (remote). Required; drivers fail closed when nil.
	CredentialSource credsource.Source
	// Scopes is the requested OAuth scope list.
	Scopes []string
	// AuthURL is the authorization endpoint.
	AuthURL string
	// TokenURL is the token endpoint.
	TokenURL string
	// RedirectURL is the redirect_uri the Harbor Protocol callback
	// handler exposes.
	RedirectURL string
	// Extra is the driver-specific extras map. Reserved for future
	// drivers' per-flow knobs; unused by the V1 `oauth2` driver.
	Extra map[string]string
	// AllowedDownstreamHosts is the boot-declared, config/file-only set of
	// downstream connection hosts (host[:port]) a southbound binding may
	// inject this provider's credential into. The credential-plane
	// invariant: no admin-writable field determines a credential sink, so
	// this allow-list is fixed at construction. A bearer-injecting binding
	// whose downstream host is absent is refused fail-closed. Every driver
	// stores it and answers `OAuthProvider.AllowedDownstreamHosts`.
	AllowedDownstreamHosts []string
	// Audience is the boot-declared token-audience ceiling. When non-empty
	// it is the authority for the exchanged token's audience — NOT the
	// caller-chosen provider name — closing the audience-picking lever. An
	// empty Audience preserves the legacy audience-from-name behaviour
	// (backward-compatible; opt-in hardening).
	Audience string
	// ScopeCeiling is the boot-declared scope ceiling. When non-empty the
	// requested scopes (`Scopes`) are INTERSECTED against it — a requested
	// scope outside the ceiling is dropped, never honoured. An empty
	// ceiling preserves the legacy pass-through behaviour.
	ScopeCeiling []string
	// ResourceIndicator is the boot-declared RFC 8707 `resource` value the
	// `tokenexchange` driver carries as the `resource` form parameter on every
	// exchange, and verifies the returned token's `aud` against (when the
	// token is JWT-shaped). Empty preserves today's behaviour (no `resource`
	// sent, no audience check). Ignored by the interactive `oauth2` driver.
	ResourceIndicator string
	// IncludeActorToken opts the `tokenexchange` driver into carrying the
	// run's verified acting principal (`agent_id`, when present on ctx) as an
	// RFC 8693 `actor_token`. Default false. Ignored by `oauth2`.
	IncludeActorToken bool
}

// FactoryDeps bundles the shared collaborators every OAuth provider
// driver consumes. The dev stack constructs the TokenStore + Sealer
// ONCE (one KEK env var per binary; see
// `config.ToolsConfig.OAuthTokenKEKEnv`) and passes the same instances
// into every factory call — so N declared providers share one token
// store + sealer + bus + redactor + coordinator. This matches the
// architecture where one `*Provider` consumes N `OAuthConfig`
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
