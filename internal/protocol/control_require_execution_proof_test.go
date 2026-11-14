package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// D-513: require_execution_proof at the surface. The body-digest half of
// the proof is verified by the auth middleware (covered over HTTP in the
// integration suite); here the proof is seated the way the middleware
// seats it after a digest match.

var proofIdentity = identity.Identity{TenantID: "tenant-a", UserID: "owner", SessionID: "proof"}

const proofDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func proofSurface(t *testing.T, fx *surfaceFixture, required bool, ensurer protocol.SessionEnsurer) *protocol.ControlSurface {
	t.Helper()
	opts := []protocol.Option{
		protocol.WithAgentResolver(fixtureAgentResolver{}),
		protocol.WithRequireExecutionProof(required),
	}
	if ensurer != nil {
		opts = append(opts, protocol.WithSessionEnsurer(ensurer))
	}
	s, err := protocol.NewControlSurface(fx.tasks, fx.steering, opts...)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	return s
}

func proofCtx(t *testing.T, proof *auth.ExecutionStartProof, scopes ...auth.Scope) context.Context {
	t.Helper()
	ctx := auth.WithAgentReach(authCtx(t, proofIdentity, scopes...), []string{"fixture-default"})
	if proof != nil {
		ctx = auth.WithExecutionOperation(ctx, *proof)
	}
	return ctx
}

func proofStart(key string) *types.StartRequest {
	return &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: proofIdentity.TenantID, User: proofIdentity.UserID, Session: proofIdentity.SessionID},
		Query:          "answer",
		IdempotencyKey: key,
	}
}

// countingEnsurer records whether a start reached session materialisation.
type countingEnsurer struct{ calls *int }

func (e countingEnsurer) EnsureSession(context.Context, identity.Identity) error {
	*e.calls++
	return nil
}

func TestRequireExecutionProof_On(t *testing.T) {
	fx := newSurfaceFixture(t)
	var ensured int
	s := proofSurface(t, fx, true, countingEnsurer{calls: &ensured})

	// No proof: refused before the session is touched or a task exists.
	_, err := s.Dispatch(proofCtx(t, nil), methods.MethodStart, proofStart("k1"))
	if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("start without proof = %v, want scope_mismatch", err)
	}
	var pe *protoerrors.Error
	if !errors.As(err, &pe) || pe.Message != `method "start": start requires a signed execution proof` {
		t.Fatalf("refusal message = %v", err)
	}
	// An admin bearer without a proof is refused the same way.
	if got := codeOf(t, mustErr(s.Dispatch(proofCtx(t, nil, auth.ScopeAdmin), methods.MethodStart, proofStart("k1")))); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("admin start without proof code = %q, want scope_mismatch", got)
	}
	// A proof whose signed retry key names another start: refused.
	other := auth.ExecutionStartProof{OperationID: "op-1", IdempotencyKey: "another-key", BodySHA256: proofDigest}
	if got := codeOf(t, mustErr(s.Dispatch(proofCtx(t, &other), methods.MethodStart, proofStart("k1")))); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("mismatched idempotency key code = %q, want scope_mismatch", got)
	}
	if ensured != 0 {
		t.Fatalf("refused starts reached the session ensurer %d times, want 0", ensured)
	}
	list, err := fx.tasks.List(authCtx(t, proofIdentity), proofIdentity, tasks.TaskFilter{})
	if err != nil || len(list) != 0 {
		t.Fatalf("refused starts created tasks: %v %v", list, err)
	}

	// A valid proof: admitted, and the operation id reaches the task.
	proof := auth.ExecutionStartProof{OperationID: "op-1", IdempotencyKey: "k1", BodySHA256: proofDigest}
	resp, err := s.Dispatch(proofCtx(t, &proof), methods.MethodStart, proofStart("k1"))
	if err != nil {
		t.Fatalf("start with proof: %v", err)
	}
	task, err := fx.tasks.Get(authCtx(t, proofIdentity), tasks.TaskID(resp.(*types.StartResponse).TaskID))
	if err != nil || task.VerifiedExecutionOperationID != "op-1" {
		t.Fatalf("admitted task = %+v %v", task, err)
	}
	// Admin with a valid proof is admitted too (the proof, not the role, admits).
	adminProof := auth.ExecutionStartProof{OperationID: "op-2", IdempotencyKey: "k2", BodySHA256: proofDigest}
	if _, err := s.Dispatch(proofCtx(t, &adminProof, auth.ScopeAdmin), methods.MethodStart, proofStart("k2")); err != nil {
		t.Fatalf("admin start with proof: %v", err)
	}
}

func TestRequireExecutionProof_Off(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := proofSurface(t, fx, false, nil)
	if _, err := s.Dispatch(proofCtx(t, nil), methods.MethodStart, proofStart("k1")); err != nil {
		t.Fatalf("setting off: start without proof = %v, want admitted", err)
	}
	// The pre-existing key-match check still applies to a present proof.
	other := auth.ExecutionStartProof{OperationID: "op-1", IdempotencyKey: "another-key", BodySHA256: proofDigest}
	if got := codeOf(t, mustErr(s.Dispatch(proofCtx(t, &other), methods.MethodStart, proofStart("k2")))); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("setting off: mismatched key code = %q, want scope_mismatch", got)
	}
}

// Child tasks a run spawns go to the task registry directly, never through
// start, so the proof requirement does not reach them.
func TestRequireExecutionProof_ChildSpawnUnaffected(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := proofSurface(t, fx, true, nil)
	proof := auth.ExecutionStartProof{OperationID: "op-1", IdempotencyKey: "parent", BodySHA256: proofDigest}
	resp, err := s.Dispatch(proofCtx(t, &proof), methods.MethodStart, proofStart("parent"))
	if err != nil {
		t.Fatalf("parent start: %v", err)
	}
	parent := tasks.TaskID(resp.(*types.StartResponse).TaskID)
	childCtx, err := identity.WithRun(context.Background(), proofIdentity, string(parent))
	if err != nil {
		t.Fatal(err)
	}
	child, err := fx.tasks.Spawn(childCtx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: proofIdentity},
		Kind:         tasks.KindBackground,
		ParentTaskID: &parent,
		Query:        "child step",
	})
	if err != nil || child.ID == "" {
		t.Fatalf("internal child spawn = %+v %v", child, err)
	}
}

func mustErr(_ any, err error) error { return err }
