package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// promptlayers.go — the agent-config layered-system-prompt control method:
// `agent_config.set_prompt_layers`. It is a consumer of the registry
// primitive (CLAUDE.md §13) — a prompt-layer edit composes a new config
// revision (so the layered prompt inherits the unified diff + rollback) and
// the registry emits the generic `agent.config.revised` event.
//
// The registry merges the supplied prompt layers with the active revision.
// Other config sections carry forward. The edit applies on the next run.

// SetPromptLayers records an admin prompt-layer edit. Guidance-only edits
// preserve the base/user pair. Other edits replace that pair. Omitted
// guidance carries forward and explicit blank guidance restores inheritance.
func (s *Service) SetPromptLayers(ctx context.Context, req prototypes.AgentConfigSetPromptLayersRequest) (prototypes.AgentConfigSetPromptLayersResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()

	// Read the active revision to carry forward sibling sections.
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	payload := carrySiblingsForward(active, hasActive)
	if hasActive {
		// Provider-editing callers of carrySiblingsForward rebuild this section.
		payload.OAuthProviders = active.Payload.OAuthProviders
	}
	payload.PromptLayers = &agentcfg.PromptLayers{
		Base:     copyStringPtr(req.PromptLayers.Base),
		User:     copyStringPtr(req.PromptLayers.User),
		Guidance: copyStringPtr(req.PromptLayers.Guidance),
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
		agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash, MergePromptLayers: true})
	if err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	return prototypes.AgentConfigSetPromptLayersResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
