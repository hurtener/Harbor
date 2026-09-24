package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/catalog"
)

type supersedingApprovalPolicy struct{ invalidated chan struct{} }

func (p supersedingApprovalPolicy) ShouldApprove(context.Context, *approval.ApprovalRequest) (bool, string, error) {
	close(p.invalidated)
	return false, "", nil
}

func TestApprovalWrapper_RechecksInvocationAfterGateReturns(t *testing.T) {
	_, coord, bus, red := buildCatalogEnv(t)
	signal := make(chan struct{})
	gate, err := approval.NewApprovalGate(approval.GateDeps{Policy: supersedingApprovalPolicy{invalidated: signal}, Coordinator: coord, Bus: bus, Redactor: red, Authorizer: approval.NewIdentityAuthorizer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close(context.Background()) })
	calls := 0
	desc := catalog.WrapWithApproval(tools.ToolDescriptor{Tool: tools.Tool{Name: "obsolete"}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls++
		return tools.ToolResult{}, nil
	}}, gate, catalog.ApprovalWrapperOptions{})
	_, err = desc.Invoke(tools.WithInvocationFence(ctxWithID(t, catalogTestID), signal), json.RawMessage(`{}`))
	if !errors.Is(err, tools.ErrInvocationSuperseded) || calls != 0 {
		t.Fatalf("gate return raced into obsolete invocation: calls=%d err=%v", calls, err)
	}
}
