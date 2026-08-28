package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func userScope(user string) prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "t", User: user, Session: "s"}
}

// TestUserVerbs_RoundTrip exercises the user-tier consumer end-to-end:
// set → list → diff → rollback over a caller's OWN durable variant.
func TestUserVerbs_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	id := userScope("alice")

	r1, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: id, AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "be terse", DisabledServers: []string{"weather"}},
	})
	if err != nil {
		t.Fatalf("set1: %v", err)
	}
	r2, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: id, AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "be terse and kind", DisabledServers: []string{"weather"}},
	})
	if err != nil {
		t.Fatalf("set2: %v", err)
	}

	// get returns the active (latest) revision.
	get, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: id, AgentID: testAgentID})
	if err != nil || !get.Set || get.Revision == nil {
		t.Fatalf("get: set=%v err=%v", get.Set, err)
	}
	if get.Revision.RevisionID != r2.Revision.RevisionID {
		t.Fatalf("get active=%q want %q", get.Revision.RevisionID, r2.Revision.RevisionID)
	}
	// The bounded payload mapped onto the user prompt layer (never the base).
	if get.Revision.Payload.PromptLayers == nil || get.Revision.Payload.PromptLayers.User == nil {
		t.Fatalf("user prompt not projected: %+v", get.Revision.Payload.PromptLayers)
	}
	if get.Revision.Payload.PromptLayers.Base != nil {
		t.Fatalf("user payload must never set the operator base")
	}

	// list newest-first.
	list, err := s.UserListRevisions(ctx, prototypes.AgentConfigUserListRevisionsRequest{Identity: id, AgentID: testAgentID})
	if err != nil || len(list.Revisions) != 2 {
		t.Fatalf("list: n=%d err=%v", len(list.Revisions), err)
	}
	if list.Revisions[0].RevisionID != r2.Revision.RevisionID {
		t.Fatalf("list not newest-first")
	}

	// diff surfaces the user-layer text delta.
	d, err := s.UserDiff(ctx, prototypes.AgentConfigUserDiffRequest{
		Identity: id, AgentID: testAgentID,
		FromRevision: r1.Revision.RevisionID, ToRevision: r2.Revision.RevisionID,
	})
	if err != nil || !d.Diff.PromptLayers.UserChanged {
		t.Fatalf("diff: changed=%v err=%v", d.Diff.PromptLayers.UserChanged, err)
	}

	// rollback repoints to r1 without mutating a revision.
	rb, err := s.UserRollback(ctx, prototypes.AgentConfigUserRollbackRequest{
		Identity: id, AgentID: testAgentID, RevisionID: r1.Revision.RevisionID,
	})
	if err != nil || rb.Revision.RevisionID != r1.Revision.RevisionID {
		t.Fatalf("rollback: %v -> %q", err, rb.Revision.RevisionID)
	}
	getAfter, _ := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: id, AgentID: testAgentID})
	if getAfter.Revision == nil || getAfter.Revision.RevisionID != r1.Revision.RevisionID {
		t.Fatalf("active after rollback not r1")
	}
}

