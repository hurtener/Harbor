// build_providers.go — the exported OAuth provider assembly. Absorbs
// cmd/harbor's applyToolCatalogWiring KEK-resolve → sealer → token store
// → provider-factory loop so cmd and devstack share one implementation.
//
// BuildProviders walks `cfg.OAuthProviders[]`, constructs the shared
// crypto chain ONCE (one operator-supplied KEK env var per binary →
// AES-256-GCM Sealer → StateStore-backed TokenStore), and dispatches
// each entry to the §4.4 driver registry by `Driver` name. Each entry's
// OWN client credential resolves through the §4.4 credential-source seam
// (`internal/tools/auth/credsource`): `env` (the default — resolved at
// boot from `ClientIDEnv` / `ClientSecretEnv`, fail-loud when unset) or
// `remote` (an authenticated pull from a coordinator endpoint at first
// need). BuildProviders constructs the source, calls its boot-time
// `ValidateAtBoot`, and threads it onto `ProviderConfig.CredentialSource`;
// the driver resolves the credential at its need-point (CLAUDE.md §7
// rule 2 — never hardcoded, never logged). The KEK is read from the env
// var named in `cfg.OAuthTokenKEKEnv` (32 hex bytes; the Sealer enforces
// length). Every failure is loud: empty / wrong-length KEK, an unset env
// credential, a malformed remote block, unknown driver / source, and
// factory errors all crash assembly with a wrapped error naming the
// offending field (CLAUDE.md §13 amendment).
package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// BuildDeps bundles the shared runtime collaborators BuildProviders
// threads into every provider factory. All four fields are mandatory
// when at least one provider is declared; an empty
// `cfg.OAuthProviders` list short-circuits before any is read.
type BuildDeps struct {
	// State is the runtime's StateStore — the TokenStore persists
	// sealed tokens through it.
	State state.StateStore
	// Bus is the shared event bus (auth events).
	Bus events.EventBus
	// Redactor is the shared audit redactor.
	Redactor audit.Redactor
	// Coordinator is the unified pause/resume primitive — the ONE
	// pause path tool-side OAuth rides on (CLAUDE.md §7 rule 4).
	Coordinator pauseresume.Coordinator
}

// BuildProviders constructs the OAuth provider map declared by
// `cfg.OAuthProviders`, keyed by provider Name. An empty declaration
// list returns an empty (non-nil) map and never touches the KEK env —
// the binary boots cleanly when no operator declares OAuth bindings.
func BuildProviders(ctx context.Context, cfg config.ToolsConfig, deps BuildDeps) (map[string]OAuthProvider, error) {
	providers := make(map[string]OAuthProvider, len(cfg.OAuthProviders))
	if len(cfg.OAuthProviders) == 0 {
		return providers, nil
	}
	kek, err := resolveTokenKEK(cfg.OAuthTokenKEKEnv)
	if err != nil {
		return nil, err
	}
	sealer, err := NewAESGCMSealer(kek)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: sealer: %w", err)
	}
	tokenStore, err := NewTokenStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: token store: %w", err)
	}
	factoryDeps := FactoryDeps{
		Store:       tokenStore,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Coordinator: deps.Coordinator,
	}
	for i, p := range cfg.OAuthProviders {
		src, err := buildCredentialSource(i, p, deps)
		if err != nil {
			return nil, err
		}
		// The env source resolves + fails loud here (today's boot
		// behavior); the remote source validates only the block shape and
		// defers the network pull to first use.
		if err := src.ValidateAtBoot(ctx); err != nil {
			return nil, err
		}
		pcfg := ProviderConfig{
			Name:                   p.Name,
			CredentialSource:       src,
			Scopes:                 append([]string(nil), p.Scopes...),
			AuthURL:                p.AuthURL,
			TokenURL:               p.TokenURL,
			RedirectURL:            p.RedirectURL,
			Extra:                  p.Extra,
			AllowedDownstreamHosts: append([]string(nil), p.AllowedDownstreamHosts...),
			Audience:               p.Audience,
			ScopeCeiling:           append([]string(nil), p.ScopeCeiling...),
			ResourceIndicator:      p.ResourceIndicator,
			IncludeActorToken:      p.IncludeActorToken,
			AllowPrivateTokenURL:   p.AllowPrivateTokenURL,
		}
		prov, err := Resolve(ctx, p.Driver, pcfg, factoryDeps)
		if err != nil {
			return nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d], driver=%q): %w",
				p.Name, i, p.Driver, err)
		}
		providers[p.Name] = prov
	}
	return providers, nil
}

