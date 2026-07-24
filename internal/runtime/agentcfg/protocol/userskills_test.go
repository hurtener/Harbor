package protocol_test

import (
	"context"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestUserSkills_DurableAcrossSessions proves the durable-per-user skills verbs
// (D-345): a skill authored under session A is visible from session B of the
// SAME (tenant, user) — durable-by-default, conversation-independent — and its
// scope is forced to user regardless of the requested body scope.
func TestUserSkills_DurableAcrossSessions(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	sessA := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionA"}
	sessB := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "sessionB"}

	up, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{
		Identity: sessA, AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{
			Name: "durable-a", Trigger: "when asked", Steps: []string{"do it"},
			Origin: "generated", Scope: "tenant", // requested scope ignored — forced to user
		},
	})
	if err != nil {
		t.Fatalf("user upsert: %v", err)
	}
	if up.Skill.Scope != "user" {
		t.Fatalf("scope not forced to user: %q", up.Skill.Scope)
	}
	if up.Revision.RevisionID == "" {
		t.Fatalf("upsert did not record a durable membership revision: %+v", up.Revision)
	}

	// Visible from a DIFFERENT session of the same (tenant, user).
	bList, err := s.UserSkillsList(ctx, prototypes.AgentConfigUserSkillsListRequest{Identity: sessB, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list from session B: %v", err)
	}
	if len(bList.Skills) != 1 || bList.Skills[0].Name != "durable-a" {
		t.Fatalf("durable skill not visible cross-session: %+v", bList.Skills)
	}

	// Delete from session B removes it for the whole (tenant, user).
	if _, err := s.UserSkillsDelete(ctx, prototypes.AgentConfigUserSkillsDeleteRequest{
		Identity: sessB, AgentID: testAgentID, Name: "durable-a",
	}); err != nil {
		t.Fatalf("delete from session B: %v", err)
	}
	aList, err := s.UserSkillsList(ctx, prototypes.AgentConfigUserSkillsListRequest{Identity: sessA, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list from session A after delete: %v", err)
	}
	if len(aList.Skills) != 0 {
		t.Fatalf("durable skill survived cross-session delete: %+v", aList.Skills)
	}
}

// TestUserSkills_CrossUserIsolation proves a durable user skill never leaks to
// a different user (same tenant) — the (tenant, user) isolation principal holds
// even though the verbs are claim-free.
func TestUserSkills_CrossUserIsolation(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	userU := prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s1"}
	userV := prototypes.IdentityScope{Tenant: "t", User: "v", Session: "s1"}

	if _, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{
		Identity: userU, AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{
			Name: "u-private", Trigger: "t", Steps: []string{"s"}, Origin: "generated", Scope: "user",
		},
	}); err != nil {
		t.Fatalf("upsert for user u: %v", err)
	}
	vList, err := s.UserSkillsList(ctx, prototypes.AgentConfigUserSkillsListRequest{Identity: userV, AgentID: testAgentID})
	if err != nil {
		t.Fatalf("list for user v: %v", err)
	}
	if len(vList.Skills) != 0 {
		t.Fatalf("user v must not see user u's durable skill: %+v", vList.Skills)
	}
}

// TestUserSkills_MissingIdentity_FailsLoud proves the identity gate holds on
// the claim-free verbs: an incomplete triple is rejected, never silently
// accepted.
func TestUserSkills_MissingIdentity_FailsLoud(t *testing.T) {
	ctx := context.Background()
	s := sessionSvc(t)
	_, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "", Session: "s"}, AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "x", Trigger: "t", Steps: []string{"s"}, Origin: "generated", Scope: "user"},
	})
	if err == nil {
		t.Fatalf("expected identity-required error on incomplete triple, got nil")
	}
}
