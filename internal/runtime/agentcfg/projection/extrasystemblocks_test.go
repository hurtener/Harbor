package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

// extrasystemblocks_test.go — the run-start projection of the ORDERED
// additive prompt blocks (phase 222 / D-367).

func seedBlocks(t *testing.T, reg agentcfg.Registry, id identity.Quadruple, pairs ...string) {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatalf("seedBlocks takes name/body pairs")
	}
	bs := make([]agentcfg.NamedBlock, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		bs = append(bs, agentcfg.NamedBlock{Name: pairs[i], Body: pairs[i+1]})
	}
	if _, err := reg.SetRevision(t.Context(), id, projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ExtraSystemBlocks: &agentcfg.ExtraSystemBlocks{Blocks: bs},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
}

// TestActiveExtraSystemBlocks_ResolvesInDeclaredOrder — the resolution keeps
// the declared order (the render order).
func TestActiveExtraSystemBlocks_ResolvesInDeclaredOrder(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// Reverse-alphabetical so a sort anywhere would be visible.
	seedBlocks(t, reg, projID(), "zulu", "z body", "alpha", "a body")

	got, ok, err := projection.ActiveExtraSystemBlocks(ctx, reg, projAgent, projID())
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if len(got) != 2 || got[0].Name != "zulu" || got[1].Name != "alpha" {
		t.Fatalf("blocks = %+v, want [zulu alpha] in that order", got)
	}
	if got[0].Body != "z body" || got[1].Body != "a body" {
		t.Fatalf("bodies not resolved: %+v", got)
	}
}

// TestActiveExtraSystemBlocks_NotSetPaths — a nil registry, an empty agent
// id, no active revision and an active revision with no blocks section all
// resolve to "not set" WITHOUT an error (the backward-compatible path).
func TestActiveExtraSystemBlocks_NotSetPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("nil registry", func(t *testing.T) {
		got, ok, err := projection.ActiveExtraSystemBlocks(ctx, nil, projAgent, projID())
		if err != nil || ok || got != nil {
			t.Fatalf("nil registry should be a no-op: got=%v ok=%v err=%v", got, ok, err)
		}
	})
	t.Run("empty agent id", func(t *testing.T) {
		got, ok, err := projection.ActiveExtraSystemBlocks(ctx, newRegistry(t), "", projID())
		if err != nil || ok || got != nil {
			t.Fatalf("empty agent id should be a no-op: got=%v ok=%v err=%v", got, ok, err)
		}
	})
	t.Run("no active revision", func(t *testing.T) {
		got, ok, err := projection.ActiveExtraSystemBlocks(ctx, newRegistry(t), projAgent, projID())
		if err != nil || ok || got != nil {
			t.Fatalf("no active revision: got=%v ok=%v err=%v", got, ok, err)
		}
	})
	t.Run("active revision with only sibling sections", func(t *testing.T) {
		reg := newRegistry(t)
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
			Skills: &agentcfg.SkillsSelection{Names: []string{"s"}},
		}, agentcfg.SetOptions{}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, ok, err := projection.ActiveExtraSystemBlocks(ctx, reg, projAgent, projID())
		if err != nil || ok || got != nil {
			t.Fatalf("no blocks section: got=%v ok=%v err=%v", got, ok, err)
		}
	})
}

// TestActiveExtraSystemBlocks_IdentityScoped — the SAME agent id under two
// tenants resolves two independent block sets. agent_id is a KEY, never an
// isolation filter (CLAUDE.md §6 clarifying note).
func TestActiveExtraSystemBlocks_IdentityScoped(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	tenantA := identity.Quadruple{Identity: identity.Identity{TenantID: "ta", UserID: "u", SessionID: "s"}}
	tenantB := identity.Quadruple{Identity: identity.Identity{TenantID: "tb", UserID: "u", SessionID: "s"}}
	seedBlocks(t, reg, tenantA, "a-only", "body a")
	seedBlocks(t, reg, tenantB, "b-only", "body b")

	for _, tc := range []struct {
		id   identity.Quadruple
		want string
	}{{tenantA, "a-only"}, {tenantB, "b-only"}} {
		got, ok, err := projection.ActiveExtraSystemBlocks(ctx, reg, projAgent, tc.id)
		if err != nil || !ok {
			t.Fatalf("tenant %s: ok=%v err=%v", tc.id.TenantID, ok, err)
		}
		if len(got) != 1 || got[0].Name != tc.want {
			t.Fatalf("tenant %s sees %+v, want [%s] — agent_id leaked across the tenant boundary", tc.id.TenantID, got, tc.want)
		}
	}
}

