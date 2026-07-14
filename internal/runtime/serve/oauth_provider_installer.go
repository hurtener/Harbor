package serve

import (
	"context"
	"errors"
	"fmt"

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
// Concurrent reuse: builder + set are set once at construction; both are
// internally synchronised. The installer holds no mutable state.
type OAuthProviderInstaller struct {
	builder *toolauth.ProviderBuilder
	set     toolauth.ProviderSet
}

// NewOAuthProviderInstaller builds the production installer. Both the builder
// and the set are mandatory; a nil either returns a nil installer so the caller
// leaves the install verbs unwired (→ 501 at the wire edge) rather than
// nil-panicking.
func NewOAuthProviderInstaller(builder *toolauth.ProviderBuilder, set toolauth.ProviderSet) *OAuthProviderInstaller {
	if builder == nil || set == nil {
		return nil
	}
	return &OAuthProviderInstaller{builder: builder, set: set}
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
	prov, err := i.builder.Build(ctx, desc.Name, desc.CredentialBroker, desc.Scopes)
	if err != nil {
		if errors.Is(err, toolauth.ErrUnknownBroker) || errors.Is(err, toolauth.ErrBrokerMissingCredentialURL) {
			return fmt.Errorf("%w: %w", agentcfgprotocol.ErrProviderBrokerUnknown, err)
		}
		return fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	if err := i.set.Install(owner, desc.Name, prov); err != nil {
		// A collision (boot / other-owner) — close the just-built, un-installed
		// instance so it does not leak, then surface as a client error.
		_ = prov.Close(ctx) //nolint:errcheck // best-effort cleanup of a rejected build; the install error is the loud signal.
		return fmt.Errorf("%w: %w", agentcfgprotocol.ErrInvalidProvider, err)
	}
	return nil
}

// UninstallProvider removes the named provider from the owner-tagged set and
// CLOSES it (a still-bound connection's next call then fails loud).
func (i *OAuthProviderInstaller) UninstallProvider(ctx context.Context, name string) error {
	return i.set.Uninstall(ctx, name)
}

// InstalledFor returns the owner's installed provider names — the owner-scoped
// reconcile view.
func (i *OAuthProviderInstaller) InstalledFor(_ context.Context, owner toolauth.Owner) []string {
	return i.set.InstalledFor(owner)
}
