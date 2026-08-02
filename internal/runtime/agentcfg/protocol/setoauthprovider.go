package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/adminwrite"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// setoauthprovider.go — the admin-scoped `agent_config.set_oauth_provider`
// control method: install (upsert) a ZERO-URL, broker-pull OAuth provider onto
// the agent-config revision spine AND into the live owner-tagged provider set.
//
// # The zero-URL invariant (load-bearing)
//
// The writable descriptor carries `{name, driver:"tokenexchange",
// credential_source:"remote", credential_broker, scopes?}` — NO URL of any kind,
// NO env-var name, NO secret. Every credential-sink-determining value (the token
// endpoint, the credential-pull endpoint, the allowed downstream hosts, the
// audience, and the scope ceiling) is pinned at boot on the NAMED credential
// broker the descriptor references by non-secret name. Because the
// forbidden fields (`token_url`, `auth_url`, `client_id_env`,
// `client_secret_env`, `remote`) are simply NOT on the wire struct, the wire
// handler's `DisallowUnknownFields` decode rejects any of them BY NAME before
// this method is ever reached. This method re-validates the positive shape
// (driver / credential_source / broker) and rejects an empty credential_source
// loud (in config "" means the `env` source, which this shape forbids).
//
// # Revision + live install, fail-closed audit (one unit)
//
// The write records a NEW revision whose oauth-providers section carries the
// installed descriptor, every sibling section carried forward (the
// rebuild-completeness guard). It THEN installs the provider live into the
// owner-tagged provider set (resolving the credential_broker → building the
// tokenexchange provider). The admin-scope audit event and the mutation ship as
// ONE fail-closed unit ([adminwrite.Apply]): on audit-emit failure the mutation
// is COMPENSATED (the just-installed provider is uninstalled AND the active
// pointer rolled back) so an un-auditable write is never left observably applied.
//
// # Boot wins (a distinct loud error)
//
// A name colliding with a boot-declared `tools.oauth_providers[]` entry is
// refused with ErrBootDeclaredProvider (edit yaml + restart) — the verb governs
// revisioned installed-provider state only.

// ProviderInstaller is the driver-agnostic seam the provider install / uninstall
// verbs (and the run-start provider reconcile) use to install / uninstall a
// Protocol-installed, zero-URL OAuth provider on the LIVE owner-tagged provider
// set. The concrete (wired at the cmd/harbor + devstack boundary) resolves the
// descriptor's credential_broker against the boot broker set, builds the
// tokenexchange provider, and installs it owner-tagged; keeping it an injected
// interface preserves this package's §4.4 boundary (no concrete auth-provider
// construction here).
type ProviderInstaller interface {
	// InstallProvider validates the descriptor's credential_broker against the
	// boot broker set, builds the broker-pull provider, and installs it into the
	// owner-tagged provider set under (tenant, agentID). An unknown broker, a
	// boot-name collision, or another owner's install collision fails loud. A
	// re-install by the same owner replaces (and closes) the prior instance.
	InstallProvider(ctx context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) error
	// UninstallProvider removes the named provider from the owner-tagged set and
	// CLOSES it. The (tenant, agentID) pair is the caller's resolved owner; the
	// set refuses a cross-owner drop at its own boundary (defense in depth). A
	// missing name is an idempotent no-op.
	UninstallProvider(ctx context.Context, tenant, agentID, name string) error
}

// Provider install/uninstall sentinel errors. The wire handler maps each onto a
// canonical Protocol Code + HTTP status.
var (
	// ErrInvalidProvider — the provider descriptor failed validation (empty
	// name, wrong driver, empty/wrong credential_source, empty credential_broker)
	// (→ 400).
	ErrInvalidProvider = errors.New("agentcfg/protocol: invalid oauth provider descriptor")
	// ErrBootDeclaredProvider — set/remove_oauth_provider named a boot-declared
	// (yaml) provider — a DISTINCT loud error (boot wins; edit yaml + restart)
	// (→ 400).
	ErrBootDeclaredProvider = errors.New("agentcfg/protocol: oauth provider is boot-declared (edit yaml + restart, not the control plane)")
	// ErrProviderNotFound — remove_oauth_provider named a provider absent from
	// the agent's revisioned installed-provider set (→ 404).
	ErrProviderNotFound = errors.New("agentcfg/protocol: oauth provider not found in the agent's installed set")
	// ErrProviderInstallUnavailable — set/remove_oauth_provider was called but
	// no ProviderInstaller is wired on this runtime (→ 501).
	ErrProviderInstallUnavailable = errors.New("agentcfg/protocol: oauth provider install not wired on this runtime")
	// ErrProviderBrokerUnknown — the descriptor's credential_broker resolves to
	// no boot-declared broker (→ 400). The installer wraps its own broker error;
	// this sentinel lets the wire handler classify it as a client error.
	ErrProviderBrokerUnknown = errors.New("agentcfg/protocol: credential_broker resolves to no boot-declared broker")
	// ErrWireDescriptorNotAllowed — a provider descriptor (set_oauth_provider or
	// an add_mcp_connection inline binding) carried a credential-sink field
	// (token_url / audience / remote) while the fail-closed
	// tools.allow_wire_oauth_descriptor opt-in was OFF (→ 400). The default
	// zero-URL name-only posture is unchanged; the reject names the offending
	// field + the opt-in key.
	ErrWireDescriptorNotAllowed = errors.New("agentcfg/protocol: wire-carried oauth-provider descriptor is not allowed (the fail-closed tools.allow_wire_oauth_descriptor opt-in is off)")
)

