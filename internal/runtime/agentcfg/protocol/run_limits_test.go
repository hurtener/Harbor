package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func i64(v int64) *int64 { return &v }

func TestRunLimitsRevision_ValidatesRoundTripsAndDiffs(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []prototypes.AgentConfigRunLimits{
		{MaxDurationSeconds: i64(0)}, {MaxDurationSeconds: i64(-5)}, {MaxDurationSeconds: i64(31 * 24 * 3600)}, {MaxTokens: i64(0)},
	} {
		s, err := agentcfgprotocol.NewService(newRegistry(t))
		if err != nil {
			t.Fatal(err)
		}
		limits := bad
		_, err = s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID,
			Payload: prototypes.AgentConfigPayload{RunLimits: &limits}})
		if !errors.Is(err, agentcfgprotocol.ErrInvalidRunLimits) {
			t.Fatalf("%+v accepted: %v", bad, err)
		}
	}
	s, err := agentcfgprotocol.NewService(newRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{RunLimits: &prototypes.AgentConfigRunLimits{MaxDurationSeconds: i64(1200)}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || got.Revision.Payload.RunLimits == nil || *got.Revision.Payload.RunLimits.MaxDurationSeconds != 1200 || got.Revision.Payload.RunLimits.MaxTokens != nil {
		t.Fatalf("round trip lost the section: %+v %v", got.Revision, err)
	}
	// A narrow sibling edit preserves the run-limit section.
	model := "m"
	if _, err := s.SetLLMParams(ctx, prototypes.AgentConfigSetLLMParamsRequest{Identity: scope(), AgentID: testAgentID,
		LLMParams: prototypes.AgentConfigLLMParams{Model: &model}}); err == nil {
		got, _ = s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
		if got.Revision.Payload.RunLimits == nil {
			t.Fatal("sibling edit dropped run_limits")
		}
	}
	second, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{RunLimits: &prototypes.AgentConfigRunLimits{MaxTokens: i64(500000)}}})
	if err != nil {
		t.Fatal(err)
	}
	diff := agentcfg.DiffRunLimits(
		agentcfg.ConfigPayload{RunLimits: &agentcfg.RunLimitsSection{MaxDurationSeconds: i64(1200)}},
		agentcfg.ConfigPayload{RunLimits: &agentcfg.RunLimitsSection{MaxTokens: i64(500000)}})
	if !diff.MaxDurationChanged || diff.MaxDurationFrom != "1200" || diff.MaxDurationTo != "none" || diff.MaxTokensTo != "500000" {
		t.Fatalf("diff = %+v", diff)
	}
	wire, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{Identity: scope(), AgentID: testAgentID,
		FromRevision: first.Revision.RevisionID, ToRevision: second.Revision.RevisionID})
	if err != nil || wire.Diff.RunLimits == nil || !wire.Diff.RunLimits.MaxTokensChanged {
		t.Fatalf("wire diff = %+v %v", wire.Diff.RunLimits, err)
	}
	same, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{Identity: scope(), AgentID: testAgentID,
		FromRevision: second.Revision.RevisionID, ToRevision: second.Revision.RevisionID})
	if err != nil || same.Diff.RunLimits != nil {
		t.Fatalf("unchanged run limits must be omitted from the wire diff: %+v %v", same.Diff.RunLimits, err)
	}
}