// TokenExchangeDriverName is the driver name a Protocol-installed provider
// descriptor MUST declare (the only writable driver — the non-interactive PULL
// exchange). Mirrors the tokenexchange driver's self-registration name
// without importing the driver package here.
const TokenExchangeDriverName = "tokenexchange"

// ProviderBuilder constructs Protocol-installed broker-pull provider instances
// at runtime from a boot-declared named credential broker plus a wire
// descriptor's non-secret scope subset. Every credential-sink-determining value
// (the token endpoint, the credential-pull endpoint, the allowed downstream
// hosts, the audience, and the scope ceiling) is read from the boot broker —
// NONE from the descriptor — so no admin-writable field determines where a
// credential is sent (the credential-plane invariant). It is built ONCE at boot
// (the shared crypto chain constructed alongside BuildProviders) and is safe
// for concurrent reuse (immutable after construction).
type ProviderBuilder struct {
	brokers     map[string]config.ToolOAuthCredentialBrokerConfig
	factoryDeps FactoryDeps
	bus         events.EventBus
	redactor    audit.Redactor
}

// ProviderBuilder sentinel errors.
var (
	// ErrUnknownBroker — a descriptor's credential_broker resolves to no
	// boot-declared broker. Loud, listing the declared broker names.
	ErrUnknownBroker = errors.New("auth: credential_broker resolves to no boot-declared broker")
	// ErrBrokerMissingCredentialURL — the resolved broker declares no
	// credential_url, so an installed broker-pull provider has no boot-pinned
	// endpoint to PULL its org client credential from. Fail loud (never a silent
	// wire-supplied fallback).
	ErrBrokerMissingCredentialURL = errors.New("auth: credential_broker declares no credential_url (a Protocol-installed broker-pull provider pulls its org client credential from the boot-pinned credential_url)")
)

// NewProviderBuilder builds the runtime provider builder from the boot config.
// It constructs the shared crypto chain (KEK → sealer → token store) ONCE so an
// installed provider shares the same token store as the boot providers, and
// captures the boot broker set by name. When no broker is declared it returns a
// builder whose Build fails loud with ErrUnknownBroker (nothing to resolve
// against) — never a nil-panic. deps' four fields are mandatory when at least
// one broker is declared.
func NewProviderBuilder(ctx context.Context, cfg config.ToolsConfig, deps BuildDeps) (*ProviderBuilder, error) {
	brokers := make(map[string]config.ToolOAuthCredentialBrokerConfig, len(cfg.OAuthCredentialBrokers))
	for _, b := range cfg.OAuthCredentialBrokers {
		brokers[b.Name] = b
	}
	pb := &ProviderBuilder{brokers: brokers, bus: deps.Bus, redactor: deps.Redactor}
	if len(brokers) == 0 {
		return pb, nil
	}
	kek, err := resolveTokenKEK(cfg.OAuthTokenKEKEnv)
	if err != nil {
		return nil, err
	}
	sealer, err := NewAESGCMSealer(kek)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: broker provider builder: sealer: %w", err)
	}
	tokenStore, err := NewTokenStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: broker provider builder: token store: %w", err)
	}
	pb.factoryDeps = FactoryDeps{
		Store:       tokenStore,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Coordinator: deps.Coordinator,
	}
	return pb, nil
}

