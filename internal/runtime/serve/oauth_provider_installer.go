package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// oauth_provider_installer.go — the production concrete that installs /
// uninstalls a Protocol-installed, ZERO-URL OAuth provider onto the live
// owner-tagged provider set (`agent_config.set_oauth_provider` /
// `remove_oauth_provider`, and the run-start provider reconcile). It is the
// §4.4 boundary glue: it imports the concrete auth package (allowed in
// cmd/harbor + devstack) and satisfies the driver-agnostic
// agentcfgprotocol.ProviderInstaller + projection.OAuthProviderReconciler
// interfaces the agent-config service + run-loop depend on.
//
// Every credential-sink-determining value is read from the boot credential
// broker (resolved through the auth.ProviderBuilder from the descriptor's
// non-secret credential_broker NAME) — NONE from the wire descriptor — so no
// admin-writable field determines where a credential is sent.

// OAuthProviderInstaller installs / uninstalls Protocol-installed providers by
// building them from the boot credential broker (auth.ProviderBuilder) and
// installing them owner-tagged into the shared auth.ProviderSet.
//
// Concurrent reuse: builder + set + allowWireDescriptor are set once at
// construction; the builder + set are internally synchronised. The installer
// holds no mutable state.
type OAuthProviderInstaller struct {
	builder *toolauth.ProviderBuilder
	set     toolauth.ProviderSet
	// allowWireDescriptor is the effective DEV-ONLY, fail-closed opt-in (the
	// tools.allow_wire_oauth_descriptor config flag OR the boot env). It is a
	// KILL-SWITCH honoured on EVERY build — including the run-start reconcile —
	// so a wire-carried provider (TokenURL set) persisted during an opt-in-ON
	// window is NOT rebuilt live after the operator restarts with the opt-in
	// OFF. A name-only / boot broker-pull provider is unaffected.
	allowWireDescriptor bool
	// logger surfaces the kill-switch skip (a wire provider not rebuilt after a
	// flip-off) loud — never a silent drop.
	logger *slog.Logger
}

var _ agentcfgprotocol.ProviderPreparer = (*OAuthProviderInstaller)(nil)
var _ agentcfgprotocol.SignedCapabilityProviderPreparer = (*OAuthProviderInstaller)(nil)

// NewOAuthProviderInstaller builds the production installer. Both the builder
// and the set are mandatory; a nil either returns a nil installer so the caller
// leaves the install verbs unwired (→ 501 at the wire edge) rather than
// nil-panicking. allowWireDescriptor is the effective wire-descriptor opt-in
// (config flag OR boot env) — the kill-switch consulted on every build. A nil
// logger is replaced with the default so the kill-switch skip is never silent.
func NewOAuthProviderInstaller(builder *toolauth.ProviderBuilder, set toolauth.ProviderSet, allowWireDescriptor bool, logger *slog.Logger) *OAuthProviderInstaller {
	if builder == nil || set == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OAuthProviderInstaller{builder: builder, set: set, allowWireDescriptor: allowWireDescriptor, logger: logger}
}