// providerWritableDriver / providerWritableSource are the ONLY driver /
// credential-source values the writable descriptor admits — the non-interactive
// broker-pull exchange. Anything else is rejected loud.
const (
	providerWritableDriver = "tokenexchange"
	providerWritableSource = "remote"
)

// SetOAuthProvider installs (upserts) a ZERO-URL, broker-pull OAuth provider.
// See the file doc for the full contract (the zero-URL invariant, revision +
// live install + fail-closed audit, the boot-declared loud error).
func (s *Service) SetOAuthProvider(ctx context.Context, req prototypes.AgentConfigSetOAuthProviderRequest) (prototypes.AgentConfigSetOAuthProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, err
	}
	desc, err := s.gateAndValidateOAuthProviderDescriptor(req.Provider)
	if err != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, err
	}
	// Boot wins: a name colliding with a boot-declared provider is refused with
	// a DISTINCT loud error (edit yaml + restart). Checked BEFORE any state
	// change so nothing is persisted on a collision.
	if _, boot := s.bootDeclaredOAuthProviders[desc.Name]; boot {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, fmt.Errorf("%w: %q", ErrBootDeclaredProvider, desc.Name)
	}
	if s.providerInstaller == nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, ErrProviderInstallUnavailable
	}
	q := identity.Quadruple{Identity: id}

	defer s.lockAgent(id.TenantID, req.AgentID)()

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, err
	}
	var prior []agentcfg.OAuthProviderDescriptor
	if hasActive {
		prior = active.Payload.OAuthProviderDescriptors()
	}
	var prevActiveRevID string
	if hasActive {
		prevActiveRevID = active.RevisionID
	}
	payload := s.rebuildWithOAuthProvider(active, hasActive, prior, desc)

	var rev agentcfg.Revision
	applyErr, emitErr := adminwrite.Apply(
		func() (revert func() error, err error) {
			written, recErr := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
				agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
			if recErr != nil {
				return nil, recErr
			}
			rev = written
			if instErr := s.providerInstaller.InstallProvider(ctx, id.TenantID, req.AgentID, desc); instErr != nil {
				// A real live install failure (unknown broker, build failure, an
				// owner collision) — roll the just-written revision back so the call
				// has NO observable effect, then return the install error loud.
				if hasActive {
					if _, rbErr := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent, compensatingWrite()); rbErr != nil {
						return nil, fmt.Errorf("provider install failed AND revision rollback failed (state may be inconsistent): %w", errors.Join(instErr, rbErr))
					}
				}
				return nil, instErr
			}
			revert = func() error {
				var errs []error
				if e := s.providerInstaller.UninstallProvider(ctx, id.TenantID, req.AgentID, desc.Name); e != nil {
					errs = append(errs, e)
				}
				if hasActive {
					if _, e := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent, compensatingWrite()); e != nil {
						errs = append(errs, e)
					}
				} else {
					// No prior revision exists. A forward empty revision is NOT a
					// safe compensation: another Runtime can observe and authorise the
					// candidate between the write and that new revision. Deactivate only
					// if the physical pointer still names OUR exact candidate.
					deactivated, e := s.registry.DeactivateIfActive(ctx, q, req.AgentID, rev.RevisionID, agentcfg.ConfigScopeAgent)
					if e != nil {
						errs = append(errs, e)
					} else if !deactivated {
						errs = append(errs, fmt.Errorf("first-install compensation did not own active revision %q", rev.RevisionID))
					}
				}
				return errors.Join(errs...)
			}
			return revert, nil
		},
		func() error {
			return s.emitOAuthProviderSet(ctx, q, agentcfg.EventTypeOAuthProviderInstalled, req.AgentID, desc.Name, desc.CredentialBroker, rev.RevisionID)
		},
	)
	if applyErr != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, applyErr
	}
	if emitErr != nil {
		return prototypes.AgentConfigSetOAuthProviderResponse{}, emitErr
	}
	return prototypes.AgentConfigSetOAuthProviderResponse{
		Revision:        revisionToWire(rev),
		Name:            desc.Name,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// validateOAuthProviderDescriptor validates + normalises the wire descriptor,
// failing loud with ErrInvalidProvider. It enforces the POSITIVE shape (the
// sink/secret fields cannot exist on the struct — the wire handler's
// DisallowUnknownFields decode rejects them by name): a non-empty name; the
// driver EXACTLY "tokenexchange"; the credential_source EXACTLY "remote" (an
// empty value is a loud reject — "" means the env source, forbidden here); and a
// non-empty credential_broker. Scopes are carried verbatim (clamped to the
// broker's boot ceiling at build time).
func validateOAuthProviderDescriptor(p prototypes.AgentConfigOAuthProviderDescriptor) (agentcfg.OAuthProviderDescriptor, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: name is empty", ErrInvalidProvider)
	}
	driver := strings.TrimSpace(p.Driver)
	if driver != providerWritableDriver {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: driver %q not installable — only %q (the non-interactive broker-pull exchange) is Protocol-installable", ErrInvalidProvider, p.Driver, providerWritableDriver)
	}
	source := strings.TrimSpace(p.CredentialSource)
	if source == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_source is empty — an installed provider must declare credential_source %q (an empty value means the env source, which is config/file-only)", ErrInvalidProvider, providerWritableSource)
	}
	if source != providerWritableSource {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_source %q not installable — only %q (broker-pull) is Protocol-installable", ErrInvalidProvider, p.CredentialSource, providerWritableSource)
	}
	broker := strings.TrimSpace(p.CredentialBroker)
	if broker == "" {
		return agentcfg.OAuthProviderDescriptor{}, fmt.Errorf("%w: credential_broker is empty — an installed provider references a boot-declared broker by name (every credential sink is boot-pinned)", ErrInvalidProvider)
	}
	scopes := make([]string, 0, len(p.Scopes))
	for _, sc := range p.Scopes {
		if s := strings.TrimSpace(sc); s != "" {
			scopes = append(scopes, s)
		}
	}
	if len(scopes) == 0 {
		scopes = nil
	}
	return agentcfg.OAuthProviderDescriptor{
		Name:             name,
		Driver:           driver,
		CredentialSource: source,
		CredentialBroker: broker,
		Scopes:           scopes,
	}, nil
}