// TestActiveExtraSystemBlocks_ReturnsAFreshCopy — the run's snapshot must not
// alias the registry's payload, or a later mutation of one would be visible
// through the other (the concurrent-reuse contract).
func TestActiveExtraSystemBlocks_ReturnsAFreshCopy(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedBlocks(t, reg, projID(), "a", "original")

	first, _, err := projection.ActiveExtraSystemBlocks(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	first[0].Body = "mutated through the caller's slice"

	second, _, err := projection.ActiveExtraSystemBlocks(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second[0].Body != "original" {
		t.Fatalf("the projection handed out the registry's own backing array: %q", second[0].Body)
	}
}

// TestApplyPromptLayers_CarriesExtraSystemBlocks — the ONE shared run-start
// seam overlays the blocks onto the run's bundle, in order, WITHOUT
// clobbering the prompt layers it already carries.
func TestApplyPromptLayers_CarriesExtraSystemBlocks(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("the base"), User: ps("the user")},
		ExtraSystemBlocks: &agentcfg.ExtraSystemBlocks{Blocks: []agentcfg.NamedBlock{
			{Name: "zulu", Body: "z"},
			{Name: "alpha", Body: "a"},
		}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A pre-existing bundle carrying an unrelated override must survive.
	in := &planner.LLMOverrides{ExtraInstructions: ps("tenant additive")}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov == nil {
		t.Fatal("apply returned a nil bundle")
	}
	if len(ov.ExtraSystemBlocks) != 2 || ov.ExtraSystemBlocks[0].Name != "zulu" || ov.ExtraSystemBlocks[1].Name != "alpha" {
		t.Fatalf("blocks = %+v, want [zulu alpha] in that order", ov.ExtraSystemBlocks)
	}
	if ov.BasePromptLayer == nil || *ov.BasePromptLayer != "the base" {
		t.Fatalf("the blocks overlay clobbered the base layer: %v", ov.BasePromptLayer)
	}
	if ov.UserPromptLayer == nil || *ov.UserPromptLayer != "the user" {
		t.Fatalf("the blocks overlay clobbered the user layer: %v", ov.UserPromptLayer)
	}
	if ov.ExtraInstructions == nil || *ov.ExtraInstructions != "tenant additive" {
		t.Fatalf("the blocks overlay clobbered an unrelated field: %v", ov.ExtraInstructions)
	}
}

// TestApplyPromptLayers_ExtraSystemBlocksOnly_AllocatesABundle — an agent with blocks
// but NO prompt layers still gets a bundle (the nil-in case), and a run with
// neither still gets its nil back unchanged.
func TestApplyPromptLayers_ExtraSystemBlocksOnly_AllocatesABundle(t *testing.T) {
	ctx := context.Background()

	t.Run("blocks with no layers allocate", func(t *testing.T) {
		reg := newRegistry(t)
		seedBlocks(t, reg, projID(), "only", "block")
		ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if ov == nil || len(ov.ExtraSystemBlocks) != 1 {
			t.Fatalf("blocks-only agent got %+v, want a bundle carrying one block", ov)
		}
	})

	t.Run("neither leaves the bundle untouched", func(t *testing.T) {
		reg := newRegistry(t)
		ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if ov != nil {
			t.Fatalf("an agent with no durable prompt state allocated a bundle: %+v", ov)
		}
	})
}
