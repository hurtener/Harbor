package protocol_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// Phase 221 (D-366) — the expected-revision token at the Protocol service.
//
// The driver's own tests pin the precondition's semantics. These tests pin the
// thing only this layer can be asked about: that all SIXTEEN spine-writing
// doors actually THREAD the caller's token down to the registry, rather than
// merely declaring the field on their request types.
//
// A door that declared the field but dropped it on the floor would pass every
// grep-for-presence guard, every reflection check, and the whole driver test
// suite. It fails here, because here the door is DRIVEN with a stale token and
// must be refused.

// condQuad is the domain identity matching scope() — used to seed base
// revisions directly through the registry.
func condQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

// staleTokenFor seeds TWO revisions on the given scope and returns the content
// hash of the FIRST — a token whose base has since moved. Every door in the
// table below is then driven with it and must refuse.
func staleTokenFor(t *testing.T, reg agentcfg.Registry, scp agentcfg.ConfigScope) string {
	t.Helper()
	ctx := context.Background()
	first, err := reg.SetRevision(ctx, condQuad(), testAgentID, scp,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"seed-one"}}},
		agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed first revision: %v", err)
	}
	if _, err := reg.SetRevision(ctx, condQuad(), testAgentID, scp,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"seed-one", "seed-two"}}},
		agentcfg.SetOptions{}); err != nil {
		t.Fatalf("move the base: %v", err)
	}
	return first.ContentHash
}