// TestUserVerbs_WideningHasNoPath proves the bounded payload reaches no
// widening path: the restricted mapping never populates Base, Connections, or
// LLMParams, regardless of what the caller sends (the wire type carries no
// such fields, and the mapping is the structural boundary).
func TestUserVerbs_WideningHasNoPath(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	id := userScope("alice")
	r, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: id, AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{
			UserPrompt:         "narrow me",
			DisabledServers:    []string{"weather"},
			DisabledTools:      []string{"weather_now"},
			ServerLoadingModes: map[string]string{"weather": "deferred"},
			ToolLoadingModes:   map[string]string{"weather_now": "always"},
			PersonalSkills:     []string{"sk1"},
		},
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	p := r.Revision.Payload
	if p.Connections != nil {
		t.Errorf("user payload reached the connections section")
	}
	if p.LLMParams != nil {
		t.Errorf("user payload reached the LLM-params section")
	}
	if p.PromptLayers != nil && p.PromptLayers.Base != nil {
		t.Errorf("user payload reached the operator base prompt")
	}
	// The narrow-only disables landed on the tool-exposure section.
	if p.ToolExposure == nil || len(p.ToolExposure.PausedServers) != 1 {
		t.Errorf("disabled servers not projected onto tool exposure: %+v", p.ToolExposure)
	}
	if p.ToolExposure == nil || p.ToolExposure.ServerLoadingModes["weather"] != "deferred" || p.ToolExposure.ToolLoadingModes["weather_now"] != "always" {
		t.Errorf("user loading-mode choices not projected onto tool exposure: %+v", p.ToolExposure)
	}
	if p.Skills == nil || len(p.Skills.Names) != 1 {
		t.Errorf("personal skills not projected onto skills membership: %+v", p.Skills)
	}
}

// TestUserVerbs_InvalidLoadingModes_FailBeforeRevision proves the user-tier
// wire payload accepts only the same closed loading-mode values as the admin
// tool-exposure door and never persists a rejected choice.
func TestUserVerbs_InvalidLoadingModes_FailBeforeRevision(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	id := userScope("alice")
	for name, payload := range map[string]prototypes.AgentConfigUserPayload{
		"unknown server mode": {ServerLoadingModes: map[string]string{"weather": "sometimes"}},
		"unknown tool mode":   {ToolLoadingModes: map[string]string{"weather_now": "sometimes"}},
		"empty server key":    {ServerLoadingModes: map[string]string{"": "always"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{Identity: id, AgentID: testAgentID, Payload: payload}); err == nil {
				t.Fatal("invalid loading mode was accepted")
			} else if !errors.Is(err, agentcfgprotocol.ErrInvalidToolExposureLoading) {
				t.Fatalf("error = %v, want ErrInvalidToolExposureLoading", err)
			}
			got, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: id, AgentID: testAgentID})
			if err != nil {
				t.Fatalf("get after rejected write: %v", err)
			}
			if got.Set {
				t.Fatal("rejected user loading mode created a revision")
			}
		})
	}
}

// TestUserVerbs_EmptyPayload covers the all-nil arms of userPayloadToDomain:
// an empty bounded payload maps to a payload with no sections set.
func TestUserVerbs_EmptyPayload(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	r, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: userScope("alice"), AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{},
	})
	if err != nil {
		t.Fatalf("empty set: %v", err)
	}
	p := r.Revision.Payload
	if p.PromptLayers != nil || p.ToolExposure != nil || p.Skills != nil || p.Connections != nil || p.LLMParams != nil {
		t.Errorf("empty user payload should map to an all-nil envelope: %+v", p)
	}
}

// TestUserVerbs_CrossUserInvisible proves user B cannot see or roll back user
// A's variant through the Service (the registry keys by the real user).
func TestUserVerbs_CrossUserInvisible(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	aRev, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: userScope("alice"), AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "alice"},
	})
	if err != nil {
		t.Fatalf("alice set: %v", err)
	}
	bGet, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: userScope("bob"), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("bob get: %v", err)
	}
	if bGet.Set {
		t.Fatalf("bob sees alice's variant")
	}
	if _, err := s.UserRollback(ctx, prototypes.AgentConfigUserRollbackRequest{
		Identity: userScope("bob"), AgentID: testAgentID, RevisionID: aRev.Revision.RevisionID,
	}); err == nil {
		t.Fatalf("bob rolled back alice's revision")
	}
}

// TestUserVerbs_IdentityRequired proves an incomplete triple fails closed.
func TestUserVerbs_IdentityRequired(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	if _, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: prototypes.IdentityScope{Tenant: "t"}, AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "x"},
	}); err == nil {
		t.Fatalf("incomplete identity should fail closed")
	}
}

