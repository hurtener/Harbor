package protocol_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// rebuild_completeness_test.go is the D-283 mechanical guard: every
// section-scoped agent-config setter rebuilds `agentcfg.ConfigPayload` by
// hand, carrying forward the sibling sections it knew about when it was
// written. The `Hooks` section landed (D-280) after all five setters, and
// none of them gained the carry-forward — a silent-degradation bug (CLAUDE.md
// §13) this phase fixes. This test closes the OMISSION CLASS, not just
// today's instance: `rcSeed` populates EVERY `ConfigPayload` section and
// reflection-asserts it covers every struct field (so a future 7th section
// fails the SEED first, naming the field), and `rcAssertSiblingsSurvive`
// walks the struct BY REFLECTION (not a hand-enumerated list) so a future
// section a setter fails to carry forward is caught automatically too.

const rcAgent = "agent-rebuild-completeness"

// rcQuad is the fixed identity quadruple every subtest seeds + reads under
// (matching scope()'s tenant/user/session — the registry key the Service
// derives from the wire IdentityScope).
func rcQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

// rcSeed builds an agentcfg.ConfigPayload with EVERY section populated to a
// distinct, comparable, non-zero value. It reflection-asserts every field is
// non-nil before returning (rcAssertSeedComplete) — the forcing function: a
// future section added to ConfigPayload without a matching seed entry fails
// HERE, not silently, before any setter is even invoked.
func rcSeed(t *testing.T) agentcfg.ConfigPayload {
	t.Helper()
	seed := agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: strPtr("seed-base"), User: strPtr("seed-user")},
		ToolExposure: &agentcfg.ToolExposure{
			PausedServers:      []string{"seed-paused-srv"},
			DisabledTools:      []string{"seed-disabled-tool"},
			ServerLoadingModes: map[string]string{"seed-srv": "deferred"},
			ToolLoadingModes:   map[string]string{"seed-tool": "always"},
		},
		Skills: &agentcfg.SkillsSelection{Names: []string{"seed-skill"}},
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
			{Name: "seed-conn", Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/seed"},
		}},
		LLMParams: &agentcfg.LLMParams{
			Model:           strPtr("seed-model"),
			Temperature:     f64(0.42),
			MaxTokens:       intp(4096),
			ReasoningEffort: strPtr("high"),
		},
		Hooks: &agentcfg.HooksSection{
			RunCompletion: &agentcfg.RunCompletionHook{Tool: "seed-hook-tool", TimeoutMS: 9000},
		},
		Naming: &agentcfg.NamingSection{
			Auto: true, AfterTurns: 2, RepeatEvery: 3, MaxRepetitions: 5, MaxTitleLen: 100, Model: "seed-model",
		},
	}
	rcAssertSeedComplete(t, seed)
	return seed
}

// rcAssertSeedComplete reflection-walks every top-level field of
// agentcfg.ConfigPayload and fails loud — NAMING the field — if the seed
// left it nil. Every section today is an optional pointer (the
// forward-compatible envelope, agentcfg.go's ConfigPayload doc); a future
// non-pointer section would need this check extended, which the failure
// message below says explicitly.
func rcAssertSeedComplete(t *testing.T, p agentcfg.ConfigPayload) {
	t.Helper()
	v := reflect.ValueOf(p)
	tp := v.Type()
	for i := range tp.NumField() {
		field := tp.Field(i)
		fv := v.Field(i)
		if fv.Kind() != reflect.Pointer {
			t.Fatalf("rebuild-completeness guard: ConfigPayload.%s is not an optional-pointer section — "+
				"rcSeed and rcAssertSeedComplete (rebuild_completeness_test.go) assume every section is a "+
				"pointer and need updating for the new shape", field.Name)
			continue
		}
		if fv.IsNil() {
			t.Fatalf("rebuild-completeness guard: ConfigPayload.%s is nil in the seed — a new section was "+
				"added to agentcfg.ConfigPayload without extending rcSeed (rebuild_completeness_test.go). "+
				"Populate it in rcSeed AND add its carry-forward to all seven section-scoped setters "+
				"(mcppolicy.go, addconnection.go, removeconnection.go, setdiscoveryorigins.go, skills.go, promptlayers.go, llmparams.go) — see D-283.",
				field.Name)
		}
	}
}

// rcAssertSiblingsSurvive reflection-walks every ConfigPayload field OTHER
// than owned and asserts it is byte-identical (reflect.DeepEqual) to seed's
// value. The walk is BY REFLECTION, not a hand-enumerated if-per-section
// list — a section added to ConfigPayload in the future is automatically
// covered here too, so the guard closes the omission CLASS (D-283), not just
// today's six known sections.
func rcAssertSiblingsSurvive(t *testing.T, verb, owned string, seed, got agentcfg.ConfigPayload) {
	t.Helper()
	sv := reflect.ValueOf(seed)
	gv := reflect.ValueOf(got)
	tp := sv.Type()
	for i := range tp.NumField() {
		name := tp.Field(i).Name
		if name == owned {
			continue
		}
		sf := sv.Field(i).Interface()
		gf := gv.Field(i).Interface()
		if !reflect.DeepEqual(sf, gf) {
			t.Errorf("%s dropped/altered the %q sibling section (rebuild-completeness guard): seed=%#v got=%#v",
				verb, name, sf, gf)
		}
	}
}

