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
	"net/url"
	"os"
	"sort"
	"strings"
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
	// The ONE shared KEK-backed sealer construction (NewSealerFromEnv):
	// the OAuth token store, the pending-flow store, signed-capability
	// admissions, HA-61 proposal tokens, and HA-56 render admissions all
	// derive from the same restart-stable AES-256-GCM authority. No
	// second sealer is ever constructed over the same key.
	sealer, err := NewSealerFromEnv(cfg.OAuthTokenKEKEnv)
	if err != nil {
		return nil, err
	}
	tokenStore, err := NewTokenStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: token store: %w", err)
	}
	flowStore, err := NewFlowStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: pending flow store: %w", err)
	}
	factoryDeps := FactoryDeps{
		Store:       tokenStore,
		Flows:       flowStore,
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
	sealer      Sealer
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
	// The shared crypto chain is built ONCE through the SAME generalized
	// construction (NewSealerFromEnv) the OAuth providers use, so an
	// installed provider shares the same token store AND the same
	// restart-stable sealer — never a second instance over the same key.
	sealer, err := NewSealerFromEnv(cfg.OAuthTokenKEKEnv)
	if err != nil {
		return nil, err
	}
	tokenStore, err := NewTokenStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: broker provider builder: token store: %w", err)
	}
	flowStore, err := NewFlowStore(deps.State, sealer)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: broker provider builder: pending flow store: %w", err)
	}
	pb.factoryDeps = FactoryDeps{
		Store:       tokenStore,
		Flows:       flowStore,
		Bus:         deps.Bus,
		Redactor:    deps.Redactor,
		Coordinator: deps.Coordinator,
	}
	pb.sealer = sealer
	return pb, nil
}

// AdmissionSealer returns the broker KEK-backed opaque sealing capability used
// to authenticate durable signed-capability run admissions. It is nil when no
// credential broker is boot-declared.
func (b *ProviderBuilder) AdmissionSealer() Sealer {
	if b == nil {
		return nil
	}
	return b.sealer
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
	return b.buildBrokerPull(ctx, name, brokerName, scopes, brokerPullOverride{})
}

// WireProviderDescriptor is the non-secret input to [ProviderBuilder.BuildWire]:
// the NEW server's OAuth params carried over the wire behind the dev-only
// `allow_wire_oauth_descriptor` opt-in, PLUS the boot-declared credential broker
// NAME. The runtime's OWN credential custody (the coordinator pull endpoint, the
// service-token env-var name, the org client credential) is read from the boot
// broker — NONE of it rides the wire. AllowedDownstreamHosts is DERIVED by the
// caller from the bound connection's own URL (never a wire field); the wire
// TokenURL is dialed through the token-exchange SSRF backstop.
type WireProviderDescriptor struct {
	// Name is the installed provider name.
	Name string
	// CredentialBroker names the boot-declared broker that supplies the runtime's
	// own credential custody. Required.
	CredentialBroker string
	// TokenURL is the NEW server's RFC-8693 token-exchange endpoint (wire-carried).
	TokenURL string
	// Audience is the NEW server's exchanged-token audience (wire-carried; optional).
	Audience string
	// Scopes is the requested scope subset (wire-carried; optional).
	Scopes []string
	// AllowedDownstreamHosts is the DERIVED downstream sink allow-list (from the
	// bound connection's URL). May be empty (a set_oauth_provider install with no
	// bound connection yet — the provider is unbindable until a connection derives
	// its sink); a bound connection ALWAYS supplies exactly one host.
	AllowedDownstreamHosts []string
}

// ErrWireDescriptorIncomplete — a wire provider descriptor is missing a required
// field (token_url or credential_broker). Loud, fail-closed.
var ErrWireDescriptorIncomplete = errors.New("auth: wire oauth provider descriptor is incomplete (token_url and credential_broker are required for the dev-gated wire binding)")