// rebuildWithOAuthProvider builds the replacing config payload: the installed
// descriptor is added / replaced by name in the oauth-providers section, every
// other provider carried verbatim, and every sibling section carried forward
// (the rebuild-completeness guard).
func (s *Service) rebuildWithOAuthProvider(active agentcfg.Revision, hasActive bool, prior []agentcfg.OAuthProviderDescriptor, desc agentcfg.OAuthProviderDescriptor) agentcfg.ConfigPayload {
	providers := make([]agentcfg.OAuthProviderDescriptor, 0, len(prior)+1)
	replaced := false
	for _, d := range prior {
		if d.Name == desc.Name {
			providers = append(providers, desc)
			replaced = true
			continue
		}
		providers = append(providers, d)
	}
	if !replaced {
		providers = append(providers, desc)
	}
	payload := carrySiblingsForward(active, hasActive)
	payload.OAuthProviders = &agentcfg.OAuthProvidersSection{Providers: providers}
	return payload
}

// emitOAuthProviderSet publishes the install / uninstall admin-scope audit event
// and RETURNS a publish failure so the caller fails the write closed. A nil bus
// is a no-op success. NO URL / env-var name / secret is ever carried.
func (s *Service) emitOAuthProviderSet(ctx context.Context, q identity.Quadruple, evType events.EventType, agentID, providerName, broker, revisionID string) error {
	if s.bus == nil {
		return nil
	}
	now := s.now().UTC()
	ev := events.Event{
		Type:       evType,
		Identity:   q,
		OccurredAt: now,
		Payload: agentcfg.OAuthProviderSetPayload{
			Author:           q,
			AgentID:          agentID,
			ProviderName:     providerName,
			CredentialBroker: broker,
			RevisionID:       revisionID,
			OccurredAt:       now,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("agentcfg/protocol: publish %s: %w", ev.Type, err)
	}
	return nil
}

// carrySiblingsForward returns a ConfigPayload carrying every sibling section of
// the active revision forward (skills / tool-exposure / prompt-layers /
// connections / llm-params / hooks / naming) — the rebuild-completeness guard
// shared by the provider install/uninstall rebuilds. The oauth-providers section
// is set by the caller.
func carrySiblingsForward(active agentcfg.Revision, hasActive bool) agentcfg.ConfigPayload {
	payload := agentcfg.ConfigPayload{}
	if hasActive {
		payload.Skills = active.Payload.Skills
		payload.ToolExposure = active.Payload.ToolExposure
		payload.PromptLayers = active.Payload.PromptLayers
		payload.Connections = active.Payload.Connections
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
		// The ordered additive prompt blocks are a sibling section like any
		// other: this verb replaces only its own, so the blocks survive.
		payload.ExtraSystemBlocks = active.Payload.ExtraSystemBlocks
	}
	return payload
}
