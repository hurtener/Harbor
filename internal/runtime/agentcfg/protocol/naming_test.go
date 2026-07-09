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
				Auto: true, AfterTurns: 2, RepeatEvery: 3, MaxRepetitions: 5, MaxTitleLen: 100, Model: "good-model",
			},
		},
	})
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	n := resp.Revision.Payload.Naming
	if n == nil || !n.Auto || n.AfterTurns != 2 || n.RepeatEvery != 3 || n.MaxRepetitions != 5 || n.MaxTitleLen != 100 || n.Model != "good-model" {
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
