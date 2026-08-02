package protocol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
// Prompt/disable methods write the session OVERLAY (keyed by the real triple,
// so it is session-isolated). Personal-skill methods use one injected
// controller that owns the agent-owned record CAS and the tier-only read.
// None of them
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
	if s.sessionPersonalSkills == nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	ov, err := s.sessionOverlay.SetUserPrompt(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.UserPrompt)
	if err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	_, names, err := s.loadSessionSkillProjection(ctx, identity.Quadruple{Identity: id}, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetUserPromptResponse{}, err
	}
	return prototypes.AgentConfigSessionSetUserPromptResponse{
		Overlay:         overlayToWire(ov, names),
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
	if s.sessionPersonalSkills == nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	ov, err := s.sessionOverlay.SetSourceDisables(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.DisabledServers, req.DisabledTools)
	if err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	_, names, err := s.loadSessionSkillProjection(ctx, identity.Quadruple{Identity: id}, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSetSourceDisablesResponse{}, err
	}
	return prototypes.AgentConfigSessionSetSourceDisablesResponse{
		Overlay:         overlayToWire(ov, names),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSkillsList lists the session's skills (metadata only) under the
// caller's real triple and selected agent. It intentionally returns only the
// controller's ScopeSession tier; ScopeUser composition belongs to Directory
// and the general skill tools.
func (s *Service) SessionSkillsList(ctx context.Context, req prototypes.AgentConfigSessionSkillsListRequest) (prototypes.AgentConfigSessionSkillsListResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, err
	}
	if s.sessionPersonalSkills == nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, ErrSkillsUnavailable
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsListResponse{}, err
	}
	list, _, err := s.loadSessionSkillProjection(ctx, identity.Quadruple{Identity: id}, req.AgentID)
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
// real triple through the injected controller's one-CAS mutation. The skill
// scope is FORCED to session — a session personal skill never promotes to the
// agent/tenant scope. The response reloads the authoritative tier and derives
// names dynamically; it never mutates legacy Overlay.PersonalSkills.
func (s *Service) SessionSkillsUpsert(ctx context.Context, req prototypes.AgentConfigSessionSkillsUpsertRequest) (prototypes.AgentConfigSessionSkillsUpsertResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	if s.sessionPersonalSkills == nil {
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
	if err := s.sessionPersonalSkills.UpsertSessionSkill(ctx, q, req.AgentID, skill); err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	current, names, err := s.loadSessionSkillProjection(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	stored, found := findSessionSkill(current, skill.Name)
	if !found {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, fmt.Errorf("%w: controller omitted upserted session skill %q", skills.ErrSkillNotFound, skill.Name)
	}
	ov, _, err := s.sessionOverlay.Get(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsUpsertResponse{}, err
	}
	return prototypes.AgentConfigSessionSkillsUpsertResponse{
		Skill:           skillToSummary(stored),
		Overlay:         overlayToWire(ov, names),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// SessionSkillsDelete logically deletes a personal skill under the caller's
// real triple through the injected controller's one-CAS mutation. It reloads
// the authoritative tier for the response and never writes the legacy overlay
// name field.
func (s *Service) SessionSkillsDelete(ctx context.Context, req prototypes.AgentConfigSessionSkillsDeleteRequest) (prototypes.AgentConfigSessionSkillsDeleteResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	if s.sessionPersonalSkills == nil {
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
	// RUNG-PRECISE: the controller owns ONLY the selected agent's session tier;
	// a same-named durable user-scope skill is outside this mutation authority.
	if err := s.sessionPersonalSkills.DeleteSessionSkill(ctx, q, req.AgentID, req.Name); err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	_, names, err := s.loadSessionSkillProjection(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	ov, _, err := s.sessionOverlay.Get(ctx, q, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSessionSkillsDeleteResponse{}, err
	}
	return prototypes.AgentConfigSessionSkillsDeleteResponse{
		Overlay:         overlayToWire(ov, names),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// loadSessionSkillProjection reloads the controller-owned session tier and
// derives its deterministic dynamic name projection. A defensive ScopeSession
// filter ensures this session-only Protocol surface can never accidentally
// expose ScopeUser even if a controller implementation violates its contract.
func (s *Service) loadSessionSkillProjection(ctx context.Context, id identity.Quadruple, agentID string) ([]skills.Skill, []string, error) {
	if s.sessionPersonalSkills == nil {
		return nil, nil, ErrSkillsUnavailable
	}
	loaded, err := s.sessionPersonalSkills.SessionSkills(ctx, id, agentID)
	if err != nil {
		return nil, nil, err
	}
	current := make([]skills.Skill, 0, len(loaded))
	names := make([]string, 0, len(loaded))
	seen := make(map[string]struct{}, len(loaded))
	for _, skill := range loaded {
		if skill.Scope != skills.ScopeSession {
			continue
		}
		current = append(current, skill)
		if _, ok := seen[skill.Name]; !ok {
			seen[skill.Name] = struct{}{}
			names = append(names, skill.Name)
		}
	}
	sort.Strings(names)
	return current, names, nil
}

func findSessionSkill(list []skills.Skill, name string) (skills.Skill, bool) {
	for _, skill := range list {
		if strings.EqualFold(strings.TrimSpace(skill.Name), strings.TrimSpace(name)) {
			return skill, true
		}
	}
	return skills.Skill{}, false
}

// overlayToWire projects a domain session overlay plus the current dynamic
// personal-name view onto the wire shape (defensive copies; no shared backing
// slices). The shape carries no base field — a session caller never sees or
// writes the operator base.
func overlayToWire(o sessionoverlay.Overlay, personalNames []string) prototypes.AgentConfigSessionOverlay {
	return prototypes.AgentConfigSessionOverlay{
		UserPrompt:      o.UserPrompt,
		DisabledServers: append([]string(nil), o.DisabledServers...),
		DisabledTools:   append([]string(nil), o.DisabledTools...),
		PersonalSkills:  append([]string(nil), personalNames...),
	}
}