// BuildWire constructs a broker-pull `tokenexchange` provider for a wire-carried
// binding (the dev-only `allow_wire_oauth_descriptor` path). It reads the
// runtime's OWN credential custody (the coordinator pull endpoint, the
// service-token env name, the org client credential) from the boot-declared
// broker NAMED by the descriptor — EXACTLY as [Build] does — and overrides ONLY
// the NEW server's per-server OAuth params (token endpoint, audience,
// downstream sink) from the wire:
//
//   - The wire TokenURL replaces the broker's exchange endpoint (the NEW server's
//     token endpoint), dialed through the driver's own token-exchange SSRF
//     backstop (private / link-local / ULA / unspecified refused post-DNS, every
//     redirect refused, no proxy — subject to the independent dev-only
//     private-dial opt-in).
//   - The exchanged token's downstream sink is the caller-DERIVED
//     AllowedDownstreamHosts (from the bound connection's own URL, never a wire
//     field), so a token can only ever be injected into the endpoint the
//     connection actually dials.
//   - The wire Audience (optional) sets the NEW server's audience; the broker's
//     boot scope ceiling still clamps the requested scopes.
//
// No credential-source URL, env-var name, or secret rides the wire — the
// runtime's credential custody is 100% boot-declared on the named broker.
func (b *ProviderBuilder) BuildWire(ctx context.Context, desc WireProviderDescriptor) (OAuthProvider, error) {
	if desc.TokenURL == "" || desc.CredentialBroker == "" {
		return nil, fmt.Errorf("%w (name=%q)", ErrWireDescriptorIncomplete, desc.Name)
	}
	return b.buildBrokerPull(ctx, desc.Name, desc.CredentialBroker, desc.Scopes, brokerPullOverride{
		override:     true,
		tokenURL:     desc.TokenURL,
		audience:     desc.Audience,
		allowedHosts: desc.AllowedDownstreamHosts,
	})
}

// BuildSignedCapability constructs the signed-capability production exception. The token
// endpoint and credential source remain pinned by the named boot broker; only
// the audience and downstream sink already authenticated by the signed
// authority vary. Requested scopes must already be a subset of the boot scope
// ceiling; this path refuses widening instead of relying on the general
// token-exchange driver's silent intersection. It deliberately does not consult
// the development-only wire descriptor gate.
func (b *ProviderBuilder) BuildSignedCapability(ctx context.Context, brokerName string, binding SignedCapabilityExchangeBinding, scopes []string) (OAuthProvider, error) {
	u, err := url.Parse(binding.Resource)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("auth: signed capability sink is not a canonical https origin")
	}
	if binding.ProviderName == "" || binding.Audience == "" || binding.Resource == "" {
		return nil, fmt.Errorf("auth: signed capability exchange binding is incomplete")
	}
	broker, ok := b.brokers[brokerName]
	if !ok {
		return nil, fmt.Errorf("%w: %q (declared: %v)", ErrUnknownBroker, brokerName, b.BrokerNames())
	}
	if err := requireSignedCapabilityScopeSubset(scopes, broker.ScopeCeiling); err != nil {
		return nil, fmt.Errorf("%w: broker %q: %w", ErrConfigInvalid, brokerName, err)
	}
	return b.buildBrokerPull(ctx, binding.ProviderName, brokerName, scopes, brokerPullOverride{
		override: true, audience: binding.Audience, resourceIndicator: binding.Resource, allowedHosts: []string{u.Host},
		signedCapability: &binding, refuseRedirects: true,
	})
}

