package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func f64(f float64) *float64 { return &f }
func intp(i int) *int        { return &i }

// svcWithModels builds a Service whose set_llm_params validates a set model
// against the supplied configured-model set.
func svcWithModels(t *testing.T, models ...string) *agentcfgprotocol.Service {
	t.Helper()
	s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithValidModels(models))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

// TestSetLLMParams_RecordsRevision_PreservesSiblings proves an LLM-params edit
// composes a new revision pinning the section AND preserves the active
// revision's prompt-layer + skills + tool-exposure sections (the desired-state
// REPLACE touches only the LLM-params section).
func TestSetLLMParams_RecordsRevision_PreservesSiblings(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-x")
	// Seed sibling sections.
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills:       &prototypes.AgentConfigSkillsSelection{Names: []string{"skA"}},
			ToolExposure: &prototypes.AgentConfigToolExposure{PausedServers: []string{"srvA"}},
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: strPtr("the base")},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	resp, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{
			Model:           strPtr("model-x"),
			Temperature:     f64(0.3),
			MaxTokens:       intp(4096),
			ReasoningEffort: strPtr("medium"),
		},
	})
	if err != nil {
		t.Fatalf("set llm params: %v", err)
	}
	pl := resp.Revision.Payload
	if pl.LLMParams == nil || pl.LLMParams.Model == nil || *pl.LLMParams.Model != "model-x" {
		t.Fatalf("model not recorded: %+v", pl.LLMParams)
	}
	if pl.LLMParams.Temperature == nil || *pl.LLMParams.Temperature != 0.3 {
		t.Fatalf("temperature not recorded: %+v", pl.LLMParams)
	}
	if pl.Skills == nil || len(pl.Skills.Names) != 1 {
		t.Fatalf("skills section not preserved across llm-params edit: %+v", pl.Skills)
	}
	if pl.ToolExposure == nil || len(pl.ToolExposure.PausedServers) != 1 {
		t.Fatalf("tool-exposure section not preserved: %+v", pl.ToolExposure)
	}
	if pl.PromptLayers == nil || pl.PromptLayers.Base == nil || *pl.PromptLayers.Base != "the base" {
		t.Fatalf("prompt-layer section not preserved: %+v", pl.PromptLayers)
	}
}

// TestSetLLMParams_PreservedByOtherEdits proves the symmetric invariant: a
// prompt-layer / skills / tool-exposure edit preserves a previously-pinned
// LLM-params section (the bidirectional section-merge — the §17.6 cross-cut:
// a prompt edit must NOT silently wipe a pinned model).
func TestSetLLMParams_PreservedByOtherEdits(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-x")
	if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: strPtr("model-x"), Temperature: f64(0.5)},
	}); err != nil {
		t.Fatalf("set llm params: %v", err)
	}
	// A prompt-layer edit must keep the LLM-params.
	plResp, err := s.SetPromptLayers(ctx, prototypes.AgentConfigSetPromptLayersRequest{
		Identity: scope(), AgentID: testAgentID,
		PromptLayers: prototypes.AgentConfigPromptLayers{Base: strPtr("new base")},
	})
	if err != nil {
		t.Fatalf("set prompt layers: %v", err)
	}
	if plResp.Revision.Payload.LLMParams == nil || plResp.Revision.Payload.LLMParams.Model == nil ||
		*plResp.Revision.Payload.LLMParams.Model != "model-x" {
		t.Fatalf("LLM-params cleared by a prompt edit: %+v", plResp.Revision.Payload.LLMParams)
	}
	// A tool-exposure edit must keep the LLM-params too.
	teResp, err := s.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvB"}},
	})
	if err != nil {
		t.Fatalf("set tool exposure: %v", err)
	}
	if teResp.Revision.Payload.LLMParams == nil || teResp.Revision.Payload.LLMParams.Model == nil ||
		*teResp.Revision.Payload.LLMParams.Model != "model-x" {
		t.Fatalf("LLM-params cleared by a tool-exposure edit: %+v", teResp.Revision.Payload.LLMParams)
	}
}

// TestSetLLMParams_UnknownModelRejected proves a set model with no configured
// ModelProfile fails loud at set time (parity with the tenant model-swap) and
// records NO revision.
func TestSetLLMParams_UnknownModelRejected(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a")
	_, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: strPtr("ghost-model")},
	})
	if !errors.Is(err, agentcfgprotocol.ErrUnknownModel) {
		t.Fatalf("error = %v, want ErrUnknownModel", err)
	}
	// No revision recorded.
	get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if get.Set {
		t.Fatalf("a rejected set must not record a revision: %+v", get.Revision)
	}
}

