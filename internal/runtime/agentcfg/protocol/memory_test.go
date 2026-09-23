package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func TestMemoryBudgetRevision(t *testing.T) {
	for _, available := range []bool{false, true} {
		for _, budget := range []int{-1, 0, 64000, 1000000} {
			s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithMemoryBudget(available))
			if err != nil {
				t.Fatal(err)
			}
			req := prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID,
				Payload: prototypes.AgentConfigPayload{Memory: &prototypes.AgentConfigMemory{BudgetTokens: budget}}}
			_, err = s.SetRevision(context.Background(), req)
			if !available || budget < 0 {
				if !errors.Is(err, agentcfgprotocol.ErrInvalidMemory) {
					t.Fatalf("available=%v budget=%d: %v", available, budget, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if err != nil {
				t.Fatal(err)
			}
			if !available || budget < 0 {
				if got.Set {
					t.Fatal("invalid budget persisted")
				}
				continue
			}
			if !got.Set || got.Revision.Payload.Memory == nil || got.Revision.Payload.Memory.BudgetTokens != budget {
				t.Fatalf("budget did not round trip: %+v", got)
			}
			req.Payload.Memory = nil
			if _, err = s.SetRevision(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			got, err = s.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if err != nil || got.Revision.Payload.Memory != nil {
				t.Fatalf("inherit reset: %+v %v", got, err)
			}
		}
	}
}

func TestMemoryBudgetDiffRollbackAndCAS(t *testing.T) {
	ctx := t.Context()
	s, err := agentcfgprotocol.NewService(newRegistry(t), agentcfgprotocol.WithMemoryBudget(true))
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID}
	initial, err := s.SetRevision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.ExpectedContentHash = initial.Revision.ContentHash
	req.Payload.Memory = &prototypes.AgentConfigMemory{BudgetTokens: 64000}
	set, err := s.SetRevision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Payload.Memory.BudgetTokens = 32000
	if _, err := s.SetRevision(ctx, req); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	diff, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{Identity: scope(), AgentID: testAgentID,
		FromRevision: initial.Revision.RevisionID, ToRevision: set.Revision.RevisionID})
	if err != nil {
		t.Fatal(err)
	}
	if !diff.Diff.Memory.BudgetTokensChanged || diff.Diff.Memory.BudgetTokensFrom != "" || diff.Diff.Memory.BudgetTokensTo != "64000" {
		t.Fatalf("diff: %+v", diff.Diff.Memory)
	}
	rolled, err := s.Rollback(ctx, prototypes.AgentConfigRollbackRequest{Identity: scope(), AgentID: testAgentID,
		RevisionID: initial.Revision.RevisionID, ExpectedContentHash: set.Revision.ContentHash})
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Revision.Payload.Memory != nil {
		t.Fatal("rollback did not restore YAML inheritance")
	}
}
