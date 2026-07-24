package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
)

// userskills.go — the CLAIM-FREE durable-per-user skills verb family
// (`agent_config.user.skills.*`). It completes the half-wired user-durable
// skill seam: a plain authenticated user authors PERSONAL skills that persist
// across ALL of their conversations (not per-session).
//
// # Why claim-free (no admin, no auth.ScopeAgentConfigUser)
//
// A personal skill cannot widen capability, so authoring one is as safe as the
// existing session-skills rung. Two mechanisms enforce this independently of
// this verb: `internal/skills/capfilter` is default-deny, and
// `internal/skills/tools/redactor.go` SCRUBS any tool name a skill references
// that is not in the run's allowed set (rewriting it to "a suitable tool (use
// tool_search)"). A skill's RequiredTools is provenance/filter metadata, never
// a grant. So the durable-per-user rung needs only a valid identity — the same
// tier as `agent_config.session.skills.*`.
//
// # Durability = scope, not a knob
//
// The verb force-stamps `skills.ScopeUser`, whose rows persist session-zeroed
// so they resolve across every session of the same (tenant, user). Physical
// durability rides the driver: the in-memory dev store is ephemeral;
// sqlite/postgres survive restart. There is no separate durability knob (the
// analogue of the session verb force-stamping `Scope=session`).
//
// # Composition with the user config variant
//
// The upsert/delete write the skill BODY to the SkillStore at user scope AND
// record the name into the caller's user-scope config revision's skills
// membership (agentcfg.ConfigScopeUser) — the same durable membership the
// `AgentConfigUserPayload.PersonalSkills` field addresses. So the durable body
// verbs COMPOSE with that membership field: `set_revision` sets the membership
// declaratively; these verbs write the body and mutate the membership
// incrementally, giving the durable rung the same diff/rollback trail as the
// rest of the user config variant. The run-start projection reads that
// membership so durable user skills stay visible even when the admin pins a
// skills membership.

// UserSkillsList lists the caller's durable user-scope personal skills
// (metadata only) under their real (tenant, user). The SkillStore resolves
// ScopeUser rows across the caller's sessions, so the list is durable and
// conversation-independent.
func (s *Service) UserSkillsList(ctx context.Context, req prototypes.AgentConfigUserSkillsListRequest) (prototypes.AgentConfigUserSkillsListResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserSkillsListResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigUserSkillsListResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserSkillsListResponse{}, err
	}
	list, err := s.skills.List(ctx, identity.Quadruple{Identity: id}, skills.ListFilter{Scope: skills.ScopeUser})
	if err != nil {
		return prototypes.AgentConfigUserSkillsListResponse{}, err
	}
	out := make([]prototypes.AgentConfigSkillSummary, 0, len(list))
	for _, sk := range list {
		out = append(out, skillToSummary(sk))
	}
	return prototypes.AgentConfigUserSkillsListResponse{
		Skills:          out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// UserSkillsUpsert upserts a DURABLE personal skill at user scope and records
// its name in the caller's user-scope config revision membership. The skill
// scope is FORCED to skills.ScopeUser — a session caller cannot widen the
// visibility scope, and the durable rung is keyed (tenant, user).
func (s *Service) UserSkillsUpsert(ctx context.Context, req prototypes.AgentConfigUserSkillsUpsertRequest) (prototypes.AgentConfigUserSkillsUpsertResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	skill := skillFromInput(req.Skill)
	// Force user scope: a durable personal skill is keyed (tenant, user) and
	// stored session-zeroed. It never carries a project/tenant scope id.
	skill.Scope = skills.ScopeUser
	skill.ScopeTenantID = ""
	skill.ScopeProjectID = ""
	// Body first, then the membership revision (same ordering + self-healing
	// rationale as the admin SkillsUpsert): a stranded body is inert because
	// the membership is the projection gate; an idempotent retry self-heals.
	if err := s.skills.Upsert(ctx, q, skill); err != nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, err
	}
	stored, err := s.userScopeSkillByName(ctx, q, skill.Name)
	if err != nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, err
	}
	rev, err := s.recordUserSkillsMembership(ctx, q, req.AgentID, addName, skill.Name)
	if err != nil {
		return prototypes.AgentConfigUserSkillsUpsertResponse{}, err
	}
	return prototypes.AgentConfigUserSkillsUpsertResponse{
		Skill:           skillToSummary(stored),
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// UserSkillsDelete deletes one of the caller's durable user-scope personal
// skills and removes its name from the user-scope membership revision.
func (s *Service) UserSkillsDelete(ctx context.Context, req prototypes.AgentConfigUserSkillsDeleteRequest) (prototypes.AgentConfigUserSkillsDeleteResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, err
	}
	if req.Name == "" {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, fmt.Errorf("%w: skill name is empty", ErrIdentityRequired)
	}
	q := identity.Quadruple{Identity: id}
	// RUNG-PRECISE: the durable user verb deletes ONLY the ScopeUser row
	// (keyed (tenant, user), session-independent — the intended cross-session
	// durable delete). It must never touch a same-named session-scoped row.
	if err := s.skills.Delete(ctx, q, req.Name, skills.ScopeUser); err != nil {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, err
	}
	// Remove the name from the durable ConfigScopeUser membership so the
	// revision never lists a name whose body is gone.
	rev, err := s.recordUserSkillsMembership(ctx, q, req.AgentID, removeName, req.Name)
	if err != nil {
		return prototypes.AgentConfigUserSkillsDeleteResponse{}, err
	}
	return prototypes.AgentConfigUserSkillsDeleteResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// userScopeSkillByName reads a single user-scope skill back by name via a
// scope-filtered List. The shared SkillStore.Get resolves the exact-session
// row preferentially on a name collision, so it is the wrong path for the
// durable rung; a Scope=user List is unambiguous and returns the store-computed
// content hash + timestamps for the response summary.
func (s *Service) userScopeSkillByName(ctx context.Context, q identity.Quadruple, name string) (skills.Skill, error) {
	list, err := s.skills.List(ctx, q, skills.ListFilter{Scope: skills.ScopeUser})
	if err != nil {
		return skills.Skill{}, err
	}
	for _, sk := range list {
		if sk.Name == name {
			return sk, nil
		}
	}
	return skills.Skill{}, fmt.Errorf("%w: name=%q (user scope)", skills.ErrSkillNotFound, name)
}

// recordUserSkillsMembership reads the caller's active USER-scope config
// revision, applies the add/remove delta to its skills membership, and writes
// a new user-scope revision pinning the result — PRESERVING the user prompt +
// tool-exposure sections of the active revision so a skills edit never clears
// the user's durable prompt layer or narrow-only disables. Serialised
// per-owner (ConfigScopeUser, tenant, real-user, agent) so distinct users
// never contend. This is the user-scope analogue of recordSkillsMembership.
func (s *Service) recordUserSkillsMembership(ctx context.Context, q identity.Quadruple, agentID string, op membershipOp, name string) (agentcfg.Revision, error) {
	defer s.lockOwner(agentcfg.ConfigScopeUser, q.TenantID, q.UserID, agentID)()
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeUser)
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
	// Replace ONLY the skills section; preserve the user prompt + tool-exposure
	// sections (the restricted user-scope surface — Base / Connections /
	// LLMParams stay nil for user scope). NormalizePayload sorts + de-dups the
	// membership, so the order here is not significant for the content hash.
	payload := agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: names},
	}
	if hasActive {
		payload.PromptLayers = active.Payload.PromptLayers
		payload.ToolExposure = active.Payload.ToolExposure
	}
	return s.registry.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeUser, payload)
}
