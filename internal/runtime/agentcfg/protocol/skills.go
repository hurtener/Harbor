package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
)

// skills.go — the agent-config skills-control methods: the first consumer
// of the registry primitive (CLAUDE.md §13). Each mutation drives the
// existing SkillStore AND records the membership change as a config
// revision, so skills inherit the unified diff + rollback. The
// pack-overwrite-refusal conflict policy is honoured at the Protocol edge:
// the typed `skills.ErrPackOverwriteRefused` surfaces as a Protocol error,
// never a silent overwrite (§13).

// SkillsList returns the agent's skills (metadata only) from the
// SkillStore under the caller's identity.
func (s *Service) SkillsList(ctx context.Context, req prototypes.AgentConfigSkillsListRequest) (prototypes.AgentConfigSkillsListResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSkillsListResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSkillsListResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSkillsListResponse{}, err
	}
	list, err := s.skills.List(ctx, identity.Quadruple{Identity: id}, skills.ListFilter{})
	if err != nil {
		return prototypes.AgentConfigSkillsListResponse{}, err
	}
	out := make([]prototypes.AgentConfigSkillSummary, 0, len(list))
	for _, sk := range list {
		out = append(out, skillToSummary(sk))
	}
	return prototypes.AgentConfigSkillsListResponse{
		Skills:          out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SkillsUpsert upserts a skill into the SkillStore and records the
// membership change as a config revision. A pack-overwrite refusal
// surfaces as the typed `skills.ErrPackOverwriteRefused` (mapped to
// CodeInvalidRequest at the wire edge) — never a silent overwrite.
func (s *Service) SkillsUpsert(ctx context.Context, req prototypes.AgentConfigSkillsUpsertRequest) (prototypes.AgentConfigSkillsUpsertResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSkillsUpsertResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSkillsUpsertResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSkillsUpsertResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	skill := skillFromInput(req.Skill)
	// Ordering: the body is written to the SkillStore FIRST, then the
	// membership revision. The two are not transactional — if the membership
	// write fails, the body persists (and its skill.upserted already fired)
	// but no membership revision references it, so projection never includes
	// it; the operator gets the error and an idempotent retry self-heals
	// (re-upsert is a no-op body write + the membership revision). A stranded
	// body is inert (membership is the projection gate), never a silent
	// partial success.
	if err := s.skills.Upsert(ctx, q, skill); err != nil {
		// ErrPackOverwriteRefused (and any validation error) propagate
		// up — the wire handler classifies them. No silent overwrite.
		return prototypes.AgentConfigSkillsUpsertResponse{}, err
	}
	// Read the stored skill back so the response summary carries the
	// store-computed content hash + timestamps.
	stored, err := s.skills.Get(ctx, q, skill.Name)
	if err != nil {
		return prototypes.AgentConfigSkillsUpsertResponse{}, err
	}
	rev, err := s.recordSkillsMembership(ctx, q, req.AgentID, addName, skill.Name)
	if err != nil {
		return prototypes.AgentConfigSkillsUpsertResponse{}, err
	}
	return prototypes.AgentConfigSkillsUpsertResponse{
		Revision:        revisionToWire(rev),
		Skill:           skillToSummary(stored),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SkillsDelete deletes a skill from the SkillStore and records the
// membership change as a config revision.
func (s *Service) SkillsDelete(ctx context.Context, req prototypes.AgentConfigSkillsDeleteRequest) (prototypes.AgentConfigSkillsDeleteResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSkillsDeleteResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSkillsDeleteResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSkillsDeleteResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	if req.Name == "" {
		return prototypes.AgentConfigSkillsDeleteResponse{}, fmt.Errorf("%w: skill name is empty", ErrIdentityRequired)
	}
	if err := s.skills.Delete(ctx, q, req.Name); err != nil {
		return prototypes.AgentConfigSkillsDeleteResponse{}, err
	}
	rev, err := s.recordSkillsMembership(ctx, q, req.AgentID, removeName, req.Name)
	if err != nil {
		return prototypes.AgentConfigSkillsDeleteResponse{}, err
	}
	return prototypes.AgentConfigSkillsDeleteResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// membershipOp selects how recordSkillsMembership mutates the active set.
type membershipOp int

const (
	addName membershipOp = iota
	removeName
)

// recordSkillsMembership reads the agent's current active skills
// membership, applies the add/remove delta, and writes a new config
// revision pinning the result. The new membership is computed from the
// active revision's set (not from the SkillStore), so the revision trail
// is a pure function of prior revisions — the SkillStore holds bodies, the
// registry holds the versioned membership.
func (s *Service) recordSkillsMembership(ctx context.Context, q identity.Quadruple, agentID string, op membershipOp, name string) (agentcfg.Revision, error) {
	defer s.lockAgent(q.TenantID, agentID)()
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	set := map[string]struct{}{}
	if hasActive {
		for _, n := range active.Payload.SkillNames() {
			set[n] = struct{}{}
		}
	}
	switch op {
	case addName:
		set[name] = struct{}{}
	case removeName:
		delete(set, name)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	// Compose the new revision so it REPLACES only the skills section and
	// PRESERVES the tool-exposure + prompt + connections + llm-params + hooks
	// sections of the active revision — a skills edit must never silently
	// clear an agent's MCP pause state, prompt layers, or a pinned
	// run-completion hook (the symmetric invariant set_tool_exposure honours
	// for skills). NormalizePayload sorts + de-dups the membership, so the
	// order here is not significant for the content hash.
	payload := agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: names},
	}
	if hasActive {
		payload.ToolExposure = active.Payload.ToolExposure
		payload.PromptLayers = active.Payload.PromptLayers
		payload.Connections = active.Payload.Connections
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
	}
	return s.registry.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, payload)
}

// skillFromInput maps a wire skill input onto a runtime skills.Skill. The
// SkillStore validates the mandatory fields (name, trigger, ≥1 step,
// origin, scope) at Upsert.
func skillFromInput(in prototypes.AgentConfigSkillInput) skills.Skill {
	return skills.Skill{
		Name:        in.Name,
		Title:       in.Title,
		Description: in.Description,
		Trigger:     in.Trigger,
		TaskType:    in.TaskType,
		Tags:        append([]string(nil), in.Tags...),
		Steps:       append([]string(nil), in.Steps...),
		Origin:      skills.Origin(in.Origin),
		Scope:       skills.Scope(in.Scope),
	}
}

// skillToSummary projects a runtime skills.Skill onto the wire summary —
// metadata only, never the body.
func skillToSummary(sk skills.Skill) prototypes.AgentConfigSkillSummary {
	return prototypes.AgentConfigSkillSummary{
		Name:        sk.Name,
		Title:       sk.Title,
		Trigger:     sk.Trigger,
		TaskType:    sk.TaskType,
		Origin:      string(sk.Origin),
		Scope:       string(sk.Scope),
		ContentHash: sk.ContentHash,
		UpdatedAt:   sk.UpdatedAt,
	}
}
