package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

// sessionSvc builds a Service wired with BOTH the skills store and the
// session-overlay store (the safe-subset lower tier needs both).
func sessionSvc(t *testing.T) *agentcfgprotocol.Service {
	t.Helper()
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	ov, err := sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("overlay store: %v", err)
	}
	s, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithSkillStore(newSkills(t)),
		agentcfgprotocol.WithSessionOverlay(ov),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// TestSessionSetUserPrompt_RecordsOverlay proves the session user prompt is
// stored and round-trips through the overlay. The session shape carries no
// Base field — base-unwritable is structural (there is nothing to assert
// against because there is no base path on the session surface at all).
func TestSessionSetUserPrompt_RecordsOverlay(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	resp, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: scope(), AgentID: testAgentID, UserPrompt: "be concise",
	})
	if err != nil {
		t.Fatalf("set user prompt: %v", err)
	}
	if resp.Overlay.UserPrompt != "be concise" {
		t.Fatalf("user prompt not recorded: %+v", resp.Overlay)
	}
}

// TestSessionSetSourceDisables_NarrowOnly proves the session disable set is
// recorded as a DISABLE list. There is no enable field on the request shape,
// so a session can never express "enable a not-allowed source" — widening is
// structurally impossible on this surface.
func TestSessionSetSourceDisables_NarrowOnly(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	resp, err := s.SessionSetSourceDisables(ctx, prototypes.AgentConfigSessionSetSourceDisablesRequest{
		Identity: scope(), AgentID: testAgentID,
		DisabledServers: []string{"srvA", "srvA"}, // de-duped
		DisabledTools:   []string{"toolZ"},
	})
	if err != nil {
		t.Fatalf("set source disables: %v", err)
	}
	if len(resp.Overlay.DisabledServers) != 1 || resp.Overlay.DisabledServers[0] != "srvA" {
		t.Fatalf("disabled servers = %+v", resp.Overlay.DisabledServers)
	}
	if len(resp.Overlay.DisabledTools) != 1 || resp.Overlay.DisabledTools[0] != "toolZ" {
		t.Fatalf("disabled tools = %+v", resp.Overlay.DisabledTools)
	}
}

// TestSessionSkills_UpsertListDelete proves the ephemeral personal-skill
// lifecycle: upsert lands in the session-scoped SkillStore + the overlay's
// personal-skills set; list returns it; delete removes it from both.
func TestSessionSkills_UpsertListDelete(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	up, err := s.SessionSkillsUpsert(ctx, prototypes.AgentConfigSessionSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{
			Name: "personal-a", Trigger: "when asked", Steps: []string{"do it"},
			Origin: "generated", Scope: "tenant", // requested scope ignored — forced to session
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if up.Skill.Scope != string(skills.ScopeSession) {
		t.Fatalf("personal skill must be session-scoped (never promote); got %q", up.Skill.Scope)
	}
	if len(up.Overlay.PersonalSkills) != 1 || up.Overlay.PersonalSkills[0] != "personal-a" {
		t.Fatalf("overlay personal skills = %+v", up.Overlay.PersonalSkills)
	}
	list, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(list.Skills) != 1 {
		t.Fatalf("list = %d err=%v", len(list.Skills), err)
	}
	del, err := s.SessionSkillsDelete(ctx, prototypes.AgentConfigSessionSkillsDeleteRequest{
		Identity: scope(), AgentID: testAgentID, Name: "personal-a",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(del.Overlay.PersonalSkills) != 0 {
		t.Fatalf("personal skill not removed from overlay: %+v", del.Overlay.PersonalSkills)
	}
}

// TestSession_Unwired proves the session verbs fail loud (not silently) when
// the overlay store is not wired — never a stub default.
func TestSession_Unwired(t *testing.T) {
	ctx := context.Background()
	s := svc(t, true) // skills wired, overlay NOT wired
	_, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: scope(), AgentID: testAgentID, UserPrompt: "x",
	})
	if !errors.Is(err, agentcfgprotocol.ErrSessionOverlayUnavailable) {
		t.Fatalf("unwired overlay should fail with ErrSessionOverlayUnavailable, got %v", err)
	}
}

// TestSession_IdentityRequired proves an incomplete identity triple fails
// closed on the session surface.
func TestSession_IdentityRequired(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	_, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "", Session: "s"}, AgentID: testAgentID, UserPrompt: "x",
	})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should fail with ErrIdentityRequired, got %v", err)
	}
}

// TestSession_CrossSessionIsolation proves one session's overlay + personal
// skills are invisible to another session (the overlay is keyed by the real
// triple; the SkillStore is session-scoped).
func TestSession_CrossSessionIsolation(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	sessA := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionA"}
	sessB := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionB"}

	if _, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: sessA, AgentID: testAgentID, UserPrompt: "A-only",
	}); err != nil {
		t.Fatalf("set A: %v", err)
	}
	if _, err := s.SessionSkillsUpsert(ctx, prototypes.AgentConfigSessionSkillsUpsertRequest{
		Identity: sessA, AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "a-skill", Trigger: "t", Steps: []string{"s"}, Origin: "generated", Scope: "session"},
	}); err != nil {
		t.Fatalf("upsert A skill: %v", err)
	}

	// Session B sees no overlay user prompt and no personal skill from A.
	bList, err := s.SessionSkillsList(ctx, prototypes.AgentConfigSessionSkillsListRequest{Identity: sessB, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(bList.Skills) != 0 {
		t.Fatalf("session B must not see session A's personal skill: %+v", bList.Skills)
	}
	// Setting B's prompt does not affect A.
	bResp, err := s.SessionSetUserPrompt(ctx, prototypes.AgentConfigSessionSetUserPromptRequest{
		Identity: sessB, AgentID: testAgentID, UserPrompt: "B-only",
	})
	if err != nil {
		t.Fatalf("set B: %v", err)
	}
	if bResp.Overlay.UserPrompt != "B-only" || len(bResp.Overlay.PersonalSkills) != 0 {
		t.Fatalf("session B overlay leaked from A: %+v", bResp.Overlay)
	}
}