// BrokerNames returns the sorted boot-declared broker names (for a fail-loud
// error message and the Console's broker picker feed).
func (b *ProviderBuilder) BrokerNames() []string {
	out := make([]string, 0, len(b.brokers))
	for name := range b.brokers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Build constructs a broker-pull provider instance named `name`, resolving
// `brokerName` against the boot broker set and requesting `scopes` (clamped to
// the broker's boot scope ceiling by the tokenexchange driver). An unknown
// broker or a broker without a credential_url fails loud; the credential source
// is the boot-pinned remote PULL. The descriptor NEVER contributes a
// URL, an env-var name, or a secret — only the scope subset.
func (b *ProviderBuilder) Build(ctx context.Context, name, brokerName string, scopes []string) (OAuthProvider, error) {
	broker, ok := b.brokers[brokerName]
	if !ok {
		return nil, fmt.Errorf("%w: %q (declared: %v)", ErrUnknownBroker, brokerName, b.BrokerNames())
	}
	if broker.CredentialURL == "" {
		return nil, fmt.Errorf("%w: broker %q", ErrBrokerMissingCredentialURL, brokerName)
	}
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: name,
		Bus:          b.bus,
		Redactor:     b.redactor,
		Clock:        time.Now,
		Remote: &credsource.RemoteConfig{
			URL:          broker.CredentialURL,
			AuthTokenEnv: broker.AuthTokenEnv,
			CacheTTL:     broker.CacheTTL,
			Timeout:      broker.Timeout,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: broker %q credential source: %w", brokerName, err)
	}
	if err := src.ValidateAtBoot(ctx); err != nil {
		return nil, fmt.Errorf("tools/oauth: broker %q credential source: %w", brokerName, err)
	}
	pcfg := ProviderConfig{
		Name:                   name,
		CredentialSource:       src,
		Scopes:                 append([]string(nil), scopes...),
		TokenURL:               broker.TokenURL,
		AllowedDownstreamHosts: append([]string(nil), broker.AllowedDownstreamHosts...),
		Audience:               broker.Audience,
		ScopeCeiling:           append([]string(nil), broker.ScopeCeiling...),
	}
	prov, err := Resolve(ctx, TokenExchangeDriverName, pcfg, b.factoryDeps)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: install provider %q (broker=%q, driver=%q): %w", name, brokerName, TokenExchangeDriverName, err)
	}
	return prov, nil
}

// WireProviderDescriptor is the non-secret input to [ProviderBuilder.BuildWire]:
// a FULL OAuth-provider binding carried over the wire behind the dev-only
// `allow_wire_oauth_descriptor` opt-in. Every field is non-secret — RemoteAuthTokenEnv
// NAMES an env var (the token is read from the process environment), never the
// token itself. AllowedDownstreamHosts is DERIVED by the caller from the bound
// connection's own URL (never a wire field); the two URLs (TokenURL, RemoteURL)
// are dialed through the token-exchange SSRF backstop.
type WireProviderDescriptor struct {
	// Name is the installed provider name.
	Name string
	// TokenURL is the RFC-8693 token-exchange endpoint (wire-carried).
	TokenURL string
	// Audience is the exchanged token's audience (wire-carried; optional).
	Audience string
	// Scopes is the requested scope subset (wire-carried; optional).
	Scopes []string
	// RemoteURL is the coordinator credential-pull endpoint (wire-carried).
	RemoteURL string
	// RemoteAuthTokenEnv names the env var holding the runtime's own service
	// token used to authenticate the pull. NON-SECRET (a name).
	RemoteAuthTokenEnv string
	// AllowedDownstreamHosts is the DERIVED downstream sink allow-list (from the
	// bound connection's URL). May be empty (a set_oauth_provider install with no
	// bound connection yet — the provider is unbindable until a connection derives
	// its sink); a bound connection ALWAYS supplies exactly one host.
	AllowedDownstreamHosts []string
}

