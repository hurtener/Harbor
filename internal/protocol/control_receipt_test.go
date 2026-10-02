package protocol_test

import (
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestDispatch_InputReceipt_ExactTaskReplayLookupAndTerminal(t *testing.T) {
	fx := newSurfaceFixture(t)
	q := testRun("")
	ctx := authCtx(t, q.Identity)
	h, err := fx.tasks.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	q.RunID = string(h.ID)
	if err := fx.tasks.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	in, err := fx.steering.Open(q)
	if err != nil {
		t.Fatal(err)
	}
	scope := types.IdentityScope{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID}
	request := &types.ControlRequest{Identity: scope, EventID: "request-id", Payload: map[string]any{"message": "exact correction"}}
	for range 100 {
		resp, err := fx.surface.Dispatch(ctx, methods.MethodUserMessage, request)
		if err != nil {
			t.Fatal(err)
		}
		got := resp.(*types.ControlResponse)
		if !got.Accepted || got.Receipt == nil || got.Receipt.InputRevision != 1 || got.Receipt.Status != "accepted" {
			t.Fatalf("admission=%+v", got)
		}
	}
	if in.Len() != 1 {
		t.Fatalf("duplicate deliveries=%d", in.Len())
	}
	read := &types.ControlReceiptRequest{Identity: scope, EventID: request.EventID}
	response, err := fx.surface.Dispatch(ctx, methods.MethodControlReceipt, read)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.(*types.ControlReceiptResponse); got.Receipt.Status != "accepted" || got.Receipt.TaskID != q.RunID {
		t.Fatalf("lookup=%+v", got)
	}
	request.Payload = map[string]any{"message": "different correction"}
	if _, err := fx.surface.Dispatch(ctx, methods.MethodUserMessage, request); codeOf(t, err) != protoerrors.CodeControlReceiptConflict {
		t.Fatalf("conflict=%v", err)
	}
	request.Payload = map[string]any{"message": "exact correction", "attachment": "not supported"}
	if _, err := fx.surface.Dispatch(ctx, methods.MethodUserMessage, request); codeOf(t, err) != protoerrors.CodePayloadInvalid {
		t.Fatalf("unsupported shape=%v", err)
	}
	foreign := q.Identity
	foreign.UserID = "other-user"
	if _, err := fx.surface.Dispatch(authCtx(t, foreign), methods.MethodControlReceipt, read); codeOf(t, err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("foreign lookup=%v", err)
	}
	if _, err := fx.tasks.MarkInputApplied(ctx, h.ID, "request-id", 1); err != nil {
		t.Fatal(err)
	}
	if err := fx.tasks.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"done"`), IncorporatedInputRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fx.steering.Retire(q); err != nil {
		t.Fatal(err)
	}
	response, err = fx.surface.Dispatch(ctx, methods.MethodControlReceipt, read)
	if err != nil || response.(*types.ControlReceiptResponse).Receipt.Status != "applied" {
		t.Fatalf("terminal lookup=%+v err=%v", response, err)
	}
	request.EventID = "late"
	request.Payload = map[string]any{"message": "late correction"}
	response, err = fx.surface.Dispatch(ctx, methods.MethodUserMessage, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.(*types.ControlResponse); got.Accepted || got.Receipt.Status != "terminal" || got.Receipt.InputRevision != 0 {
		t.Fatalf("late=%+v", got)
	}
}

func TestDispatch_InputReceipt_ExpectedRevisionAdmission(t *testing.T) {
	fx := newSurfaceFixture(t)
	q := testRun("")
	ctx := authCtx(t, q.Identity)
	h, err := fx.tasks.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	q.RunID = string(h.ID)
	if err = fx.tasks.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	inbox, err := fx.steering.Open(q)
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	req := &types.ControlRequest{Identity: types.IdentityScope{Tenant: q.TenantID, User: q.UserID, Session: q.SessionID, Run: q.RunID}, EventID: "first", ExpectedInputRevision: &zero, Payload: map[string]any{"message": "first correction"}}
	response, err := fx.surface.Dispatch(ctx, methods.MethodUserMessage, req)
	if err != nil {
		t.Fatal(err)
	}
	first := response.(*types.ControlResponse)
	req.EventID = "stale"
	if _, err = fx.surface.Dispatch(ctx, methods.MethodUserMessage, req); codeOf(t, err) != protoerrors.CodeRevisionConflict {
		t.Fatalf("stale intent=%v", err)
	}
	if inbox.Len() != 1 {
		t.Fatalf("stale admission interrupted inbox: %d", inbox.Len())
	}
	req.EventID = "first"
	response, err = fx.surface.Dispatch(ctx, methods.MethodUserMessage, req)
	if err != nil || *response.(*types.ControlResponse).Receipt != *first.Receipt {
		t.Fatalf("exact replay=%v %v", response, err)
	}
	req.EventID = ""
	if _, err = fx.surface.Dispatch(ctx, methods.MethodUserMessage, req); codeOf(t, err) != protoerrors.CodeInvalidRequest {
		t.Fatalf("unkeyed precondition=%v", err)
	}
}
