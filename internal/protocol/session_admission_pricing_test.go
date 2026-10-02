package protocol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/tasks"
)

// A rejected pricing declaration cannot leave unresolved mutation liability:
// validation rejects it before the task registry can accept a task.
func TestAdmissionPricingRefusalDoesNotPoisonSession(t *testing.T) {
	for _, validShape := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing_manifest", true: "uninstalled_manifest"}[validShape], func(t *testing.T) {
			f := newAdmissionProtocolFixture(t)
			id := identity.Identity{TenantID: "adversarial-t", UserID: "adversarial-u", SessionID: "adversarial-s"}
			admin := f.verifiedContext(t, f.token(t, id, nil, 0))
			if _, err := f.gate.Enroll(admin, id, 0, 1); err != nil {
				t.Fatal(err)
			}
			ctx := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 1))
			allocation := &types.InferenceAllocation{AllocationID: "fund", Revision: 1, MaxTotalTokens: 1000, MaxCostMicroUSD: new(int64(100))}
			if validShape {
				allocation.PricingManifestID = "uninstalled"
				allocation.PricingManifestRevision = 1
				allocation.PricingManifestSHA256 = strings.Repeat("a", 64)
			}
			request := &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, IdempotencyKey: "invalid-pricing", InferenceAllocation: allocation}
			_, err := f.surface.Dispatch(ctx, methods.MethodStart, request)
			var perr *protoerrors.Error
			if !errors.As(err, &perr) || perr.Code != protoerrors.CodeInferenceAllocationPricingUnavailable {
				t.Fatalf("pricing refusal = %v", err)
			}
			request.IdempotencyKey = "corrected-request"
			request.InferenceAllocation = nil
			if _, err := f.surface.Dispatch(ctx, methods.MethodStart, request); err != nil {
				t.Fatalf("proven pre-effect pricing rejection permanently blocked corrected start: %v", err)
			}
		})
	}
}

// A custom registry can report the same public pricing error after accepting
// work. Only the native registry's explicit pre-acceptance proof can release
// the reservation; recognizing the protocol code alone would lose uncertainty.
type pricingErrorAfterAcceptedSpawn struct {
	tasks.TaskRegistry
	joinedProof bool
	accepted    int
}

func (r *pricingErrorAfterAcceptedSpawn) Spawn(ctx context.Context, req tasks.SpawnRequest) (tasks.TaskHandle, error) {
	if _, err := r.TaskRegistry.Spawn(ctx, req); err != nil {
		return tasks.TaskHandle{}, err
	}
	r.accepted++
	if r.joinedProof {
		return tasks.TaskHandle{}, errors.Join(tasks.RejectBeforeSpawn(llm.ErrAllocationPricingUnavailable), errors.New("synthetic uncertain completion"))
	}
	return tasks.TaskHandle{}, llm.ErrAllocationPricingUnavailable
}

func TestAdmissionPricingErrorAfterAcceptanceStaysReserved(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "code_without_proof", true: "proof_joined_with_unknown"}[joined], func(t *testing.T) {
			f := newAdmissionProtocolFixture(t)
			registry := &pricingErrorAfterAcceptedSpawn{TaskRegistry: f.tasks, joinedProof: joined}
			surface, err := protocol.NewControlSurface(registry, f.steering,
				protocol.WithAgentResolver(fixtureAgentResolver{}),
				protocol.WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
				protocol.WithSessionAdmissionGate(f.gate))
			if err != nil {
				t.Fatal(err)
			}
			id := identity.Identity{TenantID: "pricing-t", UserID: "pricing-u", SessionID: "pricing-s"}
			admin := f.verifiedContext(t, f.token(t, id, nil, 0))
			if _, err := f.gate.Enroll(admin, id, 0, 1); err != nil {
				t.Fatal(err)
			}
			ctx := f.verifiedContext(t, f.token(t, id, []methods.Method{methods.MethodStart}, 1))
			req := &types.StartRequest{Identity: types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}, IdempotencyKey: "accepted-then-error"}
			_, err = surface.Dispatch(ctx, methods.MethodStart, req)
			var perr *protoerrors.Error
			if registry.accepted != 1 || !errors.As(err, &perr) || perr.Code != protoerrors.CodeInferenceAllocationPricingUnavailable {
				t.Fatal("fixture did not accept exactly one task before its error", err)
			}
			req.IdempotencyKey = "must-not-start"
			if _, err := surface.Dispatch(ctx, methods.MethodStart, req); err == nil || registry.accepted != 1 {
				t.Fatal("pricing code erased uncertain accepted work", err)
			}
			if _, err := f.gate.Enroll(admin, id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
				t.Fatal("uncertain accepted work allowed epoch replacement", err)
			}
		})
	}
}
