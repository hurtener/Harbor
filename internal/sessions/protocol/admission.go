package protocol

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
)

// SetAdmission enrolls or advances authority using the runtime's durable gate.
func (s *Service) SetAdmission(ctx context.Context, req prototypes.SessionsSetAdmissionRequest) (prototypes.SessionsSetAdmissionResponse, error) {
	if err := sessionadmission.CheckMethod(ctx, methods.MethodSessionsSetAdmission); err != nil {
		return prototypes.SessionsSetAdmissionResponse{}, err
	}
	if err := auth.AuthorizeMethod(ctx, methods.MethodSessionsSetAdmission); err != nil {
		return prototypes.SessionsSetAdmissionResponse{}, sessionadmission.ProtocolError(err)
	}
	id, err := validIdentity(req.Identity)
	if err != nil {
		return prototypes.SessionsSetAdmissionResponse{}, err
	}
	gate, ok := sessionadmission.From(ctx)
	if !ok {
		return prototypes.SessionsSetAdmissionResponse{}, sessionadmission.ProtocolError(sessionadmission.ErrUnavailable)
	}
	policy, err := gate.Enroll(ctx, id, req.ExpectedEpoch, req.Epoch)
	if err != nil {
		return prototypes.SessionsSetAdmissionResponse{}, sessionadmission.ProtocolError(err)
	}
	s.emitAdminAudit(ctx, id, string(methods.MethodSessionsSetAdmission))
	return prototypes.SessionsSetAdmissionResponse{Epoch: policy.Epoch, ProtocolVersion: prototypes.ProtocolVersion}, nil
}
