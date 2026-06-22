package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// llmparams.go — the agent-config per-agent LLM-parameter control method:
// `agent_config.set_llm_params`. It is a consumer of the registry primitive
// (CLAUDE.md §13) — an LLM-params edit composes a new config revision (so the
// per-agent sampling defaults inherit the unified diff + rollback) and the
// registry emits the generic `agent.config.revised` event.
//
// Desired-state replace semantics: a set REPLACES ONLY the LLM-params section
// of the envelope; the prompt-layer + skills + tool-exposure + connection
// sections of the active revision are preserved (the bidirectional
// section-merge invariant every convenience verb honours).
//
// Resolution: the per-agent params override the tenant-wide baseline at run
// start (session › per-agent › tenant-wide baseline › config default). A set
// Model is validated against the configured ModelProfiles HERE, at set time
// (parity with the tenant model-swap) — an invalid model can never be
// persisted, so the run-start path never has to fall back silently
// (CLAUDE.md §13). The edit applies NEXT-turn (the run-start projection reads
// the active revision; in-flight runs keep their immutable snapshot).

// SetLLMParams records a new config revision pinning the supplied per-agent
// LLM-parameter section (model / temperature / max-tokens / reasoning-effort)
// as a desired-state replace of the LLM-params section. A set Model is
// validated against the configured ModelProfiles (an unknown model is
// rejected with ErrUnknownModel). The prompt-layer + skills + tool-exposure +
// connection sections of the active revision are carried forward unchanged.
func (s *Service) SetLLMParams(ctx context.Context, req prototypes.AgentConfigSetLLMParamsRequest) (prototypes.AgentConfigSetLLMParamsResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetLLMParamsResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetLLMParamsResponse{}, err
	}
	// Validate the section at set time so an invalid model or out-of-range
	// sampling value is never persisted.
	if err := s.validateLLMParams(&req.LLMParams); err != nil {
		return prototypes.AgentConfigSetLLMParamsResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	defer s.lockAgent(id.TenantID, req.AgentID)()

	// Read the active revision so the new revision PRESERVES the prompt-layer
	// + skills + tool-exposure + connection sections (an LLM-params edit
	// replaces only its own section).
	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetLLMParamsResponse{}, err
	}
	payload := agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{
			Model:           copyStringPtr(req.LLMParams.Model),
			Temperature:     copyFloat64Ptr(req.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(req.LLMParams.MaxTokens),
			ReasoningEffort: copyStringPtr(req.LLMParams.ReasoningEffort),
		},
	}
	if hasActive {
		payload.PromptLayers = active.Payload.PromptLayers
		payload.Skills = active.Payload.Skills
		payload.ToolExposure = active.Payload.ToolExposure
		payload.Connections = active.Payload.Connections
	}

	rev, err := s.registry.SetRevision(ctx, q, req.AgentID, payload)
	if err != nil {
		return prototypes.AgentConfigSetLLMParamsResponse{}, err
	}
	return prototypes.AgentConfigSetLLMParamsResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}
