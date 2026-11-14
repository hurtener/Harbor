package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// A flagged start while the session has an active run answers
// session_running and creates nothing; the exact retry of the accepted start
// still answers its task (the flag is outside the idempotency identity).
func TestStartRequireIdleSession_RefusesBesideAnActiveRun(t *testing.T) {
	fx := newSurfaceFixture(t)
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "idle"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	scope := types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	first, err := fx.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: scope, IdempotencyKey: "one", RequireIdleSession: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = fx.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: scope, IdempotencyKey: "two", RequireIdleSession: true})
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Code != protoerrors.CodeSessionRunning {
		t.Fatalf("second start = %v, want session_running", err)
	}
	retry, err := fx.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{Identity: scope, IdempotencyKey: "one"})
	if err != nil || retry.(*types.StartResponse).TaskID != first.(*types.StartResponse).TaskID || !retry.(*types.StartResponse).Reused {
		t.Fatalf("retry without the flag = %+v %v, want the same task reused", retry, err)
	}
}
