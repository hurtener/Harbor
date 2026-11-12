package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestStartRunLimits_AcceptedValidatedAndInIdempotencyIdentity(t *testing.T) {
	fx := newSurfaceFixture(t)
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "limits"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	scope := types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	req := &types.StartRequest{Identity: scope, IdempotencyKey: "limited", RunLimits: &types.RunLimitsRequest{MaxDurationSeconds: 600, MaxTokens: 50000}}
	resp, err := fx.surface.Dispatch(ctx, methods.MethodStart, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fx.tasks.Get(ctx, tasks.TaskID(resp.(*types.StartResponse).TaskID))
	if err != nil || got.RunLimits == nil || got.RunLimits.MaxDurationSeconds != 600 || got.RunLimits.MaxTokens != 50000 {
		t.Fatalf("accepted task lost run limits: %+v %v", got.RunLimits, err)
	}
	changed := &types.StartRequest{Identity: scope, IdempotencyKey: "limited", RunLimits: &types.RunLimitsRequest{MaxDurationSeconds: 60}}
	if _, err := fx.surface.Dispatch(ctx, methods.MethodStart, changed); err == nil {
		t.Fatal("changed run limits reused the original idempotency key")
	}
	for _, bad := range []types.RunLimitsRequest{{MaxDurationSeconds: -1}, {MaxTokens: -1}, {MaxDurationSeconds: 31 * 24 * 3600}} {
		limits := bad
		if _, err := fx.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: scope, RunLimits: &limits}); err == nil {
			t.Fatalf("invalid run limits %+v accepted", bad)
		}
	}
}
