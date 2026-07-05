package credsource

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
)

// Credential-source driver registry — the §4.4 seam factory. The env
// and remote drivers self-register from their package init(); the
// production aggregator `internal/drivers/prod` blank-imports each so
// the registration fires at boot. `BuildProviders` dispatches by the
// operator-declared `credential_source` name via [Resolve].

// Config is the boundary type the registry passes to a source factory.
// It carries the union of both drivers' construction inputs; a factory
// reads only the fields its source needs. `BuildProviders` maps the
// operator YAML (`config.ToolOAuthProviderConfig`) onto this struct at
// the boundary so the concrete drivers never import `internal/config`.
type Config struct {
	// ProviderName is the operator-facing provider name — surfaced in
	// boot / fetch error messages and used to attribute fetch events.
	ProviderName string
	// ProviderIndex is the provider's position in
	// `tools.oauth_providers[]`, used to reproduce the historical env
	// fail-loud message shape.
	ProviderIndex int

	// ClientIDEnv / ClientSecretEnv name the env vars the `env` source
	// reads. Empty for the remote source.
	ClientIDEnv     string
	ClientSecretEnv string

	// Remote carries the `remote` source's parameters; nil for env.
	Remote *RemoteConfig

	// HTTPClient is the client the remote source fetches through.
	// Optional — the remote driver applies a bounded default.
	HTTPClient *http.Client
	// Clock is the wall-clock source (cache math). Optional — defaults
	// to time.Now.
	Clock func() time.Time
	// Bus / Redactor let the remote source emit its SafePayload fetch
	// events. Mandatory for the remote source; unused by env.
	Bus      events.EventBus
	Redactor audit.Redactor
}

// RemoteConfig is the `remote` source's operator-declared parameters,
// mapped from `config.ToolOAuthRemoteConfig` at the boundary.
type RemoteConfig struct {
	// URL is the coordinator credential endpoint (the authenticated GET
	// target). Required; validated well-formed at boot.
	URL string
	// AuthTokenEnv names the env var holding the runtime's own service
	// token — sent as `Authorization: Bearer <token>`. Required;
	// validated non-empty at boot (the token itself is read lazily at
	// fetch time so a rotated token is picked up without restart).
	AuthTokenEnv string
	// CacheTTL caps the in-memory serve horizon. Zero = the driver's
	// default cap. The effective horizon is min(response expires_in,
	// CacheTTL).
	CacheTTL time.Duration
	// Timeout bounds a single fetch. Zero = the driver's default.
	Timeout time.Duration
}

// Factory builds a [Source] from a [Config]. Drivers self-register one
// Factory each via init() → [MustRegister].
type Factory func(cfg Config) (Source, error)

// Sentinel errors specific to the registry.
var (
	// ErrSourceUnknown — [Resolve] was called with a name no driver has
	// registered for. The message lists the registered names.
	ErrSourceUnknown = errors.New("credsource: credential source not registered")
	// ErrSourceEmptyName — [Register] was called with an empty name.
	ErrSourceEmptyName = errors.New("credsource: source registration: empty name")
	// ErrSourceNilFactory — [Register] was called with a nil Factory.
	ErrSourceNilFactory = errors.New("credsource: source registration: nil factory")
	// ErrSourceDuplicate — [Register] was called twice for one name
	// (typically a double blank-import).
	ErrSourceDuplicate = errors.New("credsource: source registration: duplicate name")
)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register installs a Factory under name. Drivers self-register from
// their package init(). Re-registering a name returns
// [ErrSourceDuplicate]; the function does NOT panic itself so tests can
// exercise the duplicate path.
func Register(name string, factory Factory) error {
	if name == "" {
		return ErrSourceEmptyName
	}
	if factory == nil {
		return fmt.Errorf("%w: %q", ErrSourceNilFactory, name)
	}
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	if _, exists := factories[name]; exists {
		return fmt.Errorf("%w: %q", ErrSourceDuplicate, name)
	}
	factories[name] = factory
	return nil
}

// MustRegister wraps [Register] and panics on error — the driver-side
// init() idiom. A duplicate-name panic signals a build misconfiguration.
func MustRegister(name string, factory Factory) {
	if err := Register(name, factory); err != nil {
		panic(fmt.Sprintf("credsource.MustRegister(%q): %v", name, err))
	}
}

// Resolve constructs the [Source] for the named credential source by
// dispatching to the registered Factory. Returns wrapped
// [ErrSourceUnknown] — with the registered-source list — when the name
// has no driver.
func Resolve(name string, cfg Config) (Source, error) {
	factoriesMu.RLock()
	f, ok := factories[name]
	factoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q (registered: %s)", ErrSourceUnknown, name, registeredNames())
	}
	return f(cfg)
}

// RegisteredSources returns a sorted list of registered source names.
func RegisteredSources() []string {
	factoriesMu.RLock()
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	factoriesMu.RUnlock()
	sort.Strings(out)
	return out
}

func registeredNames() string {
	names := RegisteredSources()
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

// unregisterForTest removes a registration — in-package test cleanup
// only (the registry is process-global; a left-behind registration
// corrupts a sibling test's misconfiguration assertions). Production
// never unregisters.
func unregisterForTest(name string) {
	factoriesMu.Lock()
	delete(factories, name)
	factoriesMu.Unlock()
}
