package protocol_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

type tamperedRetirementRegistry struct {
	agentcfg.Registry
	completeCalls atomic.Int64
}

func (r *tamperedRetirementRegistry) Retire(_ context.Context, _ identity.Quadruple, _ string, req agentcfg.RetirementRequest) (agentcfg.RetirementStatus, error) {
	return agentcfg.RetirementStatus{OperationID: req.OperationID, Cleanup: []agentcfg.CleanupStep{{Class: "mcp_connection", Resource: "altered-resource"}}}, nil
}

func (r *tamperedRetirementRegistry) RetirementStatus(context.Context, identity.Quadruple, string) (agentcfg.RetirementStatus, bool, error) {
	return agentcfg.RetirementStatus{}, true, agentcfg.ErrRetirementConflict
}

func (r *tamperedRetirementRegistry) CompleteRetirementStep(context.Context, identity.Quadruple, string, string, string, string) (agentcfg.RetirementStatus, error) {
	r.completeCalls.Add(1)
	return agentcfg.RetirementStatus{}, nil
}

type retirementCountingDetacher struct {
	calls atomic.Int64
}

func (d *retirementCountingDetacher) DetachConnection(context.Context, string, string, string) error {
	d.calls.Add(1)
	return nil
}

func TestRetire_TamperedStatusFailsBeforeExternalTeardown(t *testing.T) {
	reg := &tamperedRetirementRegistry{Registry: newRegistry(t)}
	detacher := &retirementCountingDetacher{}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithConnectionDetacher(detacher))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Retire(context.Background(), prototypes.AgentConfigRetireRequest{
		Identity:            scope(),
		AgentID:             testAgentID,
		OperationID:         "tampered-operation",
		ExpectedContentHash: agentcfg.ExpectNoActiveRevision,
	})
	if !errors.Is(err, agentcfg.ErrRetirementConflict) {
		t.Fatalf("Retire error=%v, want tamper conflict", err)
	}
	if detacher.calls.Load() != 0 || reg.completeCalls.Load() != 0 {
		t.Fatalf("tampered status caused side effects: detach=%d complete=%d", detacher.calls.Load(), reg.completeCalls.Load())
	}
}