// TestConditionalWrite_AllSixteenDoorsAcceptTheToken drives every one of the
// sixteen spine-writing doors with a STALE token and asserts each is refused
// with agentcfg.ErrRevisionConflict.
//
// The count is asserted, not documented: a seventeenth spine writer added by a
// later phase without threading the field has no entry here, and the length
// assertion below fails.
func TestConditionalWrite_AllSixteenDoorsAcceptTheToken(t *testing.T) {
	ctx := context.Background()

	type door struct {
		method string
		// drive seeds its own fixture and invokes the door with a stale token.
		drive func(t *testing.T) error
	}

	doors := []door{
		// ---------------- Agent scope — twelve ----------------
		{"agent_config.set_revision", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			_, err = s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
				Identity: scope(), AgentID: testAgentID,
				Payload:             prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"new"}}},
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.rollback", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			revs, lerr := reg.ListRevisions(ctx, condQuad(), testAgentID, agentcfg.ConfigScopeAgent, 0)
			if lerr != nil || len(revs) < 2 {
				t.Fatalf("list: %v (%d revisions)", lerr, len(revs))
			}
			_, err = s.Rollback(ctx, prototypes.AgentConfigRollbackRequest{
				Identity: scope(), AgentID: testAgentID,
				RevisionID:          revs[len(revs)-1].RevisionID,
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.skills.upsert", func(t *testing.T) error {
			s, reg := svcWithReg(t)
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			_, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
				Identity: scope(), AgentID: testAgentID,
				Skill:               condSkillInput("cond-skill"),
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.skills.delete", func(t *testing.T) error {
			s, reg := svcWithReg(t)
			if _, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
				Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("cond-skill"),
			}); err != nil {
				t.Fatalf("seed skill: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			_, err := s.SkillsDelete(ctx, prototypes.AgentConfigSkillsDeleteRequest{
				Identity: scope(), AgentID: testAgentID, Name: "cond-skill",
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.set_tool_exposure", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			_, err = s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
				Identity: scope(), AgentID: testAgentID,
				ToolExposure:        prototypes.AgentConfigToolExposure{PausedServers: []string{"srv"}},
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.set_prompt_layers", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			base := "a new base"
			_, err = s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
				Identity: scope(), AgentID: testAgentID,
				PromptLayers:        prototypes.AgentConfigPromptLayers{Base: &base},
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.set_llm_params", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			temp := 0.5
			_, err = s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
				Identity: scope(), AgentID: testAgentID,
				LLMParams:           prototypes.AgentConfigLLMParams{Temperature: &temp},
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.add_mcp_connection", func(t *testing.T) error {
			h := newAddHarness(t, nil)
			tok := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)
			req := addReq(stdioConn(), nil)
			req.ExpectedContentHash = tok
			_, err := h.svc.AddMCPConnection(ctx, req)
			return err
		}},
		{"agent_config.remove_mcp_connection", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			seedConnRevision(t, ctx, reg, "srv")
			// Move the base AFTER the connection seed so the token is stale but
			// the connection is still present (the door must refuse on the
			// token, not on a missing target).
			tok := activeHashThenMove(t, reg, "agent-remove", agentcfg.ConfigScopeAgent)
			req := removeReq("srv")
			req.ExpectedContentHash = tok
			_, err = s.RemoveMCPConnection(ctx, req)
			return err
		}},
		{"agent_config.set_mcp_discovery_origins", func(t *testing.T) error {
			reg := newRegistry(t)
			applier := newFakeApplier()
			s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithDiscoveryOriginApplier(applier))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			seedDiscoveryRev(t, ctx, reg, "agent-d", agentcfg.MCPConnectionDescriptor{
				Name: "srv", Transport: agentcfg.MCPTransportHTTP, URL: "https://x.invalid/rpc"})
			tok := activeHashThenMove(t, reg, "agent-d", agentcfg.ConfigScopeAgent)
			req := discReq("agent-d", "srv", []string{"https://as.example.net"})
			req.ExpectedContentHash = tok
			_, err = s.SetMCPDiscoveryOrigins(ctx, req)
			return err
		}},
		{"agent_config.set_oauth_provider", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg,
				agentcfgprotocol.WithProviderInstaller(newFakeInstaller()))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
			_, err = s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
				Identity: scope(), AgentID: testAgentID,
				Provider:            okProvider("cond-prov"),
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.remove_oauth_provider", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg,
				agentcfgprotocol.WithProviderInstaller(newFakeInstaller()))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if _, err := s.SetOAuthProvider(ctx, prototypes.AgentConfigSetOAuthProviderRequest{
				Identity: scope(), AgentID: testAgentID, Provider: okProvider("cond-prov"),
			}); err != nil {
				t.Fatalf("seed provider: %v", err)
			}
			tok := activeHashThenMove(t, reg, testAgentID, agentcfg.ConfigScopeAgent)
			_, err = s.RemoveOAuthProvider(ctx, prototypes.AgentConfigRemoveOAuthProviderRequest{
				Identity: scope(), AgentID: testAgentID, Name: "cond-prov",
				ExpectedContentHash: tok,
			})
			return err
		}},

		// ---------------- User scope — four ----------------
		{"agent_config.user.set_revision", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeUser)
			_, err = s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
				Identity: scope(), AgentID: testAgentID,
				Payload:             prototypes.AgentConfigUserPayload{UserPrompt: "a new user layer"},
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.user.rollback", func(t *testing.T) error {
			reg := newRegistry(t)
			s, err := agentcfgprotocol.NewService(reg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeUser)
			revs, lerr := reg.ListRevisions(ctx, condQuad(), testAgentID, agentcfg.ConfigScopeUser, 0)
			if lerr != nil || len(revs) < 2 {
				t.Fatalf("list: %v (%d revisions)", lerr, len(revs))
			}
			_, err = s.UserRollback(ctx, prototypes.AgentConfigUserRollbackRequest{
				Identity: scope(), AgentID: testAgentID,
				RevisionID:          revs[len(revs)-1].RevisionID,
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.user.skills.upsert", func(t *testing.T) error {
			s, reg := svcWithReg(t)
			tok := staleTokenFor(t, reg, agentcfg.ConfigScopeUser)
			_, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{
				Identity: scope(), AgentID: testAgentID,
				Skill:               condSkillInput("cond-user-skill"),
				ExpectedContentHash: tok,
			})
			return err
		}},
		{"agent_config.user.skills.delete", func(t *testing.T) error {
			s, reg := svcWithReg(t)
			if _, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{
				Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("cond-user-skill"),
			}); err != nil {
				t.Fatalf("seed user skill: %v", err)
			}
			tok := activeHashThenMove(t, reg, testAgentID, agentcfg.ConfigScopeUser)
			_, err := s.UserSkillsDelete(ctx, prototypes.AgentConfigUserSkillsDeleteRequest{
				Identity: scope(), AgentID: testAgentID, Name: "cond-user-skill",
				ExpectedContentHash: tok,
			})
			return err
		}},
	}

	if len(doors) != 16 {
		t.Fatalf("the table covers %d doors, want 16 — a spine writer was added or removed "+
			"without threading the expected-revision token", len(doors))
	}

	for _, d := range doors {
		t.Run(d.method, func(t *testing.T) {
			err := d.drive(t)
			if !errors.Is(err, agentcfg.ErrRevisionConflict) {
				t.Fatalf("%s with a STALE expected_content_hash returned %v, want ErrRevisionConflict — "+
					"the door declares the field but does not thread it to the registry", d.method, err)
			}
		})
	}
}

