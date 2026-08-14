package serve

// bootpacks_test.go — the serve-band HA-66 boot-baseline composition:
// the eager immutable index opener, the resolved-agent validation
// wrapper, and the pre-read durable collision check. The loader itself
// (internal/skills/bootpacks) and the config validation are covered by
// their own phases; these tests pin the serve-band wiring posture: an
// empty declaration resolves NO baseline, nil seams are no-ops, and the
// static catalog adapter answers presence (metadata only — never a
// grant).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestOpenBootPackIndex_EmptyDeclaration_NoBaseline pins the
// partial-build posture: no `skills.boot_agent_packs` declaration
// resolves (nil, nil) — no boot baseline bound, guards inert, preview
// route 501.
func TestOpenBootPackIndex_EmptyDeclaration_NoBaseline(t *testing.T) {
	idx, err := OpenBootPackIndex(context.Background(), &config.Config{}, tools.NewCatalog())
	if err != nil {
		t.Fatalf("OpenBootPackIndex (empty): %v", err)
	}
	if idx != nil {
		t.Fatal("OpenBootPackIndex (empty) returned a non-nil index")
	}
}

// TestValidateBootAgentPacksForAgent_Delegation pins the serve wrapper
// over the config package's pure helper: no declarations pass, a
// declaration naming a different agent fails loud (the resolved
// boot/default agent is the ONLY admissible agent id).
func TestValidateBootAgentPacksForAgent_Delegation(t *testing.T) {
	if err := ValidateBootAgentPacksForAgent(&config.Config{}, "boot-agent"); err != nil {
		t.Fatalf("ValidateBootAgentPacksForAgent (no declarations): %v", err)
	}
	cfg := &config.Config{Skills: config.SkillsConfig{BootAgentPacks: []config.BootAgentPackConfig{
		{TenantID: "t1", AgentID: "other-agent", Directory: "/tmp/boot"},
	}}}
	if err := ValidateBootAgentPacksForAgent(cfg, "boot-agent"); err == nil {
		t.Fatal("ValidateBootAgentPacksForAgent with a mismatched agent must fail loud")
	}
	if err := ValidateBootAgentPacksForAgent(cfg, "other-agent"); err != nil {
		t.Fatalf("ValidateBootAgentPacksForAgent with the exact agent: %v", err)
	}
}

// TestPreReadBootPackCollisions_NilSafe pins the nil posture: no index
// or no registry is a no-op (an absent boot key / absent active
// revision passes).
func TestPreReadBootPackCollisions_NilSafe(t *testing.T) {
	ctx := context.Background()
	if err := PreReadBootPackCollisions(ctx, nil, nil, "dev"); err != nil {
		t.Fatalf("PreReadBootPackCollisions (nil index): %v", err)
	}
}

// TestBootCatalogAdapter_Compatible pins the loader's static
// compatibility reader: presence in the wrapped catalog answers true;
// an unknown name or a nil catalog answers false (required_tools is
// metadata-only and grants nothing).
func TestBootCatalogAdapter_Compatible(t *testing.T) {
	cat := tools.NewCatalog()
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "clock.now", Transport: tools.TransportInProcess, SideEffects: tools.SideEffectPure, Loading: tools.LoadingAlways},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	adapter := bootCatalogAdapter{cat: cat}
	if !adapter.Compatible("clock.now") {
		t.Error("Compatible(clock.now) = false, want true (registered in the wrapped catalog)")
	}
	if adapter.Compatible("no.such.tool") {
		t.Error("Compatible(no.such.tool) = true, want false")
	}
	if (bootCatalogAdapter{cat: nil}).Compatible("clock.now") {
		t.Error("Compatible with nil catalog = true, want false")
	}
}
