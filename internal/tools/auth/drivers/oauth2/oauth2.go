// Package oauth2 ships Harbor's V1 default OAuth provider driver
// (closes issue #116 and the deferred construction gap).
//
// The driver implements the §4.4 seam pattern for OAuth flow
// strategies: it self-registers under the canonical driver name
// `"oauth2"` via init() so operators declaring
// `tools.oauth_providers[].driver: oauth2` in `harbor.yaml` get a
// working provider at boot with zero Go wiring code.
//
// # The OAuth2 + PKCE Authorization Code flow
//
// The driver delegates to `internal/tools/auth.Provider` (the
// concrete `OAuthProvider`), which already implements the full
// Authorization Code + PKCE + RFC 7591 dynamic registration + metadata
// discovery flow. This package's responsibility is the operator-config
// → `Provider` boundary: read env-var-indirected credentials, validate
// fail-loud, build the underlying `auth.OAuthConfig`, and call
// `auth.NewProvider`.
//
// # Source ID binding (V1 simplification)
//
// The `*Provider.Token(ctx, source)` API keys by
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
//   - A nil `cfg.CredentialSource` (BuildProviders always threads one).
//   - A resolved credential with an empty `client_id` (the env var
//     named by `ClientIDEnv` was unset or empty).
//   - A resolved credential with an empty `client_secret`.
//   - Missing `cfg.TokenURL` AND `cfg.AuthURL` (no server endpoints
//     configured; OAuth2/PKCE requires both).
//   - Missing `cfg.RedirectURL`.
//   - Missing deps (Store / Bus / Redactor / Coordinator).
//
// The client credential resolves through the §4.4 credential-source
// seam. The generic `oauth2` driver supports the `env` source only —
// the interactive flow bakes the credential at construction, so it
// resolves EAGERLY here; the config validator restricts `remote` to the
// non-interactive `tokenexchange` driver.
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

// ErrMissingCredentialSource — cfg.CredentialSource was nil at
// construction. BuildProviders always threads one; a nil source is a
// direct-construction bug.
var ErrMissingCredentialSource = errors.New("auth/oauth2: CredentialSource is nil (BuildProviders threads the source; direct callers pass credsource.Static)")

// DriverName is the canonical name the driver registers under. The
// `internal/config` validator's `allowedOAuthDrivers` allowlist mirrors
// this constant.
const DriverName = "oauth2"