// TestConditionalWrite_SixteenRequestTypesDeclareTheField is the structural
// twin of the table above: the sixteen wire request types each carry
// `expected_content_hash` as an optional string. Reflection over the real
// types, so a rename or a dropped json tag fails here rather than silently
// making the field unreachable from the wire.
func TestConditionalWrite_SixteenRequestTypesDeclareTheField(t *testing.T) {
	types := []any{
		prototypes.AgentConfigSetRevisionRequest{},
		prototypes.AgentConfigRollbackRequest{},
		prototypes.AgentConfigSkillsUpsertRequest{},
		prototypes.AgentConfigSkillsDeleteRequest{},
		prototypes.AgentConfigSetToolExposureRequest{},
		prototypes.AgentConfigSetPromptLayersRequest{},
		prototypes.AgentConfigSetLLMParamsRequest{},
		prototypes.AgentConfigAddMCPConnectionRequest{},
		prototypes.AgentConfigRemoveMCPConnectionRequest{},
		prototypes.AgentConfigSetMCPDiscoveryOriginsRequest{},
		prototypes.AgentConfigSetOAuthProviderRequest{},
		prototypes.AgentConfigRemoveOAuthProviderRequest{},
		prototypes.AgentConfigUserSetRevisionRequest{},
		prototypes.AgentConfigUserRollbackRequest{},
		prototypes.AgentConfigUserSkillsUpsertRequest{},
		prototypes.AgentConfigUserSkillsDeleteRequest{},
	}
	if len(types) != 16 {
		t.Fatalf("the type list covers %d types, want 16", len(types))
	}
	for _, v := range types {
		rt := reflect.TypeOf(v)
		f, ok := rt.FieldByName("ExpectedContentHash")
		if !ok {
			t.Errorf("%s has no ExpectedContentHash field", rt.Name())
			continue
		}
		if f.Type.Kind() != reflect.String {
			t.Errorf("%s.ExpectedContentHash is %s, want string", rt.Name(), f.Type.Kind())
		}
		if got := f.Tag.Get("json"); got != "expected_content_hash,omitempty" {
			t.Errorf("%s.ExpectedContentHash json tag = %q, want %q",
				rt.Name(), got, "expected_content_hash,omitempty")
		}
	}
}

