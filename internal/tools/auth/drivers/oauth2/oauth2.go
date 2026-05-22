// Package oauth2 ships Harbor's V1 default OAuth provider driver
// (D-095, closes issue #116 and D-090's deferred construction gap).
//
// The driver implements the §4.4 seam pattern for OAuth flow
// strategies: it self-registers under the canonical driver name
// `"oauth2"` via init() so operators declaring
// `tools.oauth_providers[].driver: oauth2` in `harbor.yaml` get a
// working provider at boot with zero Go wiring code.
//
// # The OAuth2 + PKCE Authorization Code flow
//
// The driver delegates to `internal/tools/auth.Provider` (the Phase 30
// concrete `OAuthProvider`), which already implements the full
// Authorization Code + PKCE + RFC 7591 dynamic registration + metadata
// discovery flow. This package's responsibility is the operator-config
// → `Provider` boundary: read env-var-indirected credentials, validate
// fail-loud, build the underlying `auth.OAuthConfig`, and call
// `auth.NewProvider`.
//
// # Source ID binding (V1 simplification)
//
// The Phase 30 `*Provider.Token(ctx, source)` API keys by
// `tools.ToolSourceID` — each provider holds one `OAuthConfig` per
// source. For V1, the `oauth2` driver constructs ONE `*Provider` per
// `tools.oauth_providers[]` entry with a single `OAuthConfig` whose
// `Source = tools.ToolSourceID(cfg.Name)`. The catalog wrapper
// (`internal/tools/catalog.WrapWithOAuth`) passes the underlying
// tool's source ID, which may not match the provider name; the driver
// transparently substitutes by wrapping the `*Provider` in a small
// adapter that retargets every `Token` call onto the operator-
// configured source.
//
// Future per-vendor drivers (Google Workspace, GitHub, Slack, ...) may
// implement more sophisticated multi-source mappings; the V1 default
// keeps the operator's mental model simple: one provider declaration
// → one OAuth attachment.
//
// # Fail-loud at construction
//
// CLAUDE.md §13 amendment — operator-facing seams demand explicit
// configuration. The driver fails closed on:
//
//   - Empty `cfg.ClientID` (the env var named by `ClientIDEnv` was
//     unset or empty).
//   - Empty `cfg.ClientSecret` (the env var named by
//     `ClientSecretEnv` was unset or empty).
//   - Missing `cfg.TokenURL` AND `cfg.AuthURL` (no server endpoints
//     configured; OAuth2/PKCE requires both).
//   - Missing `cfg.RedirectURL`.
//   - Missing deps (Store / Bus / Redactor / Coordinator).
//
// Every failure mode surfaces a wrapped error naming the offending
// config field; the dev stack propagates the error up so the boot
// banner names the field the operator needs to set.
package oauth2

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// DriverName is the canonical name the driver registers under. The
// `internal/config` validator's `allowedOAuthDrivers` allowlist mirrors
// this constant.
const DriverName = "oauth2"

// Sentinel errors specific to the oauth2 driver. Driver-internal /
// upstream-server failures continue to use the parent package's
// sentinels (`auth.ErrAuthRequired`, `auth.ErrExchangeFailed`, etc.).
var (
	// ErrMissingClientID — `cfg.ClientID` was empty at construction.
	// The dev stack resolves `os.Getenv(ClientIDEnv)` before calling
	// the factory; an empty value means the env var was unset or
	// empty. Fail-loud per §13 amendment.
	ErrMissingClientID = errors.New("auth/oauth2: ClientID is empty (the env var named by client_id_env was unset or empty)")
	// ErrMissingClientSecret — `cfg.ClientSecret` was empty at
	// construction. Same fail-loud rationale as `ErrMissingClientID`.
	ErrMissingClientSecret = errors.New("auth/oauth2: ClientSecret is empty (the env var named by client_secret_env was unset or empty)")
	// ErrMissingEndpoints — both `cfg.AuthURL` and `cfg.TokenURL` are
	// empty. OAuth2/PKCE requires the authorization-server endpoints;
	// driver-specific drivers may infer them, but the generic
	// `oauth2` driver does NOT do discovery — the operator MUST
	// declare both.
	ErrMissingEndpoints = errors.New("auth/oauth2: both auth_url and token_url must be set (the oauth2 driver does not auto-discover endpoints — declare them in tools.oauth_providers[])")
	// ErrMissingRedirectURL — `cfg.RedirectURL` was empty.
	ErrMissingRedirectURL = errors.New("auth/oauth2: redirect_url must not be empty (the redirect_uri the Harbor Protocol callback handler exposes)")
)

// init self-registers the `oauth2` driver under its canonical name.
// `cmd/harbor/main.go` blank-imports this package so the registration
// fires at process boot (§4.4 seam pattern).
func init() {
	auth.MustRegister(DriverName, New)
}

