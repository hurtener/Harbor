package protocol_test

// userskillimport_capability_policy_test.go — focused regression for the
// exported production capability-policy constructor
// (NewUserSkillImportCapabilityPolicy): usability through the public
// UserSkillImportCapability interface, the canonical policy envelope from the
// ActivePlannerCatalogView projection, the fail-loud nil-catalog path, and
// the defensive copy of the granted-scope ceiling (a caller mutating its
// input slice after construction cannot change the adapter's behavior).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
)

// capabilityPolicyTestCatalog registers an ungated tool plus a scope-gated
// tool, so the granted-scope ceiling is observable in the policy output: with
// scope:a granted both tools are permitted; without it the scoped tool is
// filtered out.
func capabilityPolicyTestCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	register := func(tool tools.Tool) {
		t.Helper()
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tool,
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", tool.Name, err)
		}
	}
	register(tools.Tool{Name: "open_tool"})
	register(tools.Tool{Name: "scoped_tool", AuthScopes: []string{"scope:a"}})
	return cat
}

// capabilityPolicyFixture wires the real registry + real StateStore-backed
// session overlay and seeds an active agent-scope revision, so the
// projection's lifecycle fence is live and the policy call resolves through
// the canonical projection path (no fake seams on the adapter boundary).
type capabilityPolicyFixture struct {
	caller  identity.Identity
	reg     agentcfg.Registry
	overlay sessionoverlay.Store
}

func newCapabilityPolicyFixture(t *testing.T) capabilityPolicyFixture {
	t.Helper()
	fx := capabilityPolicyFixture{caller: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	// The overlay must share the registry's StateStore so the lifecycle fence
	// the registry writes is the one the overlay's Get reads.
	reg, st := newRegistryWithState(t)
	fx.reg = reg
	var err error
	fx.overlay, err = sessionoverlay.NewStore(st, nil)
	if err != nil {
		t.Fatalf("sessionoverlay.NewStore: %v", err)
	}
	if _, err := fx.reg.SetRevision(context.Background(), identity.Quadruple{Identity: fx.caller}, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"history"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed active revision: %v", err)
	}
	return fx
}

func TestUserSkillImportCapabilityPolicy_Constructor_UsableThroughInterface(t *testing.T) {
	ctx := context.Background()
	fx := newCapabilityPolicyFixture(t)
	cat := capabilityPolicyTestCatalog(t)

	// granted carries spare capacity so a caller append writes into the SAME
	// backing array the adapter would share without a defensive copy.
	granted := make([]string, 1, 2)
	granted[0] = "scope:a"

	// The constructor returns the interface, so composition from another
	// package never needs the private concrete type.
	capability := agentcfgprotocol.NewUserSkillImportCapabilityPolicy(fx.reg, fx.overlay, cat, granted)

	policy, err := capability.Policy(ctx, fx.caller, testAgentID)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	if policy.ID != "harbor.user-skill-import" || policy.Version != "1" {
		t.Errorf("policy envelope = (%q, %q), want (harbor.user-skill-import, 1)", policy.ID, policy.Version)
	}
	if !sort.StringsAreSorted(policy.PermittedTools) {
		t.Errorf("PermittedTools not sorted: %v", policy.PermittedTools)
	}
	if len(policy.PermittedNS) != 0 || len(policy.PermittedTags) != 0 {
		t.Errorf("policy carried ns/tags, want empty: %+v", policy)
	}
	if !containsStr(policy.PermittedTools, "open_tool") || !containsStr(policy.PermittedTools, "scoped_tool") {
		t.Errorf("PermittedTools = %v, want open_tool + scoped_tool (scope:a granted)", policy.PermittedTools)
	}

	// Later mutation of the caller's slice — an in-place element swap AND an
	// append into the shared spare capacity — must not change the adapter.
	granted[0] = "scope:other"
	granted = append(granted, "scope:b")
	again, err := capability.Policy(ctx, fx.caller, testAgentID)
	if err != nil {
		t.Fatalf("Policy after caller mutation: %v", err)
	}
	if !containsStr(again.PermittedTools, "scoped_tool") {
		t.Errorf("caller slice mutation leaked into the adapter: PermittedTools = %v", again.PermittedTools)
	}

	// Contrast: a fresh adapter built from the MUTATED slice sees the new
	// ceiling — the granted-scope filter is live, and the first adapter's
	// unchanged snapshot is exactly what the constructor copied.
	mutated := agentcfgprotocol.NewUserSkillImportCapabilityPolicy(fx.reg, fx.overlay, cat, granted)
	fromMutated, err := mutated.Policy(ctx, fx.caller, testAgentID)
	if err != nil {
		t.Fatalf("mutated-slice adapter Policy: %v", err)
	}
	if containsStr(fromMutated.PermittedTools, "scoped_tool") {
		t.Errorf("mutated-slice adapter still permits scoped_tool, want filtered: %v", fromMutated.PermittedTools)
	}
	if !containsStr(fromMutated.PermittedTools, "open_tool") {
		t.Errorf("mutated-slice adapter lost open_tool: %v", fromMutated.PermittedTools)
	}
}

func TestUserSkillImportCapabilityPolicy_Constructor_NilCatalogFailsLoud(t *testing.T) {
	ctx := context.Background()
	fx := newCapabilityPolicyFixture(t)
	capability := agentcfgprotocol.NewUserSkillImportCapabilityPolicy(fx.reg, fx.overlay, nil, nil)
	if _, err := capability.Policy(ctx, fx.caller, testAgentID); !errors.Is(err, agentcfgprotocol.ErrUserSkillImportMisconfigured) {
		t.Fatalf("nil-catalog Policy error = %v, want ErrUserSkillImportMisconfigured", err)
	}
}
