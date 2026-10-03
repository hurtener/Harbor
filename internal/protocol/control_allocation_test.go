package protocol_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestAllocation_StartRejectsUnpricedMoneyAndChangedRetry(t *testing.T) {
	fx := newSurfaceFixture(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.WithVerified(t.Context(), id)
	a := &types.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000}
	req := &types.StartRequest{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}, IdempotencyKey: "key", InferenceAllocation: a}
	first, err := fx.surface.Dispatch(ctx, methods.MethodStart, req)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := fx.surface.Dispatch(ctx, methods.MethodStart, req)
	if err != nil || first.(*types.StartResponse).TaskID != replay.(*types.StartResponse).TaskID {
		t.Fatalf("replay %v", err)
	}
	a.Revision++
	if _, err = fx.surface.Dispatch(ctx, methods.MethodStart, req); err == nil {
		t.Fatal("changed revision reused task")
	}
	a.MaxCostMicroUSD = new(int64(100))
	req.IdempotencyKey = "cost"
	_, err = fx.surface.Dispatch(ctx, methods.MethodStart, req)
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeInferenceAllocationPricingUnavailable {
		t.Fatalf("pricing refusal %v", err)
	}
}