// ErrWireDescriptorIncomplete — a wire provider descriptor is missing a required
// field (token_url or the remote credential-pull block). Loud, fail-closed.
var ErrWireDescriptorIncomplete = errors.New("auth: wire oauth provider descriptor is incomplete (token_url and remote{url,auth_token_env} are required for the dev-gated full binding)")

// BuildWire constructs a broker-pull `tokenexchange` provider from a wire-carried
// full binding (the dev-only `allow_wire_oauth_descriptor` path). Unlike [Build]
// (which reads every sink from a boot-declared broker), the token endpoint, the
// audience, and the credential-pull endpoint come from the wire descriptor —
// bounded by two guards that keep the credential-plane relaxation honest:
//
//   - The exchanged token's downstream sink is the caller-DERIVED
//     AllowedDownstreamHosts (from the bound connection's own URL, never a wire
//     field), so a token can only ever be injected into the endpoint the
//     connection actually dials.
//   - Both wire URLs face the token-exchange SSRF backstop: the exchange POST
//     through the driver's own hardened client (private / link-local / ULA /
//     unspecified refused post-DNS, every redirect refused, no proxy — subject to
//     the independent dev-only private-dial opt-in), and the credential PULL through
//     a client hardened HERE with the same private-dial + redirect refusal (the
//     pull carries the runtime's service bearer, so a redirecting or private-range
//     pull endpoint is a fault).
//
// No secret rides the wire: the pull reads its bearer from the env var NAMED by
// RemoteAuthTokenEnv at fetch time.
func (b *ProviderBuilder) BuildWire(ctx context.Context, desc WireProviderDescriptor) (OAuthProvider, error) {
	if desc.TokenURL == "" || desc.RemoteURL == "" || desc.RemoteAuthTokenEnv == "" {
		return nil, fmt.Errorf("%w (name=%q)", ErrWireDescriptorIncomplete, desc.Name)
	}
	if b.factoryDeps.Store == nil {
		// No broker was declared at boot ⇒ the shared crypto chain (KEK → sealer
		// → token store) was never built, so a wire provider has no token store to
		// share. Fail loud rather than nil-panic — the operator opted into the
		// wire descriptor but declared no `oauth_token_kek_env`.
		return nil, fmt.Errorf("auth: wire oauth provider %q: no boot token store (set tools.oauth_token_kek_env and declare at least one oauth_credential_broker to seed the shared crypto chain)", desc.Name)
	}
	// The credential PULL carries the runtime's service bearer — harden its
	// client to the same bar as the token-exchange POST: refuse a private-range /
	// link-local / ULA / unspecified resolved address post-DNS (the pull URL is
	// wire-derived, so unlike a boot-declared broker it is attacker-influenceable
	// when the operator opts in), no proxy. The credsource/remote source adds its
	// own redirect refusal. The pull's private-dial refusal is UNCONDITIONAL (the
	// dev-only private-dial opt-in is scoped to the token-exchange POST, not the pull).
	pullClient := hardenedWirePullClient(defaultWirePullTimeout)
	src, err := credsource.Resolve(credsource.SourceRemote, credsource.Config{
		ProviderName: desc.Name,
		Bus:          b.bus,
		Redactor:     b.redactor,
		Clock:        time.Now,
		HTTPClient:   pullClient,
		Remote: &credsource.RemoteConfig{
			URL:          desc.RemoteURL,
			AuthTokenEnv: desc.RemoteAuthTokenEnv,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auth: wire oauth provider %q credential source: %w", desc.Name, err)
	}
	if err := src.ValidateAtBoot(ctx); err != nil {
		return nil, fmt.Errorf("auth: wire oauth provider %q credential source: %w", desc.Name, err)
	}
	pcfg := ProviderConfig{
		Name:                   desc.Name,
		CredentialSource:       src,
		Scopes:                 append([]string(nil), desc.Scopes...),
		TokenURL:               desc.TokenURL,
		Audience:               desc.Audience,
		AllowedDownstreamHosts: append([]string(nil), desc.AllowedDownstreamHosts...),
	}
	prov, err := Resolve(ctx, TokenExchangeDriverName, pcfg, b.factoryDeps)
	if err != nil {
		return nil, fmt.Errorf("auth: wire oauth provider %q (driver=%q): %w", desc.Name, TokenExchangeDriverName, err)
	}
	return prov, nil
}

// defaultWirePullTimeout bounds a single wire credential-pull fetch.
const defaultWirePullTimeout = 30 * time.Second

// hardenedWirePullClient builds the HTTP client the wire-carried credential PULL
// fetches through: a post-DNS dial control that refuses a private-range /
// link-local / ULA / unspecified resolved address (the SSRF backstop for the
// wire-derived, attacker-influenceable pull URL) and no proxy. The
// credsource/remote source adds the redirect refusal (the pull carries a service
// bearer; a redirect is a replay fault). Loopback is allowed (a localhost
// coordinator / fixture is a legitimate dev deployment, mirroring the
// token-exchange loopback carve-out). The private-dial refusal is UNCONDITIONAL:
// unlike the token-exchange POST it has no dev-only private-dial relaxation.
func hardenedWirePullClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			host = address
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("wire oauth credential pull: unresolved dial address %q", address)
		}
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return fmt.Errorf("wire oauth credential pull: dial to a private/link-local address %q refused (the pull carries the runtime service bearer)", host)
		}
		return nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: dialer.DialContext,
		},
	}
}

