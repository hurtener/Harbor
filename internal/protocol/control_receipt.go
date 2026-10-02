package protocol

import (
	"context"
	"errors"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

func inputReceiptProjection(r tasks.InputReceipt) types.ControlReceipt {
	return types.ControlReceipt{EventID: r.EventID, TaskID: string(r.TaskID), InputRevision: r.Revision, Status: string(r.Status), Reason: r.Reason, AcceptedAt: r.AcceptedAt, AppliedAt: r.AppliedAt, TerminalAt: r.TerminalAt}
}

func (s *ControlSurface) dispatchInput(ctx context.Context, q identity.Quadruple, scope steering.Scope, caller identity.Identity, cr *types.ControlRequest) (*types.ControlResponse, error) {
	message, err := steering.UserMessageText(cr.Payload)
	if err != nil {
		return nil, mapSteeringError(string(methods.MethodUserMessage), err)
	}
	taskCtx, err := identity.With(ctx, q.Identity)
	if err != nil {
		return nil, protoerrors.New(protoerrors.CodeScopeMismatch, "task input scope is unavailable")
	}
	var expected []uint64
	if cr.ExpectedInputRevision != nil {
		expected = []uint64{*cr.ExpectedInputRevision}
	}
	inbox, err := s.steering.Lookup(q)
	var receipt tasks.InputReceipt
	if errors.Is(err, steering.ErrInboxNotFound) {
		record, refusalErr := s.tasks.RefuseInput(taskCtx, tasks.TaskID(q.RunID), cr.EventID, message, "run_not_active", expected...)
		receipt, err = record.Receipt, refusalErr
	} else if err == nil {
		receipt, err = inbox.EnqueueInput(taskCtx, s.tasks, steering.ControlEvent{Type: steering.ControlUserMessage, Identity: q, CallerScope: scope, CallerTenant: caller.TenantID, Payload: cr.Payload, EventID: cr.EventID, ExpectedInputRevision: cr.ExpectedInputRevision})
	}
	if err != nil {
		return nil, mapInputError(methods.MethodUserMessage, err)
	}
	projected := inputReceiptProjection(receipt)
	return &types.ControlResponse{Accepted: receipt.Status == tasks.InputAccepted || receipt.Status == tasks.InputApplied, Method: string(methods.MethodUserMessage), Receipt: &projected, ProtocolVersion: types.ProtocolVersion}, nil
}

func (s *ControlSurface) dispatchControlReceipt(ctx context.Context, req any) (*types.ControlReceiptResponse, error) {
	r, ok := req.(*types.ControlReceiptRequest)
	if !ok || r == nil || r.EventID == "" || len(r.EventID) > 128 || strings.TrimSpace(r.EventID) != r.EventID {
		return nil, protoerrors.New(protoerrors.CodeInvalidRequest, "exact input event id is required")
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: r.Identity.Tenant, UserID: r.Identity.User, SessionID: r.Identity.Session}, RunID: r.Identity.Run}
	if err := identity.Validate(q.Identity); err != nil || q.RunID == "" {
		return nil, protoerrors.New(protoerrors.CodeIdentityRequired, "exact task identity is required")
	}
	caller, ok := identity.From(ctx)
	if !ok {
		return nil, protoerrors.New(protoerrors.CodeIdentityRequired, "verified caller identity is required")
	}
	scope, ok := deriveSteeringScope(ctx, caller, q)
	if !ok {
		return nil, protoerrors.New(protoerrors.CodeScopeMismatch, "task input receipt scope is unavailable")
	}
	if err := steering.CheckScope(steering.ControlUserMessage, scope, caller.TenantID, q); err != nil {
		return nil, mapSteeringError(string(methods.MethodControlReceipt), err)
	}
	taskCtx, err := identity.With(ctx, q.Identity)
	if err != nil {
		return nil, protoerrors.New(protoerrors.CodeScopeMismatch, "task input receipt scope is unavailable")
	}
	receipt, err := s.tasks.GetInputReceipt(taskCtx, tasks.TaskID(q.RunID), r.EventID)
	if err != nil {
		return nil, mapInputError(methods.MethodControlReceipt, err)
	}
	return &types.ControlReceiptResponse{Receipt: inputReceiptProjection(receipt), ProtocolVersion: types.ProtocolVersion}, nil
}

func mapInputError(method methods.Method, err error) *protoerrors.Error {
	if errors.Is(err, tasks.ErrInputRevisionConflict) {
		return protoerrors.New(protoerrors.CodeRevisionConflict, "accepted input revision changed; refresh the task before submitting new input")
	}
	if errors.Is(err, tasks.ErrIdempotencyConflict) {
		return protoerrors.New(protoerrors.CodeControlReceiptConflict, "input event id was reused with different text")
	}
	if errors.Is(err, tasks.ErrInputReceiptNotFound) {
		return protoerrors.New(protoerrors.CodeNotFound, "input receipt not found")
	}
	if errors.Is(err, tasks.ErrInputReceiptCapacity) {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "task input receipt capacity reached; prior event ids remain retrievable")
	}
	if errors.Is(err, steering.ErrPayloadInvalid) || errors.Is(err, steering.ErrScopeMismatch) || errors.Is(err, steering.ErrInboxNotFound) {
		return mapSteeringError(string(method), err)
	}
	return mapTaskError(string(method), err)
}
