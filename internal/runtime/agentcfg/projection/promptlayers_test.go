package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func ps(s string) *string { return &s }

// TestActivePromptLayers_NoRegistry returns the no-op path.
func TestActivePromptLayers_NoRegistry(t *testing.T) {
	base, user, ok, err := projection.ActivePromptLayers(context.Background(), nil, projAgent, projID())
	if err != nil || ok || base != "" || user != "" {
		t.Fatalf("nil registry should be a no-op: base=%q user=%q ok=%v err=%v", base, user, ok, err)
	}
}

// TestActivePromptLayers_NoActiveRevision returns the backward-compatible path.
func TestActivePromptLayers_NoActiveRevision(t *testing.T) {
	reg := newRegistry(t)
	_, _, ok, err := projection.ActivePromptLayers(context.Background(), reg, projAgent, projID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("no active revision should return ok=false")
	}
}

// TestActivePromptLayers_ResolvesBaseAndUser proves the projection reads the
// active revision's base + user layers.
func TestActivePromptLayers_ResolvesBaseAndUser(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("the base"), User: ps("the user")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	base, user, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if base != "the base" || user != "the user" {
		t.Fatalf("base=%q user=%q", base, user)
	}
}

// TestActivePromptLayers_NoPromptSection returns ok=false when the active
// revision has only sibling sections.
func TestActivePromptLayers_NoPromptSection(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	_, _, ok, err := projection.ActivePromptLayers(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Fatal("a revision with no prompt-layer section should return ok=false")
	}
}

// TestApplyPromptLayers_OverlaysOntoNil allocates a bundle when none exists.
func TestApplyPromptLayers_OverlaysOntoNil(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("B"), User: ps("U")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov == nil || ov.BasePromptLayer == nil || *ov.BasePromptLayer != "B" ||
		ov.UserPromptLayer == nil || *ov.UserPromptLayer != "U" {
		t.Fatalf("overlay = %+v", ov)
	}
}

// TestApplyPromptLayers_PreservesExistingOverrides overlays onto an existing
// bundle without clobbering its other fields.
func TestApplyPromptLayers_PreservesExistingOverrides(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		PromptLayers: &agentcfg.PromptLayers{Base: ps("B")},
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	existing := &planner.LLMOverrides{ExtraInstructions: ps("tenant default")}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), existing)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov.ExtraInstructions == nil || *ov.ExtraInstructions != "tenant default" {
		t.Fatalf("extra instructions clobbered: %+v", ov)
	}
	if ov.BasePromptLayer == nil || *ov.BasePromptLayer != "B" {
		t.Fatalf("base not applied: %+v", ov)
	}
	if ov.UserPromptLayer != nil {
		t.Fatalf("user should be unset when not configured: %+v", ov.UserPromptLayer)
	}
}

// TestApplyPromptLayers_NoLayersUnchanged leaves the bundle untouched when no
// durable layers are configured.
func TestApplyPromptLayers_NoLayersUnchanged(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	existing := &planner.LLMOverrides{Model: ps("m")}
	ov, err := projection.ApplyPromptLayers(ctx, reg, nil, projAgent, projID(), existing)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if ov != existing {
		t.Fatal("bundle should be returned unchanged when no durable layers exist")
	}
}
