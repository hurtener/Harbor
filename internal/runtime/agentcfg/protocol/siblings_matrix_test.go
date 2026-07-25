package protocol_test

import (
	"context"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// TestVerbs_PreserveAllSiblingSections is the wave-end regression for the
// bidirectional section-merge invariant (audit W4): every single-section
// convenience verb must carry FORWARD every OTHER section of the active
// revision (set_revision is a whole-envelope replace, so a verb that forgets
// to copy a sibling silently WIPES it — real data loss). The existing
// per-verb PreservesSiblings tests only guarded Skills + ToolExposure; this
// seeds ALL FIVE sections and asserts each verb preserves the four it does
// not own — in particular Connections + LLMParams, the unguarded siblings.
func TestVerbs_PreserveAllSiblingSections(t *testing.T) {
	// A full payload pinning all six sections — the seed every verb must
	// preserve (minus the one it replaces). Hooks (D-280) is owned by NO verb
	// in this matrix (it rides set_revision only), so it must ALWAYS survive —
	// the exact D-283 regression this seed now guards.
	seed := func() prototypes.AgentConfigPayload {
		return prototypes.AgentConfigPayload{
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: strPtr("seed-base")},
			ToolExposure: &prototypes.AgentConfigToolExposure{PausedServers: []string{"seed-srv"}},
			Skills:       &prototypes.AgentConfigSkillsSelection{Names: []string{"seed-skill"}},
			// An http descriptor: the matrix asserts only that the Connections
			// SECTION survives each verb (by name), so the transport is
			// incidental — and http needs no stdio allowlist on the bare Service
			// most cases build. A stdio seed would have to carry an allowlisted
			// command, which is the add/set_revision RCE gate's concern, not this
			// test's.
			Connections: &prototypes.AgentConfigConnections{Servers: []prototypes.AgentConfigMCPConnectionDescriptor{
				{Name: "seed-conn", Transport: "http", URL: "https://seed-conn.invalid/rpc"},
			}},
			LLMParams: &prototypes.AgentConfigLLMParams{Temperature: f64(0.5)},
			Hooks: &prototypes.AgentConfigHooks{
				RunCompletion: &prototypes.AgentConfigRunCompletionHook{Tool: "seed-hook-tool", TimeoutMS: 5000},
			},
		}
	}

	cases := []struct {
		name string
		// run performs the verb's edit; ownsSection is the one section the
		// verb replaces (excluded from the preservation assertions).
		run        func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context)
		ownsPrompt bool
		ownsExpo   bool
		ownsSkills bool
		ownsLLM    bool
		// ownsConn excludes the Connections section from the survival
		// assertions — set for the connection verbs (add / remove) that OWN it.
		ownsConn bool
		// svcOpts, when non-nil, supplies verb-specific Service options
		// (e.g. the ConnectionAttacher the add verb needs); the skills store
		// is always wired alongside so the shared seed's skills survive.
		svcOpts func(t *testing.T) []agentcfgprotocol.Option
	}{
		{
			name:       "set_prompt_layers",
			ownsPrompt: true,
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
					Identity: scope(), AgentID: testAgentID,
					PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr("edited-base")},
				}); err != nil {
					t.Fatalf("set_prompt_layers: %v", err)
				}
			},
		},
		{
			name:     "set_tool_exposure",
			ownsExpo: true,
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
					Identity: scope(), AgentID: testAgentID,
					ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"edited-srv"}},
				}); err != nil {
					t.Fatalf("set_tool_exposure: %v", err)
				}
			},
		},
		{
			name:    "set_llm_params",
			ownsLLM: true,
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
					Identity: scope(), AgentID: testAgentID,
					LLMParams: prototypes.AgentConfigLLMParams{Temperature: f64(0.9)},
				}); err != nil {
					t.Fatalf("set_llm_params: %v", err)
				}
			},
		},
		{
			name:     "add_mcp_connection",
			ownsConn: true,
			// nil attach result = online; fakeAttacher is defined in
			// addconnection_test.go (same package). The add verb records a
			// revision that must carry Hooks (+ every other sibling) forward.
			svcOpts: func(_ *testing.T) []agentcfgprotocol.Option {
				return []agentcfgprotocol.Option{agentcfgprotocol.WithConnectionAttacher(&fakeAttacher{result: nil})}
			},
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
					Identity: scope(), AgentID: testAgentID,
					Connection: prototypes.AgentConfigMCPConnectionDescriptor{
						Name: "edited-conn", Transport: "http", URL: "https://example.invalid/edited",
					},
				}); err != nil {
					t.Fatalf("add_mcp_connection: %v", err)
				}
			},
		},
		{
			name:     "remove_mcp_connection",
			ownsConn: true,
			// Removes the seed's connection ("seed-conn"); its tool-exposure
			// residue prune targets only "seed-conn"-owned keys (none in the
			// seed), so ToolExposure — and every other sibling — survives.
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
					Identity: scope(), AgentID: testAgentID, Name: "seed-conn",
				}); err != nil {
					t.Fatalf("remove_mcp_connection: %v", err)
				}
			},
		},
		{
			name:       "skills.upsert",
			ownsSkills: true,
			run: func(t *testing.T, s *agentcfgprotocol.Service, ctx context.Context) {
				if _, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
					Identity: scope(), AgentID: testAgentID,
					Skill: prototypes.AgentConfigSkillInput{
						Name: "edited-skill", Trigger: "t", Steps: []string{"do the thing"},
						Origin: "generated", Scope: "project",
					},
				}); err != nil {
					t.Fatalf("skills.upsert: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The skills verb needs a SkillStore; a connection verb additionally
			// needs its ConnectionAttacher. Build the service with both when the
			// case supplies verb-specific options, else the shared withSkills svc.
			var s *agentcfgprotocol.Service
			if tc.svcOpts != nil {
				opts := append([]agentcfgprotocol.Option{agentcfgprotocol.WithSkillStore(newSkills(t))}, tc.svcOpts(t)...)
				var err error
				if s, err = agentcfgprotocol.NewService(newRegistry(t), opts...); err != nil {
					t.Fatalf("NewService: %v", err)
				}
			} else {
				s = svc(t, true) // withSkills — the skills verb needs a SkillStore
			}
			ctx := context.Background()
			if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
				Identity: scope(), AgentID: testAgentID, Payload: seed(),
			}); err != nil {
				t.Fatalf("seed full payload: %v", err)
			}
			tc.run(t, s, ctx)

			get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if err != nil || !get.Set || get.Revision == nil {
				t.Fatalf("get: %+v err=%v", get, err)
			}
			p := get.Revision.Payload
			// Every section the verb does NOT own must survive at its seed value.
			if !tc.ownsPrompt {
				if p.PromptLayers == nil || p.PromptLayers.Base == nil || *p.PromptLayers.Base != "seed-base" {
					t.Errorf("%s wiped the prompt-layer sibling: %+v", tc.name, p.PromptLayers)
				}
			}
			if !tc.ownsExpo {
				if p.ToolExposure == nil || len(p.ToolExposure.PausedServers) != 1 || p.ToolExposure.PausedServers[0] != "seed-srv" {
					t.Errorf("%s wiped the tool-exposure sibling: %+v", tc.name, p.ToolExposure)
				}
			}
			if !tc.ownsSkills {
				if p.Skills == nil || !containsStr(p.Skills.Names, "seed-skill") {
					t.Errorf("%s wiped the skills sibling: %+v", tc.name, p.Skills)
				}
			}
			if !tc.ownsLLM {
				if p.LLMParams == nil || p.LLMParams.Temperature == nil || *p.LLMParams.Temperature != 0.5 {
					t.Errorf("%s wiped the llm-params sibling: %+v", tc.name, p.LLMParams)
				}
			}
			// Connections survives byte-identical for every verb that does NOT
			// own it (the add / remove connection verbs own it and are excluded).
			if !tc.ownsConn {
				if p.Connections == nil || len(p.Connections.Servers) != 1 || p.Connections.Servers[0].Name != "seed-conn" {
					t.Errorf("%s wiped the connections sibling: %+v", tc.name, p.Connections)
				}
			}
			// Hooks is likewise owned by NO verb here — it must ALWAYS survive
			// (the D-283 regression: none of the five setters carried Hooks
			// forward until this phase).
			if p.Hooks == nil || p.Hooks.RunCompletion == nil || p.Hooks.RunCompletion.Tool != "seed-hook-tool" {
				t.Errorf("%s wiped the hooks sibling: %+v", tc.name, p.Hooks)
			}
		})
	}
}

