package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
)

var (
	// ErrContextReconcileUnsupported means retained context is not enabled.
	ErrContextReconcileUnsupported = errors.New("sessions/protocol: context reconciliation unsupported")
	// ErrContextUnsettled refuses to infer the result of a pending operation.
	ErrContextUnsettled = errors.New("sessions/protocol: external operation outcome unknown")
	// ErrContextUnavailable covers expired, erased, missing or invalid evidence.
	ErrContextUnavailable = errors.New("sessions/protocol: retained context unavailable")
)

// ContextReconciler is the runtime-owned durable reconciliation seam. The
// implementation must scope all reads/writes to id, fence the source admission,
// and refuse pending operations; it must never dispatch an historical action.
type ContextReconciler interface {
	ReconcileContext(context.Context, identity.Identity, string) error
}

// WithContextReconciler enables the explicit own-session recovery operation.
// A nil implementation leaves it unavailable, without enabling retention.
func WithContextReconciler(r ContextReconciler) Option {
	return func(s *Service) { s.contextReconciler = r }
}

// ReconcileContext seals committed evidence only for the verified identity.
// There is deliberately no admin override or independently selected session.
func (s *Service) ReconcileContext(ctx context.Context, req prototypes.SessionsReconcileContextRequest) (prototypes.SessionsReconcileContextResponse, error) {
	if err := sessionadmission.CheckMethod(ctx, methods.MethodSessionsReconcileContext); err != nil {
		return prototypes.SessionsReconcileContextResponse{}, err
	}
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsReconcileContextResponse{}, err
	}
	return sessionadmission.Run(ctx, id, methods.MethodSessionsReconcileContext, func(accepted context.Context) (prototypes.SessionsReconcileContextResponse, error) {
		out, err := s.reconcileContextAccepted(accepted, req)
		if errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrIdentityRequired) || errors.Is(err, ErrContextReconcileUnsupported) {
			err = sessionadmission.Rejected(err)
		}
		return out, err
	})
}

func (s *Service) reconcileContextAccepted(ctx context.Context, req prototypes.SessionsReconcileContextRequest) (prototypes.SessionsReconcileContextResponse, error) {
	var out prototypes.SessionsReconcileContextResponse
	if err := ctx.Err(); err != nil {
		return out, err
	}
	id, err := validIdentity(req.Identity)
	if err != nil {
		return out, err
	}
	run := req.SourceRunID
	if run == "" || len(run) > 256 || !utf8.ValidString(run) || strings.TrimSpace(run) != run || strings.ContainsFunc(run, unicode.IsControl) {
		return out, ErrInvalidRequest
	}
	if s.contextReconciler == nil {
		return out, ErrContextReconcileUnsupported
	}
	if err := s.contextReconciler.ReconcileContext(ctx, id, run); err != nil {
		return out, fmt.Errorf("sessions: reconcile retained context: %w", err)
	}
	return prototypes.SessionsReconcileContextResponse{SessionID: id.SessionID, SourceRunID: run, Reconciled: true}, nil
}