// TestUserVerbs_ContextCancelled proves every user verb honours a cancelled
// context and fails fast (the ctx.Err early-return on each verb).
func TestUserVerbs_ContextCancelled(t *testing.T) {
	s := svc(t, false)
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := userScope("alice")
	if _, err := s.UserGet(cctx, prototypes.AgentConfigUserGetRequest{Identity: id, AgentID: testAgentID}); err == nil {
		t.Error("UserGet ignored a cancelled ctx")
	}
	if _, err := s.UserSetRevision(cctx, prototypes.AgentConfigUserSetRevisionRequest{Identity: id, AgentID: testAgentID}); err == nil {
		t.Error("UserSetRevision ignored a cancelled ctx")
	}
	if _, err := s.UserListRevisions(cctx, prototypes.AgentConfigUserListRevisionsRequest{Identity: id, AgentID: testAgentID}); err == nil {
		t.Error("UserListRevisions ignored a cancelled ctx")
	}
	if _, err := s.UserDiff(cctx, prototypes.AgentConfigUserDiffRequest{Identity: id, AgentID: testAgentID, FromRevision: "a", ToRevision: "b"}); err == nil {
		t.Error("UserDiff ignored a cancelled ctx")
	}
	if _, err := s.UserRollback(cctx, prototypes.AgentConfigUserRollbackRequest{Identity: id, AgentID: testAgentID, RevisionID: "a"}); err == nil {
		t.Error("UserRollback ignored a cancelled ctx")
	}
}

// TestUserGet_EmptyAndIdentityRequired covers the empty-variant get plus the
// identity-required guard on the read verbs.
func TestUserGet_EmptyAndIdentityRequired(t *testing.T) {
	s := svc(t, false)
	ctx := context.Background()
	// No variant yet → Set:false.
	get, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: userScope("nobody"), AgentID: testAgentID})
	if err != nil || get.Set {
		t.Fatalf("empty user get: set=%v err=%v", get.Set, err)
	}
	// Incomplete identity on each read verb fails closed.
	bad := prototypes.IdentityScope{Tenant: "t"}
	if _, err := s.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: bad, AgentID: testAgentID}); err == nil {
		t.Error("UserGet accepted an incomplete triple")
	}
	if _, err := s.UserListRevisions(ctx, prototypes.AgentConfigUserListRevisionsRequest{Identity: bad, AgentID: testAgentID}); err == nil {
		t.Error("UserListRevisions accepted an incomplete triple")
	}
	if _, err := s.UserDiff(ctx, prototypes.AgentConfigUserDiffRequest{Identity: bad, AgentID: testAgentID}); err == nil {
		t.Error("UserDiff accepted an incomplete triple")
	}
	if _, err := s.UserRollback(ctx, prototypes.AgentConfigUserRollbackRequest{Identity: bad, AgentID: testAgentID, RevisionID: "x"}); err == nil {
		t.Error("UserRollback accepted an incomplete triple")
	}
}

// TestUserVerbs_ScopeAwareLock proves a user-scope write and an agent-scope
// write to the same agent are independent and the user write never disturbs
// the agent chain (the scope-aware lock + the distinct key space).
func TestUserVerbs_ScopeAwareLock(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// Seed an agent-level revision directly.
	adminID := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "admin", SessionID: "s"}}
	admin := agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"admin"}}}
	if _, err := reg.SetRevision(ctx, adminID, testAgentID, agentcfg.ConfigScopeAgent, admin, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: userScope("alice"), AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "alice"},
	}); err != nil {
		t.Fatalf("user set: %v", err)
	}
	// The agent-level active is still the admin revision.
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !got.Set {
		t.Fatalf("agent get: set=%v err=%v", got.Set, err)
	}
	if got.Revision.Payload.Skills == nil || len(got.Revision.Payload.Skills.Names) != 1 || got.Revision.Payload.Skills.Names[0] != "admin" {
		t.Fatalf("agent chain disturbed by user write: %+v", got.Revision.Payload.Skills)
	}
}