// buildCredentialSource constructs the §4.4 credential source declared by
// the provider entry (defaulting to `env`), mapping the operator YAML
// onto the neutral `credsource.Config` boundary. The remote source
// receives the shared bus + redactor so it can emit its SafePayload fetch
// events; the env source ignores them.
func buildCredentialSource(index int, p config.ToolOAuthProviderConfig, deps BuildDeps) (credsource.Source, error) {
	name := p.CredentialSource
	if name == "" {
		name = credsource.SourceEnv
	}
	scfg := credsource.Config{
		ProviderName:    p.Name,
		ProviderIndex:   index,
		ClientIDEnv:     p.ClientIDEnv,
		ClientSecretEnv: p.ClientSecretEnv,
		Clock:           time.Now,
		Bus:             deps.Bus,
		Redactor:        deps.Redactor,
	}
	if p.Remote != nil {
		scfg.Remote = &credsource.RemoteConfig{
			URL:          p.Remote.URL,
			AuthTokenEnv: p.Remote.AuthTokenEnv,
			CacheTTL:     p.Remote.CacheTTL,
			Timeout:      p.Remote.Timeout,
		}
	}
	src, err := credsource.Resolve(name, scfg)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: provider %q (oauth_providers[%d], credential_source=%q): %w",
			p.Name, index, name, err)
	}
	return src, nil
}

// resolveTokenKEK reads the named env var and decodes its value as a
// 32-byte hex-encoded key-encryption key for AES-256-GCM token
// encryption at rest. Fail-loud per the §13 amendment: empty env or
// wrong-length decoded key crashes assembly with a wrapped error
// naming the env var.
func resolveTokenKEK(envName string) ([]byte, error) {
	if envName == "" {
		return nil, fmt.Errorf("tools/oauth: tools.oauth_token_kek_env must be set (validated upstream — this is a sanity check)")
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return nil, fmt.Errorf("tools/oauth: env var %q (named by tools.oauth_token_kek_env) is unset or empty — operator must populate a 32-byte hex-encoded KEK",
			envName)
	}
	kek, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: env var %q is not valid hex: %w", envName, err)
	}
	if len(kek) != KEKSizeBytes {
		return nil, fmt.Errorf("tools/oauth: env var %q decoded to %d bytes, want %d (AES-256-GCM)",
			envName, len(kek), KEKSizeBytes)
	}
	return kek, nil
}