// New constructs the `oauth2` driver's OAuthProvider for one
// operator-config entry.
//
// The function is registered as the `oauth2` driver's `auth.Factory`.
// Per §4.4 the dev stack never calls this directly — `auth.Resolve`
// dispatches by driver name.
func New(cfg auth.ProviderConfig, deps auth.FactoryDeps) (auth.OAuthProvider, error) {
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingClientID, cfg.Name)
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingClientSecret, cfg.Name)
	}
	if cfg.AuthURL == "" || cfg.TokenURL == "" {
		return nil, fmt.Errorf("%w (provider name=%q, auth_url=%q, token_url=%q)",
			ErrMissingEndpoints, cfg.Name, cfg.AuthURL, cfg.TokenURL)
	}
	if cfg.RedirectURL == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingRedirectURL, cfg.Name)
	}

	// The V1 binding-scope choice for the generic `oauth2` driver:
	// the provider serves ScopeUser by default. Operators declaring a
	// shared-service-account flow (ScopeAgent) compose with a future
	// per-vendor driver that pins the agent identity. The catalog
	// entry's `oauth.binding_scope` field steers the wrapper but the
	// underlying OAuthConfig must match (Phase 30 enforces). For V1
	// the driver pins ScopeUser; an entry declaring ScopeAgent against
	// the `oauth2` driver yields a clear runtime error from the
	// Provider rather than a silent mismatch.
	source := tools.ToolSourceID(cfg.Name)
	oauthCfg := auth.OAuthConfig{
		Source:       source,
		SourceName:   cfg.Name,
		BindingScope: auth.ScopeUser,
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		AuthorizeURL: cfg.AuthURL,
		TokenURL:     cfg.TokenURL,
		RedirectURI:  cfg.RedirectURL,
		Scopes:       append([]string(nil), cfg.Scopes...),
	}

	inner, err := auth.NewProvider([]auth.OAuthConfig{oauthCfg}, auth.ProviderDeps{
		Store:       deps.Store,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Coordinator: deps.Coordinator,
		HTTPClient:  deps.HTTPClient,
		Clock:       deps.Clock,
	})
	if err != nil {
		return nil, fmt.Errorf("auth/oauth2: NewProvider (provider name=%q): %w", cfg.Name, err)
	}

	return &provider{name: cfg.Name, source: source, inner: inner}, nil
}

// provider wraps an `*auth.Provider` so every `Token` / `Revoke` call
// retargets the source argument onto the operator-configured source.
// The catalog wrapper passes the underlying tool's source ID; the
// V1 `oauth2` driver collapses every per-tool source onto the single
// provider-name-keyed OAuthConfig the driver constructs.
//
// `InitiateFlow` / `CompleteFlow` retarget the same way.
//
// Concurrent reuse (D-025): the wrapper holds an immutable inner +
// metadata; the underlying `*auth.Provider` is already D-025-safe
// (per `internal/tools/auth/concurrent_test.go`).
type provider struct {
	inner  auth.OAuthProvider
	name   string
	source tools.ToolSourceID
}

// Token implements OAuthProvider.Token.
//
// The `requested` source is retargeted onto the operator-configured
// source. The retargeting is intentional — see the package godoc for
// the V1 source-ID simplification.
func (p *provider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	// Identity is mandatory (CLAUDE.md §6 rule 9). The inner Provider
	// enforces this; we surface a wrapped error here so the trace
	// points back at the oauth2 driver wrapper.
	if _, ok := identity.From(ctx); !ok {
		return auth.Token{}, fmt.Errorf("auth/oauth2: Token (provider name=%q): %w",
			p.name, auth.ErrIdentityRequired)
	}
	return p.inner.Token(ctx, p.source)
}

// InitiateFlow implements OAuthProvider.InitiateFlow.
func (p *provider) InitiateFlow(ctx context.Context, _ tools.ToolSourceID) (auth.FlowInitiation, error) {
	if _, ok := identity.From(ctx); !ok {
		return auth.FlowInitiation{}, fmt.Errorf("auth/oauth2: InitiateFlow (provider name=%q): %w",
			p.name, auth.ErrIdentityRequired)
	}
	return p.inner.InitiateFlow(ctx, p.source)
}

// CompleteFlow implements OAuthProvider.CompleteFlow. The flow state
// is keyed by `state`, not by source, so the retargeting is a no-op
// here — passed through verbatim.
func (p *provider) CompleteFlow(ctx context.Context, state, code string) (auth.Token, error) {
	return p.inner.CompleteFlow(ctx, state, code)
}

// Revoke implements OAuthProvider.Revoke.
func (p *provider) Revoke(ctx context.Context, _ tools.ToolSourceID) error {
	if _, ok := identity.From(ctx); !ok {
		return fmt.Errorf("auth/oauth2: Revoke (provider name=%q): %w",
			p.name, auth.ErrIdentityRequired)
	}
	return p.inner.Revoke(ctx, p.source)
}

// Close implements OAuthProvider.Close.
func (p *provider) Close(ctx context.Context) error {
	return p.inner.Close(ctx)
}
