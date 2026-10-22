package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestStartRunToolAllowlistIsDurableAndCannotChangeOnReplay(t *testing.T) {
	fx := newSurfaceFixture(t)
	zero := []string{}
	req := &types.StartRequest{Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"}, Query: "Discuss the document", IdempotencyKey: "read-only", ToolAllowlist: &zero}
	first, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, req)
	if err != nil {
		t.Fatal(err)
	}
	accepted := first.(*types.StartResponse)
	if accepted.ToolAllowlistApplied == nil || !*accepted.ToolAllowlistApplied {
		t.Fatalf("run restriction was not acknowledged: %+v", accepted)
	}
	zero = append(zero, "write_tool")
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := fx.tasks.Get(ctx, tasks.TaskID(accepted.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolAllowlist == nil || len(*got.ToolAllowlist) != 0 {
		t.Fatalf("accepted task changed with request slice: %+v", got.ToolAllowlist)
	}
	if _, err = fx.surface.Dispatch(context.Background(), methods.MethodStart, req); err == nil {
		t.Fatal("changed policy reused idempotency key")
	}
	zero = []string{}
	replay, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, req)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.(*types.StartResponse).Reused || replay.(*types.StartResponse).TaskID != accepted.TaskID {
		t.Fatal("exact restricted replay did not reuse")
	}
	for _, invalid := range [][]string{{"tool", "tool"}, {"tool name"}} {
		req.ToolAllowlist = &invalid
		req.IdempotencyKey = "invalid"
		if _, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, req); codeOf(t, err) != protoerrors.CodeInvalidRequest {
			t.Fatalf("invalid tool policy: %v", err)
		}
	}
}