// TestTierMatrix_UserTierIsADistinctChain extends the tier matrix with the
// user tier: a user-scope write lands on the caller's OWN durable chain and
// leaves the admin/agent chain's sections entirely intact (the distinct
// ConfigScopeUser key space). It pins the three-tier separation — admin chain,
// user variant, and (elsewhere) the ephemeral session overlay are independent.
func TestTierMatrix_UserTierIsADistinctChain(t *testing.T) {
	s := svc(t, false)
	ctx := context.Background()
	// Seed a full agent-level (admin) revision.
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: strPtr("operator-base")},
			ToolExposure: &prototypes.AgentConfigToolExposure{PausedServers: []string{"admin-srv"}},
			Skills:       &prototypes.AgentConfigSkillsSelection{Names: []string{"admin-skill"}},
		},
	}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	// A user writes their own variant.
	if _, err := s.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "alice", Session: "s"}, AgentID: testAgentID,
		Payload: prototypes.AgentConfigUserPayload{UserPrompt: "alice-only", DisabledServers: []string{"admin-srv"}},
	}); err != nil {
		t.Fatalf("user set: %v", err)
	}
	// The agent chain's active is UNCHANGED — operator base, paused server, and
	// admin skill all intact (the user write never touched the agent chain).
	get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !get.Set || get.Revision == nil {
		t.Fatalf("admin get: set=%v err=%v", get.Set, err)
	}
	p := get.Revision.Payload
	if p.PromptLayers == nil || p.PromptLayers.Base == nil || *p.PromptLayers.Base != "operator-base" {
		t.Errorf("user write disturbed the operator base: %+v", p.PromptLayers)
	}
	if p.PromptLayers.User != nil {
		t.Errorf("user write leaked the user layer onto the agent chain: %+v", p.PromptLayers)
	}
	if p.ToolExposure == nil || len(p.ToolExposure.PausedServers) != 1 || p.ToolExposure.PausedServers[0] != "admin-srv" {
		t.Errorf("user write disturbed the agent tool-exposure: %+v", p.ToolExposure)
	}
	if p.Skills == nil || !containsStr(p.Skills.Names, "admin-skill") {
		t.Errorf("user write disturbed the agent skills: %+v", p.Skills)
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
