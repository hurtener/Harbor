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

// setllmprovider.go — the admin-scoped `agent_config.set_llm_provider` control
// method: install (upsert) / rotate a ZERO-URL, broker-pull INFERENCE provider
// binding into the live owner-tagged provider set. A SEPARATE method from
// `set_oauth_provider` (a distinct credential plane — an LLM provider KEY, not
// an OAuth client credential); NOT a relaxation of its tokenexchange-only
// allowlist.
//
// # The zero-URL invariant (load-bearing)
//
// The writable descriptor carries only `{name, provider,
// credential_source:"remote", inference_broker, model_allow?}` — NO URL of any
// kind, NO env-var name, NO secret. Every credential-sink-determining value (the
// pull endpoint, audience, scope ceiling) is pinned at boot on the NAMED
// inference broker the descriptor references by non-secret name. Because the
// forbidden fields are simply NOT on the wire struct, the wire handler's
// `DisallowUnknownFields` decode rejects any of them BY NAME before this method
// is reached. This method re-validates the positive shape and rejects an empty
// credential_source loud.
//
// # Provider-SET model
//
// Installed bindings follow the provider-SET model: bare-name resolution,
// owner-tagged install, and an uninstall that CLOSES the binding — so a
// subsequently-bound call fails LOUD rather than serving the old key. The
// live install + the admin-scope audit event ship as ONE fail-closed unit
// ([adminwrite.Apply]): on audit-emit failure the just-installed binding is
// uninstalled so an un-auditable write is never left observably applied.

// LLMProviderInstaller is the driver-agnostic seam the set_llm_provider verb
// (and the run-start provider reconcile) uses to install / uninstall a
// zero-URL, broker-pull inference provider on the LIVE owner-tagged set. The
// concrete (wired at the cmd/harbor + devstack boundary) resolves the
// descriptor's inference_broker against the boot broker set, builds the
// InferenceKeySource over the runtime's shared LiveKey, connects it (fail-loud),
// and installs it owner-tagged; keeping it an injected interface preserves this
// package's §4.4 boundary (no concrete LLM-credential construction here).
type LLMProviderInstaller interface {
	// InstallLLMProvider validates the descriptor's inference_broker against
	// the boot broker set, builds + connects the broker-pull source, and
	// installs the binding owner-tagged under (tenant, agentID). An unknown
	// broker or a connect failure fails loud. A re-install by the same owner
	// replaces (and closes) the prior binding (the rotate path).
	InstallLLMProvider(ctx context.Context, tenant, agentID string, desc agentcfg.LLMProviderDescriptor) error
	// UninstallLLMProvider removes the named binding and CLOSES it — the live
	// key is zeroed so a subsequently-bound call fails loud rather than serving
	// the old key. A missing name is an idempotent no-op.
	UninstallLLMProvider(ctx context.Context, name string) error
}

// Inference-provider install sentinel errors. The wire handler maps each onto a
// canonical Protocol Code + HTTP status.
var (
	// ErrInvalidLLMProvider — the descriptor failed validation (empty name /
	// provider, empty/non-remote credential_source, empty inference_broker)
	// (→ 400).
	ErrInvalidLLMProvider = errors.New("agentcfg/protocol: invalid llm provider descriptor")
	// ErrLLMProviderBrokerUnknown — the descriptor's inference_broker resolves
	// to no boot-declared broker (→ 400). No admin-writable field determines a
	// sink, so an unknown broker is a loud client error, never a silent accept.
	ErrLLMProviderBrokerUnknown = errors.New("agentcfg/protocol: inference_broker resolves to no boot-declared broker")
	// ErrLLMProviderInstallUnavailable — set_llm_provider was called but no
	// LLMProviderInstaller is wired on this runtime (→ 501).
	ErrLLMProviderInstallUnavailable = errors.New("agentcfg/protocol: llm provider install not wired on this runtime")
)

// llmProviderWritableSource is the ONLY credential_source the writable
// inference descriptor admits — the broker-pull source. Anything else
// (including the empty env source) is rejected loud.
const llmProviderWritableSource = "remote"

