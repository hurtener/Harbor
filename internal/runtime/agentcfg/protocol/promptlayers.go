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
// Desired-state replace semantics: a set REPLACES ONLY the prompt-layer
// section of the envelope; the skills + tool-exposure + connections +
// llm-params + hooks sections of the active revision are preserved (a
// prompt edit never clears an agent's skills, MCP pause state, or a pinned
// run-completion hook — the symmetric invariant the skills + tool-exposure
// services honour for prompt layers). The edit applies NEXT-turn (the
// run-start projection reads the active revision; the immutable per-run
// snapshot is undisturbed for in-flight runs).
//
// The base / user composition order is the structural security boundary:
// Base and User are distinct fields and the user layer is always composed
// ABOVE the base at run start, so a writer of only User can extend the
// operator's guidance but can never precede, replace, or weaken the
// operator base.

// SetPromptLayers records a new config revision pinning the supplied layered
// system prompt (operator base and/or user layer) as a desired-state replace
// of the prompt-layer section. The skills + tool-exposure + connections +
// llm-params + hooks sections of the active revision are carried forward
// unchanged.
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

	// Read the active revision so the new revision PRESERVES the skills +
	// tool-exposure sections (a prompt edit replaces only its own section).
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	payload := agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{
			Base: copyStringPtr(req.PromptLayers.Base),
			User: copyStringPtr(req.PromptLayers.User),
		},
	}
	if hasActive {
		payload.Skills = active.Payload.Skills
		payload.ToolExposure = active.Payload.ToolExposure
		payload.Connections = active.Payload.Connections
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload)
	if err != nil {
		return prototypes.AgentConfigSetPromptLayersResponse{}, err
	}
	return prototypes.AgentConfigSetPromptLayersResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
