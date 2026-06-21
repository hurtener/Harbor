package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

func sp(s string) *string   { return &s }
func fp(f float64) *float64 { return &f }
func ip(i int) *int         { return &i }

// TestActiveLLMOverrides_ResolvesPinnedSection proves the projection reads
// the agent's active revision and returns the pinned per-agent LLM-params
// as a planner.LLMOverrides carrying ONLY those dimensions.
func TestActiveLLMOverrides_ResolvesPinnedSection(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{
			Model:           sp("model-x"),
			Temperature:     fp(0.4),
			MaxTokens:       ip(2048),
			ReasoningEffort: sp("high"),
		},
	}); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}

	got, err := projection.ActiveLLMOverrides(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("ActiveLLMOverrides: %v", err)
	}
	if got == nil {
		t.Fatal("ActiveLLMOverrides returned nil, want the pinned section")
	}
	if got.Model == nil || *got.Model != "model-x" {
		t.Errorf("Model = %v, want model-x", got.Model)
	}
	if got.Temperature == nil || *got.Temperature != 0.4 {
		t.Errorf("Temperature = %v, want 0.4", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %v, want 2048", got.MaxTokens)
	}
	if got.ReasoningEffort == nil || *got.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %v, want high", got.ReasoningEffort)
	}
	// The per-agent layer must NOT carry prompt text.
	if got.ExtraInstructions != nil || got.SystemPromptOverride != nil {
		t.Errorf("per-agent layer leaked prompt text: extra=%v system=%v", got.ExtraInstructions, got.SystemPromptOverride)
	}
}

// TestActiveLLMOverrides_PartialSection proves an unset dimension is omitted
// (so it falls through to the next resolution layer downstream).
func TestActiveLLMOverrides_PartialSection(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		LLMParams: &agentcfg.LLMParams{Temperature: fp(0.7)},
	}); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}
	got, err := projection.ActiveLLMOverrides(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("ActiveLLMOverrides: %v", err)
	}
	if got == nil || got.Temperature == nil || *got.Temperature != 0.7 {
		t.Fatalf("Temperature = %v, want 0.7", got)
	}
	if got.Model != nil || got.MaxTokens != nil || got.ReasoningEffort != nil {
		t.Errorf("unset dimensions leaked: %+v", got)
	}
}

// TestActiveLLMOverrides_NoOverridePaths proves every "no per-agent override"
// path returns (nil, nil) — a nil registry, an empty agentID, an agent with
// no active revision, and an active revision with no LLM-params section.
func TestActiveLLMOverrides_NoOverridePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("nil registry", func(t *testing.T) {
		got, err := projection.ActiveLLMOverrides(ctx, nil, projAgent, projID())
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("empty agentID", func(t *testing.T) {
		reg := newRegistry(t)
		got, err := projection.ActiveLLMOverrides(ctx, reg, "", projID())
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("no active revision", func(t *testing.T) {
		reg := newRegistry(t)
		got, err := projection.ActiveLLMOverrides(ctx, reg, projAgent, projID())
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("active revision without an LLM-params section", func(t *testing.T) {
		reg := newRegistry(t)
		if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
			Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
		}); err != nil {
			t.Fatalf("SetRevision: %v", err)
		}
		got, err := projection.ActiveLLMOverrides(ctx, reg, projAgent, projID())
		if err != nil || got != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", got, err)
		}
	})
}
