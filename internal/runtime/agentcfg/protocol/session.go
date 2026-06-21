package protocol

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
)

// session.go — the SESSION-SAFE subset of the agent-config control plane:
// the non-admin lower tier of the authorization matrix. A session-scoped
// (non-admin) end user may, under the caller's REAL (tenant, user, session)
// triple:
//
//   - set a USER prompt layer that composes ABOVE the operator base (it can
//     extend the operator's guidance but never precede, replace, or weaken
//     the base — the session-writable shape carries no Base field, so a
//     session caller physically cannot edit the operator base);
//   - NARROW (never widen) source/tool enablement by naming servers/tools to
//     DISABLE — at run start the disable set is UNIONED into the admin
//     exclusion set, so a session edit can only narrow the admin-allowed
//     exposure (there is no enable path, in the shape OR the projection);
//   - manage EPHEMERAL personal skills (session-scoped; they never promote to
//     the agent/tenant scope — the scope is forced to session here).
//
// Every method writes the session OVERLAY (keyed by the real triple, so it
// is session-isolated) and/or the session-scoped SkillStore. None of them
// touches the admin agent-config registry (the agent-level desired state a
// session caller cannot mutate). The tier is enforced at the wire handler
// from the verified ctx scope (a non-admin caller is permitted ONLY on these
// verbs); the data-model boundary here is the second, defence-in-depth layer.

// ErrSessionOverlayUnavailable — a session-safe method was called but no
// session-overlay store was wired into the Service.
var ErrSessionOverlayUnavailable = errors.New("agentcfg/protocol: session safe-subset control not wired on this runtime")

// SessionSetUserPrompt sets ONLY the session's user prompt layer. The
// session-writable shape carries no base field, so this can never alter the
// operator base — base-unwritable-by-session is structural.
func (s *Service) SessionSetUserPrompt(ctx context.Context, req prototypes.AgentConfigSessionSetUserPromptRequest) (prototypes.AgentConfigSessionSetUserPromptResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	if s.sessionOverlay == nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, ErrSessionOverlayUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	ov, err := s.sessionOverlay.SetUserPrompt(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.UserPrompt)
	if err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	return prototypes.AgentConfigSessionSetUserPromptResponse{
		Overlay:         overlayToWire(ov),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSetSourceDisables records the session's narrow-only disable set
// (servers + tools). There is intentionally no enable field: the set names
// what the session wants OFF, and the run-start projection unions it into the
// admin exclusion set — so a session edit can only narrow the admin-allowed
// exposure, never widen it.
func (s *Service) SessionSetSourceDisables(ctx context.Context, req prototypes.AgentConfigSessionSetSourceDisablesRequest) (prototypes.AgentConfigSessionSetSourceDisablesResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	if s.sessionOverlay == nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, ErrSessionOverlayUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	ov, err := s.sessionOverlay.SetSourceDisables(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.DisabledServers, req.DisabledTools)
	if err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	return prototypes.AgentConfigSessionSetSourceDisablesResponse{
		Overlay:         overlayToWire(ov),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSkillsList lists the session's skills (metadata only) under the
// caller's real triple. The SkillStore is already session-scoped, so the
// list is naturally isolated to the caller's session.
func (s *Service) SessionSkillsList(ctx context.Context, req prototypes.AgentConfigSessionSkillsListRequest) (prototypes.AgentConfigSessionSkillsListResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, err
	}
	list, err := s.skills.List(ctx, identity.Quadruple{Identity: id}, skills.ListFilter{})
	if err != nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, err
	}
	out := make([]prototypes.AgentConfigSkillSummary, 0, len(list))
	for _, sk := range list {
		out = append(out, skillToSummary(sk))
	}
	return prototypes.AgentConfigSessionSkillsListResponse{
		Skills:          out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSkillsUpsert upserts an EPHEMERAL personal skill under the caller's
// real triple and records its name in the session overlay (so the run-start
// projection adds it back above the admin membership filter). The skill scope
// is FORCED to session — a session personal skill never promotes to the
// agent/tenant scope.
func (s *Service) SessionSkillsUpsert(ctx context.Context, req prototypes.AgentConfigSessionSkillsUpsertRequest) (prototypes.AgentConfigSessionSkillsUpsertResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, ErrSkillsUnavailable
	}
	if s.sessionOverlay == nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, ErrSessionOverlayUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	q := identity.Quadruple{Identity: id}
	skill := skillFromInput(req.Skill)
	// Force session scope: an ephemeral personal skill never promotes past the
	// caller's session (a session caller cannot widen the visibility scope).
	skill.Scope = skills.ScopeSession
	skill.ScopeTenantID = ""
	skill.ScopeProjectID = ""
	if err := s.skills.Upsert(ctx, q, skill); err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	stored, err := s.skills.Get(ctx, q, skill.Name)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	ov, err := s.sessionOverlay.AddPersonalSkill(ctx, q, req.AgentID, skill.Name)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	return prototypes.AgentConfigSessionSkillsUpsertResponse{
		Skill:           skillToSummary(stored),
		Overlay:         overlayToWire(ov),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSkillsDelete deletes a personal skill under the caller's real triple
// and removes its name from the session overlay.
func (s *Service) SessionSkillsDelete(ctx context.Context, req prototypes.AgentConfigSessionSkillsDeleteRequest) (prototypes.AgentConfigSessionSkillsDeleteResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	if s.skills == nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, ErrSkillsUnavailable
	}
	if s.sessionOverlay == nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, ErrSessionOverlayUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	if req.Name == "" {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, fmt.Errorf("%w: skill name is empty", ErrIdentityRequired)
	}
	q := identity.Quadruple{Identity: id}
	if err := s.skills.Delete(ctx, q, req.Name); err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	ov, err := s.sessionOverlay.RemovePersonalSkill(ctx, q, req.AgentID, req.Name)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	return prototypes.AgentConfigSessionSkillsDeleteResponse{
		Overlay:         overlayToWire(ov),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// overlayToWire projects a domain session overlay onto the wire shape
// (defensive copies; no shared backing slices). The shape carries no base
// field — a session caller never sees or writes the operator base.
func overlayToWire(o sessionoverlay.Overlay) prototypes.AgentConfigSessionOverlay {
	return prototypes.AgentConfigSessionOverlay{
		UserPrompt:      o.UserPrompt,
		DisabledServers: append([]string(nil), o.DisabledServers...),
		DisabledTools:   append([]string(nil), o.DisabledTools...),
		PersonalSkills:  append([]string(nil), o.PersonalSkills...),
	}
}
