package protocol

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/transfer"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func (s *ArtifactsSurface) handleTransfer(ctx context.Context, m methods.Method, in any) (any, error) {
	if s.transfer == nil {
		return nil, protoerrors.New(protoerrors.CodeUnknownMethod, "artifact transfer is not configured")
	}
	var result types.ArtifactTransferReceipt
	var err error
	switch m {
	case methods.MethodArtifactsPrepareImport, methods.MethodArtifactsTransfer:
		r, ok := in.(*types.ArtifactsTransferRequest)
		if !ok || r == nil {
			return nil, protoerrors.New(protoerrors.CodeInvalidRequest, "invalid transfer request")
		}
		if err := validateTransferOwnerScope(ctx, r.Scope); err != nil {
			return nil, err
		}
		if m == methods.MethodArtifactsPrepareImport {
			result, err = s.transfer.Prepare(ctx, r.Grant)
		} else {
			result, err = s.transfer.Transfer(ctx, r.Grant)
		}
	case methods.MethodArtifactsTransferStatus, methods.MethodArtifactsRevokeTransfer:
		r, ok := in.(*types.ArtifactsTransferStatusRequest)
		if !ok || r == nil {
			return nil, protoerrors.New(protoerrors.CodeInvalidRequest, "invalid transfer status request")
		}
		if err := validateTransferOwnerScope(ctx, r.Scope); err != nil {
			return nil, err
		}
		if m == methods.MethodArtifactsTransferStatus {
			result, err = s.transfer.Status(ctx, r.Direction, r.TransferID)
		} else {
			result, err = s.transfer.Revoke(ctx, r.Direction, r.TransferID)
		}
	}
	if err == nil {
		return &result, nil
	}
	code := protoerrors.CodeRuntimeError
	switch {
	case errors.Is(err, transfer.ErrUnauthorized):
		code = protoerrors.CodeScopeMismatch
	case errors.Is(err, transfer.ErrInvalid):
		code = protoerrors.CodeInvalidRequest
	case errors.Is(err, transfer.ErrNotFound):
		code = protoerrors.CodeNotFound
	case errors.Is(err, transfer.ErrConflict):
		code = protoerrors.CodeArtifactTransferConflict
	case errors.Is(err, transfer.ErrExpired):
		code = protoerrors.CodeArtifactTransferExpired
	case errors.Is(err, transfer.ErrRevoked), errors.Is(err, artifacts.ErrScopeFenced):
		code = protoerrors.CodeArtifactTransferRevoked
	case errors.Is(err, transfer.ErrTooLate):
		code = protoerrors.CodeArtifactTransferInProgress
	}
	return nil, protoerrors.New(code, "artifact transfer refused or unconfirmed; inspect the exact transfer receipt")
}

func validateTransferOwnerScope(ctx context.Context, scope types.ArtifactScope) error {
	_, err := bodyscope.Reconcile(ctx, bodyscope.ForArtifactScope(&scope), bodyscope.SurfaceArtifactsRef, nil)
	if err != nil {
		return err
	}
	return nil
}
