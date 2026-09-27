package protocol_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestStartBindsVerifiedOperationBeforeTaskAssociation(t *testing.T) {
	f := newCallerMemoryFixture(t)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "thread"}
	ctx := auth.WithExecutionOperation(auth.WithAgentReach(authCtx(t, id), []string{"caller-memory-default"}), auth.ExecutionStartProof{OperationID: "admitted-operation", IdempotencyKey: "start-key", BodySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	request := &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, Query: "answer", IdempotencyKey: "start-key"}
	resp, err := f.surface.Dispatch(ctx, methods.MethodStart, request)
	if err != nil {
		t.Fatal(err)
	}
	started := resp.(*types.StartResponse)
	task, err := f.tasks.Get(authCtx(t, id), tasks.TaskID(started.TaskID))
	if err != nil || task.VerifiedExecutionOperationID != "admitted-operation" {
		t.Fatalf("task proof: %+v %v", task, err)
	}
	request.IdempotencyKey = "tampered-tool-argument"
	if _, err := f.surface.Dispatch(ctx, methods.MethodStart, request); err == nil {
		t.Fatal("signed operation mismatch created a task")
	}
}
