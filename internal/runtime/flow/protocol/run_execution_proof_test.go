package protocol_test

import (
	"context"
	"errors"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
)

// D-513: a runtime that requires a signed execution proof for every
// external start refuses flows.run, which spawns a run and cannot carry a
// proof. The admin scope is not an exemption.
func TestRun_RequiresExecutionProofRefusesAdmin(t *testing.T) {
	inv := &fakeInvoker{resp: prototypes.FlowRunResponse{RunID: "r1"}}
	s, err := flowprotocol.NewSurface(&fakeCatalog{}, inv, flowprotocol.WithRunRequiresExecutionProof(true))
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	_, err = s.Run(context.Background(), prototypes.FlowRunRequest{Identity: validScope(), FlowID: "f1"}, true)
	if !errors.Is(err, flowprotocol.ErrRunExecutionProofRequired) {
		t.Fatalf("Run(admin, proof required): err = %v, want ErrRunExecutionProofRequired", err)
	}
	if inv.called {
		t.Fatal("Run: invoker called although runs require a signed execution proof")
	}
}

func TestRun_ExecutionProofOffInvokes(t *testing.T) {
	inv := &fakeInvoker{resp: prototypes.FlowRunResponse{RunID: "r1"}}
	s, err := flowprotocol.NewSurface(&fakeCatalog{}, inv, flowprotocol.WithRunRequiresExecutionProof(false))
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	if _, err := s.Run(context.Background(), prototypes.FlowRunRequest{Identity: validScope(), FlowID: "f1"}, true); err != nil || !inv.called {
		t.Fatalf("Run(admin, proof not required) = %v called=%v", err, inv.called)
	}
}