// InstallProvider resolves the descriptor's credential_broker against the boot
// broker set, builds the broker-pull provider, and installs it owner-tagged.
// Auth-package errors are wrapped into the agent-config service's client-error
// sentinels so the wire handler classifies them as 400s (an unknown broker /
// name collision is a bad request, not a server fault).
func (i *OAuthProviderInstaller) InstallProvider(ctx context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) error {
	owner := toolauth.Owner{Tenant: tenant, Agent: agentID}
	if owner.Tenant == "" || owner.Agent == "" {
		return fmt.Errorf("%w: install requires a (tenant, agent) owner (tenant=%q agent=%q)", agentcfgprotocol.ErrInvalidProvider, tenant, agentID)
	}
	prov, err := i.build(ctx, desc)
	if err != nil {
		if errors.Is(err, ErrWireDescriptorDisabled) {
			// Kill-switch: a wire provider persisted during a prior opt-in-ON
			// window is presented for reconcile after a restart with the opt-in
			// OFF. Do NOT rebuild it live; log LOUD and SKIP (fail-closed — the
			// provider stays ABSENT, so a still-bound connection's next call fails
			// loud rather than silently POSTing the boot client credential to the
			// previously wire-set endpoint). Returning nil (not an error) keeps the
			// reconcile from bricking run start over one disabled provider; the
			// loud log makes the skip non-silent (CLAUDE.md §13). The write path
			// never reaches here — the agent-config handler gates it first.
			i.logger.WarnContext(ctx,
				"oauth provider: wire-carried provider NOT rebuilt — tools.allow_wire_oauth_descriptor is off (kill-switch); the provider stays absent and a bound connection's next call fails loud",
				"provider", desc.Name, "tenant", tenant, "agent", agentID)
			return nil
		}
		if errors.Is(err, toolauth.ErrUnknownBroker) || errors.Is(err, toolauth.ErrBrokerMissingCredentialURL) {
			return fmt.Errorf("%w: %w", agentcfgprotocol.ErrProviderBrokerUnknown, err)
		}
		return fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	if err := i.set.Install(owner, desc.Name, prov); err != nil {
		// A collision (boot / other-owner) — close the just-built, un-installed
		// instance so it does not leak, then surface as a client error.
		_ = prov.Close(ctx)
		return fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	return nil
}

// PrepareProvider builds a provider without publishing it to the shared set.
// The returned binding can be threaded directly into private MCP preparation;
// publication is a separate reversible step after desired state persists.
func (i *OAuthProviderInstaller) PrepareProvider(ctx context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) (agentcfgprotocol.PreparedOAuthProvider, error) {
	owner := toolauth.Owner{Tenant: tenant, Agent: agentID}
	if owner.Tenant == "" || owner.Agent == "" {
		return nil, fmt.Errorf("%w: prepare requires a (tenant, agent) owner (tenant=%q agent=%q)", agentcfgprotocol.ErrInvalidProvider, tenant, agentID)
	}
	prov, err := i.build(ctx, desc)
	if err != nil {
		if errors.Is(err, toolauth.ErrUnknownBroker) || errors.Is(err, toolauth.ErrBrokerMissingCredentialURL) {
			return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrProviderBrokerUnknown, err)
		}
		return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	return &preparedOAuthProvider{installer: i, owner: owner, name: desc.Name, provider: prov}, nil
}

// PrepareSignedCapabilityProvider builds the signed-capability provider privately. It is
// deliberately never staged in the general ProviderSet: the prepared MCP
// connection receives this exact instance as an override, and activation of
// that connection is the only dispatch linearization point.
func (i *OAuthProviderInstaller) PrepareSignedCapabilityProvider(ctx context.Context, broker string, binding toolauth.SignedCapabilityExchangeBinding, scopes []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	owner := toolauth.Owner{Tenant: binding.TenantID, Agent: binding.AgentID}
	if owner.IsZero() {
		return nil, fmt.Errorf("%w: signed capability requires a (tenant, agent) owner", agentcfgprotocol.ErrInvalidProvider)
	}
	prov, err := i.builder.BuildSignedCapability(ctx, broker, binding, scopes)
	if err != nil {
		if errors.Is(err, toolauth.ErrUnknownBroker) || errors.Is(err, toolauth.ErrBrokerMissingCredentialURL) {
			return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrProviderBrokerUnknown, err)
		}
		return nil, fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	return &preparedOAuthProvider{installer: i, owner: owner, name: binding.ProviderName, provider: prov}, nil
}

type preparedOAuthProvider struct {
	mu        sync.Mutex
	installer *OAuthProviderInstaller
	owner     toolauth.Owner
	name      string
	provider  toolauth.OAuthProvider
	swap      toolauth.ProviderSwap
	committed bool
	closed    bool
}

func (p *preparedOAuthProvider) Binding() toolauth.OAuthProvider { return p.provider }

func (p *preparedOAuthProvider) Publish(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("oauth provider %q: prepared binding is closed", p.name)
	}
	if p.swap != nil {
		return nil
	}
	swap, err := p.installer.set.Stage(p.owner, p.name, p.provider)
	if err != nil {
		return fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	p.swap = swap
	return nil
}

func (p *preparedOAuthProvider) Commit(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed || p.swap == nil {
		return
	}
	p.committed = true
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := p.swap.Commit(cleanupCtx); err != nil {
		p.installer.logger.WarnContext(cleanupCtx, "oauth provider published but displaced provider cleanup failed", "provider", p.name, "error", err.Error())
	}
}

func (p *preparedOAuthProvider) Rollback(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed {
		return fmt.Errorf("oauth provider %q: committed publication cannot be rolled back", p.name)
	}
	if p.closed {
		return nil
	}
	if p.swap != nil {
		if err := p.swap.Rollback(); err != nil {
			return err
		}
	}
	p.closed = true
	return p.provider.Close(ctx)
}

func (p *preparedOAuthProvider) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.swap != nil {
		return fmt.Errorf("oauth provider %q: published binding requires Rollback or Commit", p.name)
	}
	p.closed = true
	return p.provider.Close(ctx)
}

// ErrWireDescriptorDisabled — a wire-carried provider (TokenURL set) was
// presented for build while the fail-closed wire-descriptor opt-in is OFF. The
// install path never reaches this (the agent-config handler gates the write), so
// it fires only on the run-start RECONCILE of a wire provider persisted during a
// prior opt-in-ON window: the kill-switch refuses to rebuild it live after a
// restart with the opt-in off.
var ErrWireDescriptorDisabled = errors.New("serve: wire-carried oauth provider not rebuilt — tools.allow_wire_oauth_descriptor is off (the dev opt-in is a kill-switch: a wire provider installed while it was on is not rebuilt after a restart with it off)")

// build constructs the provider instance for a descriptor: the DEV-GATED wire
// binding (TokenURL set — the NEW server's wire token_url + wire audience + the
// runtime-DERIVED downstream sink, with the runtime's OWN credential custody read
// from the named boot broker) or the default name-only broker-pull shape. The
// wire-descriptor opt-in is a KILL-SWITCH honoured HERE on every build: a wire
// descriptor with the opt-in off is refused loud (the install path is already
// gated at the handler; this closes the reconcile-after-flip-off path).
func (i *OAuthProviderInstaller) build(ctx context.Context, desc agentcfg.OAuthProviderDescriptor) (toolauth.OAuthProvider, error) {
	if desc.TokenURL != "" {
		if !i.allowWireDescriptor {
			return nil, fmt.Errorf("%w (provider=%q)", ErrWireDescriptorDisabled, desc.Name)
		}
		return i.builder.BuildWire(ctx, toolauth.WireProviderDescriptor{
			Name:                   desc.Name,
			CredentialBroker:       desc.CredentialBroker,
			TokenURL:               desc.TokenURL,
			Audience:               desc.Audience,
			Scopes:                 append([]string(nil), desc.Scopes...),
			AllowedDownstreamHosts: append([]string(nil), desc.AllowedDownstreamHosts...),
		})
	}
	return i.builder.Build(ctx, desc.Name, desc.CredentialBroker, desc.Scopes)
}

// UninstallProvider removes the named provider from the owner-tagged set and
// CLOSES it (a still-bound connection's next call then fails loud). The
// (tenant, agentID) pair is the caller's resolved owner; the set refuses a
// cross-owner drop at its own boundary (ErrProviderOwnerCollision) — defense in
// depth independent of the handler's caller-side owner resolution.
func (i *OAuthProviderInstaller) UninstallProvider(ctx context.Context, tenant, agentID, name string) error {
	return i.set.Uninstall(ctx, toolauth.Owner{Tenant: tenant, Agent: agentID}, name)
}

// InstalledFor returns the owner's installed provider names — the owner-scoped
// reconcile view.
func (i *OAuthProviderInstaller) InstalledFor(_ context.Context, owner toolauth.Owner) []string {
	return i.set.InstalledFor(owner)
}