// SetLLMProvider installs (upserts) / rotates a ZERO-URL, broker-pull inference
// provider binding. See the file doc for the full contract.
func (s *Service) SetLLMProvider(ctx context.Context, req prototypes.AgentConfigSetLLMProviderRequest) (prototypes.AgentConfigSetLLMProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, err
	}
	desc, err := validateLLMProviderDescriptor(req.Provider)
	if err != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, err
	}
	// The inference_broker MUST name a boot-declared broker: no admin-writable
	// field determines where the credential is sourced (the credential-plane invariant). An unknown
	// (including any name when no brokers are declared) fails loud BEFORE any
	// state change.
	if _, ok := s.inferenceBrokers[desc.InferenceBroker]; !ok {
		return prototypes.AgentConfigSetLLMProviderResponse{}, fmt.Errorf("%w: %q", ErrLLMProviderBrokerUnknown, desc.InferenceBroker)
	}
	if s.llmProviderInstaller == nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, ErrLLMProviderInstallUnavailable
	}
	q := identity.Quadruple{Identity: id}

	defer s.lockAgent(id.TenantID, req.AgentID)()
	// This live binding does not pass through the revision registry, so it
	// needs the lifecycle fence explicitly. Every other agent-config writer
	// reaches registry.Active/SetRevision and is fenced there.
	if err := s.ensureNotRetired(ctx, q, req.AgentID); err != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, err
	}

	applyErr, emitErr := adminwrite.Apply(
		func() (revert func() error, err error) {
			if instErr := s.llmProviderInstaller.InstallLLMProvider(ctx, id.TenantID, req.AgentID, desc); instErr != nil {
				return nil, instErr
			}
			revert = func() error {
				return s.llmProviderInstaller.UninstallLLMProvider(ctx, desc.Name)
			}
			return revert, nil
		},
		func() error {
			return s.emitLLMProviderSet(ctx, q, req.AgentID, desc)
		},
	)
	if applyErr != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, applyErr
	}
	if emitErr != nil {
		return prototypes.AgentConfigSetLLMProviderResponse{}, emitErr
	}
	return prototypes.AgentConfigSetLLMProviderResponse{
		Name:            desc.Name,
		Installed:       true,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// validateLLMProviderDescriptor validates + normalises the wire descriptor,
// failing loud with ErrInvalidLLMProvider. It enforces the POSITIVE shape (the
// sink/secret fields cannot exist on the struct — the wire handler's
// DisallowUnknownFields decode rejects them by name): a non-empty name; a
// non-empty provider; the credential_source EXACTLY "remote" (an empty value is
// a loud reject — "" means the env source, config/file-only); and a non-empty
// inference_broker. ModelAllow entries are trimmed.
func validateLLMProviderDescriptor(p prototypes.AgentConfigLLMProviderDescriptor) (agentcfg.LLMProviderDescriptor, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return agentcfg.LLMProviderDescriptor{}, fmt.Errorf("%w: name is empty", ErrInvalidLLMProvider)
	}
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		return agentcfg.LLMProviderDescriptor{}, fmt.Errorf("%w: provider is empty", ErrInvalidLLMProvider)
	}
	source := strings.TrimSpace(p.CredentialSource)
	if source == "" {
		return agentcfg.LLMProviderDescriptor{}, fmt.Errorf("%w: credential_source is empty — an installed inference provider must declare credential_source %q (an empty value means the env source, which is config/file-only)", ErrInvalidLLMProvider, llmProviderWritableSource)
	}
	if source != llmProviderWritableSource {
		return agentcfg.LLMProviderDescriptor{}, fmt.Errorf("%w: credential_source %q not installable — only %q (broker-pull) is Protocol-installable", ErrInvalidLLMProvider, p.CredentialSource, llmProviderWritableSource)
	}
	broker := strings.TrimSpace(p.InferenceBroker)
	if broker == "" {
		return agentcfg.LLMProviderDescriptor{}, fmt.Errorf("%w: inference_broker is empty — an installed provider references a boot-declared broker by name (every credential sink is boot-pinned)", ErrInvalidLLMProvider)
	}
	models := make([]string, 0, len(p.ModelAllow))
	for _, m := range p.ModelAllow {
		if mm := strings.TrimSpace(m); mm != "" {
			models = append(models, mm)
		}
	}
	if len(models) == 0 {
		models = nil
	}
	return agentcfg.LLMProviderDescriptor{
		Name:             name,
		Provider:         provider,
		CredentialSource: source,
		InferenceBroker:  broker,
		ModelAllow:       models,
	}, nil
}

// emitLLMProviderSet publishes the install / rotate admin-scope audit event and
// RETURNS a publish failure so the caller fails the write closed. A nil bus is a
// no-op success. NO URL / env-var name / secret is ever carried.
func (s *Service) emitLLMProviderSet(ctx context.Context, q identity.Quadruple, agentID string, desc agentcfg.LLMProviderDescriptor) error {
	if s.bus == nil {
		return nil
	}
	now := s.now().UTC()
	ev := events.Event{
		Type:       agentcfg.EventTypeLLMProviderInstalled,
		Identity:   q,
		OccurredAt: now,
		Payload: agentcfg.LLMProviderSetPayload{
			Author:          q,
			AgentID:         agentID,
			ProviderName:    desc.Name,
			Provider:        desc.Provider,
			InferenceBroker: desc.InferenceBroker,
			OccurredAt:      now,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("agentcfg/protocol: publish %s: %w", ev.Type, err)
	}
	return nil
}
