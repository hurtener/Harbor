package react_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
)

// strPtr / f64Ptr / intPtr are local pointer helpers for the override
// fixtures.
func strPtr(s string) *string   { return &s }
func f64Ptr(f float64) *float64 { return &f }
func intPtrOv(i int) *int       { return &i }

func overrideRC(ov *planner.LLMOverrides) planner.RunContext {
	return planner.RunContext{
		Quadruple: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r-override",
		},
		Goal:         "do the thing",
		LLMOverrides: ov,
	}
}

func finishClient() *capturingClient {
	return &capturingClient{
		response: llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{{
				ID:   "call_done",
				Name: "_finish",
				Args: json.RawMessage(`{"answer":"ok"}`),
			}},
		},
	}
}

// TestApplyLLMOverrides_ScalarsStampedOntoRequest asserts the run-start
// override bundle's scalar fields land on the LLM request the planner
// sends (model / temperature / max-tokens / reasoning-effort).
func TestApplyLLMOverrides_ScalarsStampedOntoRequest(t *testing.T) {
	cap := finishClient()
	p := react.New(cap)
	rc := overrideRC(&planner.LLMOverrides{
		Model:           strPtr("model-x"),
		Temperature:     f64Ptr(0.5),
		MaxTokens:       intPtrOv(1234),
		ReasoningEffort: strPtr("high"),
	})
	if _, err := p.Next(context.Background(), rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	req := cap.lastRequest()
	if req.Model != "model-x" {
		t.Errorf("req.Model = %q, want model-x", req.Model)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("req.Temperature = %v, want 0.5", req.Temperature)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 1234 {
		t.Errorf("req.MaxTokens = %v, want 1234", req.MaxTokens)
	}
	if req.ReasoningEffort != llm.ReasoningHigh {
		t.Errorf("req.ReasoningEffort = %q, want high", req.ReasoningEffort)
	}
}

// TestApplyLLMOverrides_ExtraInstructionsAdditive asserts the additive
// extra-instructions render into the system prompt — and that they do NOT
// replace the base prompt (the base system content is still present).
func TestApplyLLMOverrides_ExtraInstructionsAdditive(t *testing.T) {
	cap := finishClient()
	p := react.New(cap)
	rc := overrideRC(&planner.LLMOverrides{
		ExtraInstructions: strPtr("ALWAYS answer in French."),
	})
	if _, err := p.Next(context.Background(), rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	req := cap.lastRequest()
	if !messageBodyContains(req.Messages, "ALWAYS answer in French.") {
		t.Errorf("system prompt missing additive extra-instructions")
	}
	// The base prompt must survive — extra-instructions are additive, not
	// a replace. The additional-guidance section wrapper is the marker.
	if !messageBodyContains(req.Messages, "<additional_guidance>") {
		t.Errorf("extra-instructions not rendered in <additional_guidance> section")
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem {
		t.Errorf("first message is not the base system message")
	}
}

// TestApplyLLMOverrides_NilBundleIsNoOp asserts a nil override bundle
// leaves the request's scalar fields at their zero/agent-default state
// (the planner does not stamp a model; the safety edge fills it later).
func TestApplyLLMOverrides_NilBundleIsNoOp(t *testing.T) {
	cap := finishClient()
	p := react.New(cap)
	rc := overrideRC(nil)
	if _, err := p.Next(context.Background(), rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	req := cap.lastRequest()
	if req.Model != "" {
		t.Errorf("req.Model = %q, want empty (planner leaves model to the safety edge)", req.Model)
	}
	if req.Temperature != nil {
		t.Errorf("req.Temperature = %v, want nil", req.Temperature)
	}
	if req.MaxTokens != nil {
		t.Errorf("req.MaxTokens = %v, want nil", req.MaxTokens)
	}
}

// TestApplyLLMOverrides_UnknownReasoningIgnored asserts a malformed
// reasoning-effort value (which should never reach here — validated at set
// time) is defensively ignored rather than passed to the provider.
func TestApplyLLMOverrides_UnknownReasoningIgnored(t *testing.T) {
	cap := finishClient()
	p := react.New(cap)
	rc := overrideRC(&planner.LLMOverrides{ReasoningEffort: strPtr("turbo")})
	if _, err := p.Next(context.Background(), rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	req := cap.lastRequest()
	if req.ReasoningEffort != "" {
		t.Errorf("req.ReasoningEffort = %q, want empty (unknown value ignored)", req.ReasoningEffort)
	}
}
