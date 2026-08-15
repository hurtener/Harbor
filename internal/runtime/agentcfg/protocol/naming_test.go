package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

// TestSetRevision_Naming_InvalidRejected proves the validateNaming bounds fire
// at set time (ErrInvalidNaming, BEFORE any registry write): negative
// after_turns, repeat_every without a cap (the no-unlimited invariant),
// out-of-range max_title_len, and an unknown model.
func TestSetRevision_Naming_InvalidRejected(t *testing.T) {
	ctx := context.Background()
	models := []string{"good-model"}
	cases := []struct {
		name   string
		naming *prototypes.AgentConfigNaming
	}{
		{"negative after_turns", &prototypes.AgentConfigNaming{Auto: true, AfterTurns: -1}},
		{"repeat without cap", &prototypes.AgentConfigNaming{Auto: true, RepeatEvery: 2}},
		{"repeat cap zero", &prototypes.AgentConfigNaming{Auto: true, RepeatEvery: 2, MaxRepetitions: 0}},
		{"max_title_len too small", &prototypes.AgentConfigNaming{Auto: true, MaxTitleLen: 5}},
		{"max_title_len too large", &prototypes.AgentConfigNaming{Auto: true, MaxTitleLen: 500}},
		{"unknown model", &prototypes.AgentConfigNaming{Auto: true, Model: "no-such"}},
		{"unknown reasoning mode", &prototypes.AgentConfigNaming{Auto: true, ReasoningMode: "sometimes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithValidModels(models))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
				Identity: scope(), AgentID: testAgentID,
				Payload: prototypes.AgentConfigPayload{Naming: tc.naming},
			})
			if !errors.Is(err, agentcfgprotocol.ErrInvalidNaming) {
				t.Fatalf("error = %v, want ErrInvalidNaming", err)
			}
			got, gErr := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if gErr != nil {
				t.Fatalf("Get: %v", gErr)
			}
			if got.Set {
				t.Fatal("a rejected naming set still persisted a revision")
			}
		})
	}
}

// TestSetRevision_Naming_ValidRoundTrips proves a valid naming section is
// accepted and round-trips through agent_config.get on the wire.
func TestSetRevision_Naming_ValidRoundTrips(t *testing.T) {
	ctx := context.Background()
	models := []string{"good-model"}
	s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithValidModels(models))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Naming: &prototypes.AgentConfigNaming{
				Auto: true, AfterTurns: 2, RepeatEvery: 3, MaxRepetitions: 5, MaxTitleLen: 100, Model: "good-model", ReasoningMode: "provider_default",
			},
		},
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	n := resp.Revision.Payload.Naming
	if n == nil || !n.Auto || n.AfterTurns != 2 || n.RepeatEvery != 3 || n.MaxRepetitions != 5 || n.MaxTitleLen != 100 || n.Model != "good-model" || n.ReasoningMode != "provider_default" {
		t.Fatalf("naming section did not round-trip on the wire: %+v", n)
	}
	// A zero-cap once-only policy is valid (repeat_every == 0).
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true}},
	}); err != nil {
		t.Fatalf("set (once-only): %v", err)
	}
}

func TestDiff_Naming_ReasoningModeVisible(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r1, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, ReasoningMode: "off"}},
	})
	if err != nil {
		t.Fatalf("rev1: %v", err)
	}
	r2, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, ReasoningMode: "provider_default"}},
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
	n := d.Diff.Naming
	if !n.ReasoningModeChanged || n.ReasoningModeFrom != "off" || n.ReasoningModeTo != "provider_default" {
		t.Fatalf("reasoning_mode wire diff = %+v", n)
	}
}

// TestSetLLMParams_PreservesPinnedNaming is the D-283 carry-forward regression
// for the naming section: set_revision pins a naming section, then a
// section-scoped set_llm_params (owning only LLMParams) must leave naming
// intact.
func TestSetLLMParams_PreservesPinnedNaming(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Naming: &prototypes.AgentConfigNaming{Auto: true, AfterTurns: 3}},
	}); err != nil {
		t.Fatalf("seed naming: %v", err)
	}
	temp := 0.7
	if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{
		Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Temperature: &temp},
	}); err != nil {
		t.Fatalf("set_llm_params: %v", err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Revision == nil || got.Revision.Payload.Naming == nil ||
		!got.Revision.Payload.Naming.Auto || got.Revision.Payload.Naming.AfterTurns != 3 {
		t.Fatalf("naming section was dropped by set_llm_params: %+v", got.Revision)
	}
}

// TestDiff_Naming_BareOptOutVisible proves `agent_config.diff` is NOT blind
// to the bare `{auto: false}` opt-out revision at the wire level: a revision
// with NO naming section diffed against one carrying the bare opt-out
// registers `auto_changed` with the tri-state display ("" → "false") — the
// exact revision the presence-preserve fix made meaningful never renders as
// "no change in any section" in the Console diff view.
func TestDiff_Naming_BareOptOutVisible(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	// Revision 1: a payload with NO naming section (an unrelated section so
	// the envelope is non-empty).
	r1, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"s1"}},
		},
	})
	if err != nil {
		t.Fatalf("rev1: %v", err)
	}
	// Revision 2: same envelope + the bare opt-out naming section.
	r2, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"s1"}},
			Naming: &prototypes.AgentConfigNaming{Auto: false},
		},
	})
	if err != nil {
		t.Fatalf("rev2 (bare opt-out): %v", err)
	}
	if r1.Revision.RevisionID == r2.Revision.RevisionID {
		t.Fatal("the bare opt-out did not produce a new revision (idempotent re-set) — presence is not being preserved")
	}

	// absent → bare opt-out.
	d, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r1.Revision.RevisionID, ToRevision: r2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	n := d.Diff.Naming
	if !n.AutoChanged || n.AutoFrom != "" || n.AutoTo != "false" {
		t.Fatalf("absent→bareOptOut wire diff = %+v, want auto_changed with \"\"→\"false\"", n)
	}

	// bare opt-out → absent (the rollback direction).
	d2, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: r2.Revision.RevisionID, ToRevision: r1.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("reverse diff: %v", err)
	}
	n2 := d2.Diff.Naming
	if !n2.AutoChanged || n2.AutoFrom != "false" || n2.AutoTo != "" {
		t.Fatalf("bareOptOut→absent wire diff = %+v, want auto_changed with \"false\"→\"\"", n2)
	}
}