func requireSignedCapabilityScopeSubset(requested, ceiling []string) error {
	allowed := make(map[string]struct{}, len(ceiling))
	for _, scope := range ceiling {
		if normalized := strings.TrimSpace(scope); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	for _, scope := range requested {
		normalized := strings.TrimSpace(scope)
		if normalized == "" {
			return fmt.Errorf("signed capability requested an empty scope")
		}
		if _, ok := allowed[normalized]; !ok {
			return fmt.Errorf("signed capability requested scope %q outside the boot ceiling", normalized)
		}
	}
	return nil
}

// brokerPullOverride carries the wire-supplied per-server OAuth params that
// override the boot broker's defaults for a wire-installed provider. The zero
// value (override=false) is the plain boot broker-pull path.
type brokerPullOverride struct {
	override          bool
	tokenURL          string
	audience          string
	resourceIndicator string
	allowedHosts      []string
	signedCapability  *SignedCapabilityExchangeBinding
	refuseRedirects   bool
}

// buildBrokerPull is the shared broker-pull construction for both [Build] (no
// override — every sink read from the boot broker) and [BuildWire] (override —
// the NEW server's token endpoint / audience / DERIVED downstream sink come from
// the wire, the credential custody + scope ceiling stay boot-declared). The
// credential SOURCE (the coordinator pull endpoint + the service-token env name +
// the org client credential) is ALWAYS the boot broker's — never wire.
func (b *ProviderBuilder) buildBrokerPull(ctx context.Context, name, brokerName string, scopes []string, ov brokerPullOverride) (OAuthProvider, error) {
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
	// Sink params: boot broker by default; the wire overrides ONLY the NEW
	// server's token endpoint, audience, and DERIVED downstream sink. The scope
	// ceiling always stays the broker's boot bound (a wire descriptor can never
	// widen scope past it).
	tokenURL := broker.TokenURL
	audience := broker.Audience
	allowedHosts := append([]string(nil), broker.AllowedDownstreamHosts...)
	if ov.override {
		// The signed-capability path intentionally leaves tokenURL empty:
		// the exchange sink remains the boot broker's fixed endpoint. The
		// development-only wire path supplies one and retains its old override.
		if ov.tokenURL != "" {
			tokenURL = ov.tokenURL
		}
		allowedHosts = append([]string(nil), ov.allowedHosts...)
		if ov.audience != "" {
			audience = ov.audience
		}
	}
	pcfg := ProviderConfig{
		Name:                      name,
		CredentialSource:          src,
		Scopes:                    append([]string(nil), scopes...),
		TokenURL:                  tokenURL,
		AllowedDownstreamHosts:    allowedHosts,
		Audience:                  audience,
		ScopeCeiling:              append([]string(nil), broker.ScopeCeiling...),
		ResourceIndicator:         ov.resourceIndicator,
		SignedCapability:          ov.signedCapability,
		RefuseDownstreamRedirects: ov.refuseRedirects,
	}
	prov, err := Resolve(ctx, TokenExchangeDriverName, pcfg, b.factoryDeps)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: install provider %q (broker=%q, driver=%q): %w", name, brokerName, TokenExchangeDriverName, err)
	}
	return prov, nil
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

// NewSealerFromEnv is the GENERALIZED restart-stable KEK-backed sealer
// construction: it reads the 32-byte hex KEK named by envName and
// builds the AES-256-GCM Sealer. This is the ONE construction shared by
// every runtime authority that needs a restart-stable sealing key — the
// OAuth token store, signed-capability admissions, HA-61 skill-import
// proposal tokens, and HA-56 render admissions. The returned Sealer is
// immutable and safe for concurrent reuse by N goroutines; a caller
// MUST NOT construct a second sealer over the same key.
//
// Fail-loud: an empty env name, an unset/empty env value, non-hex
// content, or a wrong-length key all return a wrapped error naming the
// env var. The production composition calls this at boot whenever a
// surface that requires the shared authority is enabled, so a missing
// KEK fails readiness loud even when no OAuth provider or credential
// broker is declared.
func NewSealerFromEnv(envName string) (Sealer, error) {
	kek, err := resolveTokenKEK(envName)
	if err != nil {
		return nil, err
	}
	sealer, err := NewAESGCMSealer(kek)
	if err != nil {
		return nil, fmt.Errorf("tools/oauth: shared KEK sealer: %w", err)
	}
	return sealer, nil
}