// TestConditionalWrite_TokenIsPreconditionNotAuthority — the token can only
// ever cause a write to be REFUSED. No value of it widens what a caller may
// write, and it never rescues a request the identity gate would reject.
func TestConditionalWrite_TokenIsPreconditionNotAuthority(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	s, err := agentcfgprotocol.NewService(reg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	valid := staleTokenFor(t, reg, agentcfg.ConfigScopeAgent)
	// Make the token VALID for the current base so it cannot be the reason for
	// any refusal below.
	active, ok, err := reg.Active(ctx, condQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("Active: ok=%v err=%v", ok, err)
	}
	if active.ContentHash == valid {
		t.Fatal("fixture broken: the base did not move")
	}
	validNow := active.ContentHash

	t.Run("IncompleteIdentityStillRefusedByTheIdentityGate", func(t *testing.T) {
		_, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
			// Missing session — the identity triple is incomplete.
			Identity: prototypes.IdentityScope{Tenant: "t", User: "u"},
			AgentID:  testAgentID,
			Payload:  prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"x"}}},
			// A perfectly VALID token. It must not rescue the request.
			ExpectedContentHash: validNow,
		})
		if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
			t.Fatalf("got %v, want ErrIdentityRequired — a valid token must never satisfy the identity gate", err)
		}
	})

	t.Run("EmptyAgentIDStillRefused", func(t *testing.T) {
		_, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
			Identity: scope(), AgentID: "",
			Payload:             prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"x"}}},
			ExpectedContentHash: validNow,
		})
		if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
			t.Fatalf("got %v, want ErrIdentityRequired", err)
		}
	})

	t.Run("PayloadValidationStillRunsBeforeTheWrite", func(t *testing.T) {
		// An out-of-range sampling value is refused by the validator, not
		// converted into a conflict or a success by the token.
		bad := 99.0
		_, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
			Identity: scope(), AgentID: testAgentID,
			LLMParams:           prototypes.AgentConfigLLMParams{Temperature: &bad},
			ExpectedContentHash: validNow,
		})
		if err == nil {
			t.Fatal("an invalid llm-params payload was accepted")
		}
		if errors.Is(err, agentcfg.ErrRevisionConflict) {
			t.Fatalf("a validation failure was reported as a revision conflict: %v", err)
		}
		if !errors.Is(err, agentcfgprotocol.ErrInvalidLLMParams) {
			t.Fatalf("got %v, want ErrInvalidLLMParams", err)
		}
	})

	t.Run("AValidTokenDoesNotWidenTheWrite", func(t *testing.T) {
		// The token is not a capability: with it, the caller writes exactly
		// what it could have written without it.
		resp, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
			Identity: scope(), AgentID: testAgentID,
			Payload:             prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"allowed"}}},
			ExpectedContentHash: validNow,
		})
		if err != nil {
			t.Fatalf("valid token must proceed: %v", err)
		}
		if resp.Revision.RevisionID == "" {
			t.Fatal("no revision minted")
		}
	})
}

// activeHashThenMove returns the CURRENT active content hash and then writes a
// further revision, so the returned token is stale by exactly one write. Used
// by doors whose fixture must be seeded before the base moves.
func activeHashThenMove(t *testing.T, reg agentcfg.Registry, agentID string, scp agentcfg.ConfigScope) string {
	t.Helper()
	ctx := context.Background()
	active, ok, err := reg.Active(ctx, condQuad(), agentID, scp)
	if err != nil || !ok {
		t.Fatalf("Active(%s): ok=%v err=%v", agentID, ok, err)
	}
	cur := active.Payload
	// A sibling-preserving write that is guaranteed to change the content.
	names := append([]string(nil), cur.SkillNames()...)
	cur.Skills = &agentcfg.SkillsSelection{Names: append(names, "base-mover")}
	// The base must move on the SAME agent the token was read from — writing
	// to a different agent leaves the token valid and the door then correctly
	// accepts the write, which would read as "the door does not thread the
	// token" and is really a broken fixture.
	if _, err := reg.SetRevision(ctx, condQuad(), agentID, scp, cur, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("move the base: %v", err)
	}
	return active.ContentHash
}

// svcWithReg builds a skills-wired Service over a registry the caller keeps a
// handle on, so a test can seed the spine directly AND drive the doors. The
// package's svc() helper constructs its registry internally, which the
// conditional tests cannot use — they must move the base out from under a
// token before invoking the door.
func svcWithReg(t *testing.T) (*agentcfgprotocol.Service, agentcfg.Registry) {
	t.Helper()
	reg := newRegistry(t)
	s, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSkillStore(newSkills(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s, reg
}

// condSkillInput is a minimal valid skill body for the membership doors.
func condSkillInput(name string) prototypes.AgentConfigSkillInput {
	return prototypes.AgentConfigSkillInput{
		Name:        name,
		Title:       strings.ToUpper(name),
		Description: "a conditional-write fixture skill",
		Trigger:     "when the fixture asks",
		Steps:       []string{"do the thing"},
		Origin:      "generated",
		Scope:       "session",
	}
}