// rcSeedActive writes seed as the agent's active revision directly on the
// registry (bypassing wire validation — the seed's shape is already valid),
// so every subtest starts from the identical fully-populated baseline.
func rcSeedActive(t *testing.T, ctx context.Context, reg agentcfg.Registry, seed agentcfg.ConfigPayload) {
	t.Helper()
	if _, err := reg.SetRevision(ctx, rcQuad(), rcAgent, agentcfg.ConfigScopeAgent, seed); err != nil {
		t.Fatalf("seed active revision: %v", err)
	}
}

// rcActive reads back the agent's active revision payload from the registry.
func rcActive(t *testing.T, ctx context.Context, reg agentcfg.Registry) agentcfg.ConfigPayload {
	t.Helper()
	rev, ok, err := reg.Active(ctx, rcQuad(), rcAgent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("active: ok=%v err=%v", ok, err)
	}
	return rev.Payload
}

// TestRebuildCompleteness_EverySetter_PreservesEverySibling drives every
// section-scoped setter against the fully-populated active revision and
// asserts every non-owned section survives byte-identically. A future
// section-scoped setter (e.g. a connection-removal verb) joins the guard as
// ONE table row — name + the section it owns + setter-specific Service
// options + the invoke closure — nothing else changes.
func TestRebuildCompleteness_EverySetter_PreservesEverySibling(t *testing.T) {
	cases := []struct {
		name string
		// owned is the ONE ConfigPayload field name the setter replaces
		// (excluded from the survival assertions).
		owned string
		// opts builds the setter-specific Service options (nil = none).
		opts func(t *testing.T) []agentcfgprotocol.Option
		// invoke performs the setter's edit against the seeded rcAgent.
		invoke func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service)
	}{
		{
			name:  "set_tool_exposure",
			owned: "ToolExposure",
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
					Identity: scope(), AgentID: rcAgent,
					ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"edited-srv"}},
				}); err != nil {
					t.Fatalf("set_tool_exposure: %v", err)
				}
			},
		},
		{
			name:  "add_mcp_connection",
			owned: "Connections",
			opts: func(_ *testing.T) []agentcfgprotocol.Option {
				// nil result = online; fakeAttacher is defined in addconnection_test.go.
				return []agentcfgprotocol.Option{agentcfgprotocol.WithConnectionAttacher(&fakeAttacher{result: nil})}
			},
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.AddMCPConnection(ctx, prototypes.AgentConfigAddMCPConnectionRequest{
					Identity: scope(), AgentID: rcAgent,
					Connection: prototypes.AgentConfigMCPConnectionDescriptor{
						Name: "edited-conn", Transport: "http", URL: "https://example.invalid/edited",
					},
				}); err != nil {
					t.Fatalf("add_mcp_connection: %v", err)
				}
			},
		},
		{
			name:  "remove_mcp_connection",
			owned: "Connections",
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				// Remove the seed's connection ("seed-conn"). Its tool-exposure
				// residue prune targets only "seed-conn"-owned keys (none in the
				// seed), so ToolExposure — and every other sibling — survives
				// byte-identical; the guard proves the carry-forward.
				if _, err := s.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
					Identity: scope(), AgentID: rcAgent, Name: "seed-conn",
				}); err != nil {
					t.Fatalf("remove_mcp_connection: %v", err)
				}
			},
		},
		{
			name:  "skills.upsert",
			owned: "Skills",
			opts: func(t *testing.T) []agentcfgprotocol.Option {
				return []agentcfgprotocol.Option{agentcfgprotocol.WithSkillStore(newSkills(t))}
			},
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
					Identity: scope(), AgentID: rcAgent,
					Skill: prototypes.AgentConfigSkillInput{
						Name: "edited-skill", Trigger: "t", Steps: []string{"do the thing"},
						Origin: "generated", Scope: "project",
					},
				}); err != nil {
					t.Fatalf("skills.upsert: %v", err)
				}
			},
		},
		{
			name:  "set_prompt_layers",
			owned: "PromptLayers",
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
					Identity: scope(), AgentID: rcAgent,
					PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr("edited-base")},
				}); err != nil {
					t.Fatalf("set_prompt_layers: %v", err)
				}
			},
		},
		{
			name:  "set_llm_params",
			owned: "LLMParams",
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
					Identity: scope(), AgentID: rcAgent,
					LLMParams: prototypes.AgentConfigLLMParams{Temperature: f64(0.9)},
				}); err != nil {
					t.Fatalf("set_llm_params: %v", err)
				}
			},
		},
		{
			// set_mcp_discovery_origins rebuilds ConfigPayload by hand
			// (rebuildWithDiscoveryOrigins) to replace the named connection's
			// allow-list — the same omission class the guard exists to close.
			// No discoveryApplier is wired here, so the write is revision-only
			// (applied_live=false); the rebuild carry-forward is what matters.
			name:  "set_mcp_discovery_origins",
			owned: "Connections",
			invoke: func(t *testing.T, ctx context.Context, s *agentcfgprotocol.Service) {
				if _, err := s.SetMCPDiscoveryOrigins(ctx, prototypes.AgentConfigSetMCPDiscoveryOriginsRequest{
					Identity: scope(), AgentID: rcAgent, Name: "seed-conn",
					AllowedOrigins: []string{"https://as.example.invalid"},
				}); err != nil {
					t.Fatalf("set_mcp_discovery_origins: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			reg := newRegistry(t)
			var opts []agentcfgprotocol.Option
			if tc.opts != nil {
				opts = tc.opts(t)
			}
			s, err := agentcfgprotocol.NewService(reg, opts...)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			seed := rcSeed(t)
			rcSeedActive(t, ctx, reg, seed)

			tc.invoke(t, ctx, s)
			rcAssertSiblingsSurvive(t, tc.name, tc.owned, seed, rcActive(t, ctx, reg))
		})
	}
}
