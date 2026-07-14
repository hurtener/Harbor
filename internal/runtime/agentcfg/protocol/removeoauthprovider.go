package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/adminwrite"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// removeoauthprovider.go — the admin-scoped `agent_config.remove_oauth_provider`
// control method: uninstall a Protocol-installed OAuth provider by name as a NEW
// revision AND uninstall it live (CLOSING it).
//
// # Deliberately breaking (fail loud, not silent degradation)
//
// Uninstall CLOSES the provider (auth.OAuthProvider.Close), so a still-bound
// connection's next southbound call fails LOUD rather than degrading to an
// unauthenticated dial (CLAUDE.md §13). This is correct: "refuse to uninstall
// while bound" would make revoke un-exercisable exactly when a broker credential
// has leaked. The break is confined to the OWNING owner by the owner-scoped
// run-start reconcile (a tenant-B run reconciles only its own owner's installed
// providers, so B can never close A's provider).
//
// # Revision + live uninstall, fail-closed audit (one unit)
//
// The write records a NEW revision dropping the named descriptor (carrying every
// sibling forward) and uninstalls the provider live. The admin-scope audit event
// and the mutation ship as ONE fail-closed unit ([adminwrite.Apply]): on
// audit-emit failure the mutation is COMPENSATED (the provider is re-installed
// from the dropped descriptor AND the active pointer rolled back) so an
// un-auditable uninstall is never left observably applied.
//
// # Two distinct loud errors
//
// An unknown name (not in the active revision's installed set) fails with
// ErrProviderNotFound; a boot-declared (yaml) name fails with
// ErrBootDeclaredProvider (edit yaml + restart). Neither records a revision.

// RemoveOAuthProvider uninstalls a Protocol-installed OAuth provider by name.
// See the file doc for the full contract.
func (s *Service) RemoveOAuthProvider(ctx context.Context, req prototypes.AgentConfigRemoveOAuthProviderRequest) (prototypes.AgentConfigRemoveOAuthProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, fmt.Errorf("%w: name is empty", ErrInvalidProvider)
	}
	if s.providerInstaller == nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, ErrProviderInstallUnavailable
	}
	q := identity.Quadruple{Identity: id}

	defer s.lockAgent(id.TenantID, req.AgentID)()

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, err
	}
	var prior []agentcfg.OAuthProviderDescriptor
	if hasActive {
		prior = active.Payload.OAuthProviderDescriptors()
	}
	dropped, found := findOAuthProvider(prior, name)
	if !found {
		// Distinguish a boot-declared provider (edit yaml + restart) from a plain
		// unknown name — both fail loud, neither records a revision.
		if _, boot := s.bootDeclaredOAuthProviders[name]; boot {
			return prototypes.AgentConfigRemoveOAuthProviderResponse{}, fmt.Errorf("%w: %q", ErrBootDeclaredProvider, name)
		}
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, fmt.Errorf("%w: %q", ErrProviderNotFound, name)
	}
	prevActiveRevID := active.RevisionID
	payload := s.rebuildWithoutOAuthProvider(active, hasActive, prior, name)

	var rev agentcfg.Revision
	applyErr, emitErr := adminwrite.Apply(
		func() (revert func() error, err error) {
			written, recErr := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload)
			if recErr != nil {
				return nil, recErr
			}
			rev = written
			if uErr := s.providerInstaller.UninstallProvider(ctx, name); uErr != nil {
				// A real live uninstall failure (a Close error) — roll the
				// just-written revision back so the call has NO observable effect.
				if _, rbErr := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent); rbErr != nil {
					return nil, fmt.Errorf("provider uninstall failed AND revision rollback failed (state may be inconsistent): %w", errors.Join(uErr, rbErr))
				}
				return nil, uErr
			}
			revert = func() error {
				var errs []error
				// Re-install a FRESH instance from the dropped descriptor (the old
				// instance was closed by the uninstall above).
				if e := s.providerInstaller.InstallProvider(ctx, id.TenantID, req.AgentID, dropped); e != nil {
					errs = append(errs, e)
				}
				if _, e := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent); e != nil {
					errs = append(errs, e)
				}
				return errors.Join(errs...)
			}
			return revert, nil
		},
		func() error {
			return s.emitOAuthProviderSet(ctx, q, agentcfg.EventTypeOAuthProviderRemoved, req.AgentID, name, "", rev.RevisionID)
		},
	)
	if applyErr != nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, applyErr
	}
	if emitErr != nil {
		return prototypes.AgentConfigRemoveOAuthProviderResponse{}, emitErr
	}
	return prototypes.AgentConfigRemoveOAuthProviderResponse{
		Revision:        revisionToWire(rev),
		Name:            name,
		Uninstalled:     true,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// rebuildWithoutOAuthProvider builds the replacing config payload: the named
// descriptor is dropped from the oauth-providers section, every other provider
// carried verbatim, and every sibling section carried forward.
func (s *Service) rebuildWithoutOAuthProvider(active agentcfg.Revision, hasActive bool, prior []agentcfg.OAuthProviderDescriptor, name string) agentcfg.ConfigPayload {
	var providers []agentcfg.OAuthProviderDescriptor
	for _, d := range prior {
		if d.Name == name {
			continue
		}
		providers = append(providers, d)
	}
	payload := carrySiblingsForward(active, hasActive)
	if len(providers) > 0 {
		payload.OAuthProviders = &agentcfg.OAuthProvidersSection{Providers: providers}
	}
	return payload
}

// findOAuthProvider returns the descriptor with the given name and whether it
// was present. The input slice is never mutated.
func findOAuthProvider(in []agentcfg.OAuthProviderDescriptor, name string) (agentcfg.OAuthProviderDescriptor, bool) {
	for _, d := range in {
		if d.Name == name {
			return d, true
		}
	}
	return agentcfg.OAuthProviderDescriptor{}, false
}