// TestSetLLMParams_UnknownModelRejectedViaSetRevision proves the full-payload
// set_revision path ALSO validates a pinned model at set time.
func TestSetLLMParams_UnknownModelRejectedViaSetRevision(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a")
	_, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			LLMParams: &prototypes.AgentConfigLLMParams{Model: strPtr("ghost-model")},
		},
	})
	if !errors.Is(err, agentcfgprotocol.ErrUnknownModel) {
		t.Fatalf("error = %v, want ErrUnknownModel (set_revision must validate too)", err)
	}
}

// TestSetLLMParams_UnsetModelAllowedWithoutValidation proves an LLM-params
// edit that sets only sampling params (no model) is accepted even when a
// model would otherwise be validated — only a set model is validated.
func TestSetLLMParams_UnsetModelAllowed(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a")
	resp, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Temperature: f64(0.8)},
	})
	if err != nil {
		t.Fatalf("set llm params (no model): %v", err)
	}
	if resp.Revision.Payload.LLMParams == nil || resp.Revision.Payload.LLMParams.Temperature == nil {
		t.Fatalf("temperature-only edit not recorded: %+v", resp.Revision.Payload.LLMParams)
	}
}

// TestSetLLMParams_OutOfRangeSamplingRejected proves an out-of-range
// sampling value fails loud at set time with ErrInvalidLLMParams (parity with
// runs.set_overrides) and records NO revision.
func TestSetLLMParams_OutOfRangeSamplingRejected(t *testing.T) {
	ctx := context.Background()
	cases := map[string]prototypes.AgentConfigLLMParams{
		"temperature too high": {Temperature: f64(2.5)},
		"temperature negative": {Temperature: f64(-0.1)},
		"non-positive maxtok":  {MaxTokens: intp(0)},
		"negative maxtok":      {MaxTokens: intp(-5)},
		"unknown reasoning":    {ReasoningEffort: strPtr("ultra")},
	}
	for name, lp := range cases {
		t.Run(name, func(t *testing.T) {
			s := svcWithModels(t, "model-a")
			_, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
				Identity: scope(), AgentID: testAgentID, LLMParams: lp,
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidLLMParams) {
				t.Fatalf("error = %v, want ErrInvalidLLMParams", err)
			}
			get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if get.Set {
				t.Fatalf("a rejected set must not record a revision")
			}
		})
	}
}

// TestSetLLMParams_OffReasoningEffortAccepted proves "off" is in the canonical
// reasoning-effort taxonomy the per-agent section accepts (it is a valid
// planner value).
func TestSetLLMParams_OffReasoningEffortAccepted(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a")
	if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{ReasoningEffort: strPtr("off")},
	}); err != nil {
		t.Fatalf("reasoning_effort=off should be accepted: %v", err)
	}
}

// TestSetLLMParams_DiffShowsDelta proves a diff of two revisions surfaces the
// per-field LLM-params delta.
func TestSetLLMParams_DiffShowsDelta(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a", "model-b")
	r1, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: strPtr("model-a"), Temperature: f64(0.2)},
	})
	if err != nil {
		t.Fatalf("rev1: %v", err)
	}
	r2, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: strPtr("model-b"), Temperature: f64(0.2)},
	})
	if err != nil {
		t.Fatalf("rev2: %v", err)
	}
	d, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r1.Revision.RevisionID, ToRevision: r2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	lp := d.Diff.LLMParams
	if !lp.ModelChanged || lp.ModelFrom != "model-a" || lp.ModelTo != "model-b" {
		t.Fatalf("model delta = %+v", lp)
	}
	if lp.TemperatureChanged {
		t.Fatalf("temperature should be unchanged (0.2 → 0.2): %+v", lp)
	}
}

// TestSetLLMParams_IdentityRequired proves an incomplete identity triple
// fails closed.
func TestSetLLMParams_IdentityRequired(t *testing.T) {
	ctx := context.Background()
	s := svcWithModels(t, "model-a")
	_, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity:  prototypes.IdentityScope{Tenant: "t", User: "", Session: "s"},
		AgentID:   testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: strPtr("model-a")},
	})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should fail with ErrIdentityRequired, got %v", err)
	}
}