// Sentinel errors specific to the oauth2 driver. Driver-internal /
// upstream-server failures continue to use the parent package's
// sentinels (`auth.ErrAuthRequired`, `auth.ErrExchangeFailed`, etc.).
var (
	// ErrMissingClientID — the credential resolved through the source
	// seam carried an empty client_id (the env var named by
	// client_id_env was unset or empty). Fail-loud per §13 amendment.
	ErrMissingClientID = errors.New("auth/oauth2: resolved client_id is empty (the env var named by client_id_env was unset or empty)")
	// ErrMissingClientSecret — the resolved credential carried an empty
	// client_secret. Same fail-loud rationale as `ErrMissingClientID`.
	ErrMissingClientSecret = errors.New("auth/oauth2: resolved client_secret is empty (the env var named by client_secret_env was unset or empty)")
	// ErrMissingEndpoints — both `cfg.AuthURL` and `cfg.TokenURL` are
	// empty. OAuth2/PKCE requires the authorization-server endpoints;
	// driver-specific drivers may infer them, but the generic
	// `oauth2` driver does NOT do discovery — the operator MUST
	// declare both.
	ErrMissingEndpoints = errors.New("auth/oauth2: both auth_url and token_url must be set (the oauth2 driver does not auto-discover endpoints — declare them in tools.oauth_providers[])")
	// ErrMissingRedirectURL — `cfg.RedirectURL` was empty. The
	// redirect_uri names the callback endpoint; `harbor dev` mounts
	// Harbor's production handler (`auth.CallbackHandler`)
	// at `GET /v1/tools/oauth/callback`, so the dev
	// value is `http://<bind>/v1/tools/oauth/callback`. Headless
	// embedders mount the same handler on their own mux (see
	// docs/recipes/steer-and-resume-a-run.md).
	ErrMissingRedirectURL = errors.New("auth/oauth2: redirect_url must not be empty (point it at the mounted auth.CallbackHandler — harbor dev serves GET /v1/tools/oauth/callback)")
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
	if cfg.CredentialSource == nil {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingCredentialSource, cfg.Name)
	}
	// The interactive Authorization-Code flow bakes the client credential
	// into the underlying auth.Provider at construction (the flow itself
	// is unchanged), so the oauth2 driver resolves its
	// credential through the seam EAGERLY here. With the default `env`
	// source this is byte-identical to the historical boot resolution;
	// the config validator restricts `remote` to the non-interactive
	// `tokenexchange` driver (which resolves lazily under an
	// identity-bearing ctx), so this eager resolve is env/static only —
	// no network call, no identity needed.
	cred, err := cfg.CredentialSource.Resolve(context.Background())
	if err != nil {
		return nil, fmt.Errorf("auth/oauth2: resolve client credential (provider name=%q): %w", cfg.Name, err)
	}
	if cred.ClientID == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingClientID, cfg.Name)
	}
	if cred.ClientSecret == "" {
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
	// underlying OAuthConfig must match (Harbor enforces). For V1
	// the driver pins ScopeUser; an entry declaring ScopeAgent against
	// the `oauth2` driver yields a clear runtime error from the
	// Provider rather than a silent mismatch.
	source := tools.ToolSourceID(cfg.Name)
	oauthCfg := auth.OAuthConfig{
		Source:       source,
		SourceName:   cfg.Name,
		BindingScope: auth.ScopeUser,
		ClientID:     cred.ClientID,
		ClientSecret: cred.ClientSecret,
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

	return &provider{
		name:                   cfg.Name,
		source:                 source,
		inner:                  inner,
		allowedDownstreamHosts: append([]string(nil), cfg.AllowedDownstreamHosts...),
	}, nil
}

// provider wraps an `*auth.Provider` so every `Token` / `Revoke` call
// retargets the source argument onto the operator-configured source.
// The catalog wrapper passes the underlying tool's source ID; the
// V1 `oauth2` driver collapses every per-tool source onto the single
// provider-name-keyed OAuthConfig the driver constructs.
//
// `InitiateFlow` / `CompleteFlow` retarget the same way.
//
// Concurrent reuse: the wrapper holds an immutable inner +
// metadata; the underlying `*auth.Provider` is already safe for concurrent reuse
// (per `internal/tools/auth/concurrent_test.go`).
type provider struct {
	name   string
	source tools.ToolSourceID
	inner  auth.OAuthProvider
	// allowedDownstreamHosts is the boot-declared sink allow-list for this
	// provider's southbound binding — the hosts the interactive bearer may
	// be injected into. Set once at construction; the MCP southbound
	// binding refuses a connection host absent from it (fail-closed).
	allowedDownstreamHosts []string
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

// PendingFlow implements OAuthProvider.PendingFlow. State-keyed like
// CompleteFlow — passed through verbatim.
func (p *provider) PendingFlow(state string) (auth.PendingFlowInfo, bool) {
	return p.inner.PendingFlow(state)
}

// DenyFlow implements OAuthProvider.DenyFlow. State-keyed like
// CompleteFlow — passed through verbatim.
func (p *provider) DenyFlow(ctx context.Context, state, reason string) error {
	return p.inner.DenyFlow(ctx, state, reason)
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

// AllowedDownstreamHosts implements OAuthProvider — the boot-declared sink
// allow-list for the interactive binding. A copy is returned so callers
// cannot alias provider state.
func (p *provider) AllowedDownstreamHosts() []string {
	return append([]string(nil), p.allowedDownstreamHosts...)
}
