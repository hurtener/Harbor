package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// recordingEnsurer stands in for the session registry adapter: it records
// the participant the start carried and refuses a guest when the session
// is "missing".
type recordingEnsurer struct {
	missing bool
}

func (e recordingEnsurer) EnsureSession(ctx context.Context, id identity.Identity) error {
	if p, ok := identity.ParticipantFrom(ctx); ok && p.IsGuest(id) && e.missing {
		return protocol.ErrSessionParticipantNoSession
	}
	return nil
}

var sharedSession = identity.Identity{TenantID: "tenant-a", UserID: "owner", SessionID: "shared"}

func participantCtx(t *testing.T, writer, speaker string) context.Context {
	t.Helper()
	ctx := authCtx(t, sharedSession)
	ctx, err := identity.WithParticipant(ctx, identity.Participant{UserID: writer, Speaker: speaker})
	if err != nil {
		t.Fatalf("WithParticipant: %v", err)
	}
	return ctx
}

func participantSurface(t *testing.T, fx *surfaceFixture, ensurer protocol.SessionEnsurer) *protocol.ControlSurface {
	t.Helper()
	opts := []protocol.Option{
		protocol.WithAgentResolver(fixtureAgentResolver{}),
		protocol.WithAgentReachAuthorizer(fixtureReachAuthorizer{}),
	}
	if ensurer != nil {
		opts = append(opts, protocol.WithSessionEnsurer(ensurer))
	}
	surface, err := protocol.NewControlSurface(fx.tasks, fx.steering, opts...)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	return surface
}

func startShared(ctx context.Context, s *protocol.ControlSurface, key string) (*types.StartResponse, error) {
	resp, err := s.Dispatch(ctx, methods.MethodStart, &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: sharedSession.TenantID, User: sharedSession.UserID, Session: sharedSession.SessionID},
		Query:          "hello",
		IdempotencyKey: key,
	})
	if err != nil {
		return nil, err
	}
	return resp.(*types.StartResponse), nil
}

func TestDispatchStart_Participant_CarriesWriterOnTask(t *testing.T) {
	fx := newSurfaceFixture(t)
	surface := participantSurface(t, fx, recordingEnsurer{})
	resp, err := startShared(participantCtx(t, "writer-b", "Bruno"), surface, "k1")
	if err != nil {
		t.Fatalf("participant start: %v", err)
	}
	task, err := fx.tasks.Get(authCtx(t, sharedSession), tasks.TaskID(resp.TaskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.Identity.UserID != "owner" {
		t.Fatalf("task partition user = %q, want the session owner", task.Identity.UserID)
	}
	if task.Participant == nil || task.Participant.UserID != "writer-b" || task.Participant.Speaker != "Bruno" {
		t.Fatalf("task participant = %+v, want writer-b/Bruno", task.Participant)
	}

	// The same idempotency key from another writer is a conflict, never a
	// silent adoption of writer-b's turn.
	if _, err := startShared(participantCtx(t, "writer-c", "Carla"), surface, "k1"); err == nil {
		t.Fatal("a different writer reusing an idempotency key must conflict")
	}

	// An ordinary owner start carries no participant.
	own, err := startShared(authCtx(t, sharedSession), surface, "k2")
	if err != nil {
		t.Fatalf("owner start: %v", err)
	}
	ownTask, err := fx.tasks.Get(authCtx(t, sharedSession), tasks.TaskID(own.TaskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ownTask.Participant != nil {
		t.Fatalf("owner task participant = %+v, want nil", ownTask.Participant)
	}
}

func TestDispatchStart_Participant_NeverCreatesASession(t *testing.T) {
	fx := newSurfaceFixture(t)
	if _, err := startShared(participantCtx(t, "writer-b", ""), participantSurface(t, fx, recordingEnsurer{missing: true}), "k"); codeOf(t, err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("missing session code = %q, want scope_mismatch", codeOf(t, err))
	}
	// A surface without a session ensurer cannot prove existence: fail closed.
	if _, err := startShared(participantCtx(t, "writer-b", ""), participantSurface(t, fx, nil), "k2"); codeOf(t, err) != protoerrors.CodeScopeMismatch {
		t.Fatalf("no-ensurer code = %q, want scope_mismatch", codeOf(t, err))
	}
}

// A run belongs to the person who wrote it: the owner cannot steer a
// participant's run, a participant cannot steer the owner's or another
// participant's, and each writer can steer its own.
func TestDispatchControl_Participant_OnlyTheWriterSteers(t *testing.T) {
	fx := newSurfaceFixture(t)
	surface := participantSurface(t, fx, recordingEnsurer{})
	guestRun, err := startShared(participantCtx(t, "writer-b", ""), surface, "guest")
	if err != nil {
		t.Fatalf("guest start: %v", err)
	}
	ownerRun, err := startShared(authCtx(t, sharedSession), surface, "owner")
	if err != nil {
		t.Fatalf("owner start: %v", err)
	}
	for _, run := range []string{guestRun.TaskID, ownerRun.TaskID} {
		if _, err := fx.steering.Open(identity.Quadruple{Identity: sharedSession, RunID: run}); err != nil {
			t.Fatalf("steering.Open: %v", err)
		}
	}
	cancel := func(ctx context.Context, run string) error {
		_, err := surface.Dispatch(ctx, methods.MethodUserMessage, &types.ControlRequest{
			Identity: types.IdentityScope{Tenant: sharedSession.TenantID, User: sharedSession.UserID, Session: sharedSession.SessionID, Run: run},
			Payload:  map[string]any{"message": "hi"},
		})
		return err
	}
	cases := []struct {
		name  string
		ctx   context.Context
		run   string
		allow bool
	}{
		{"writer steers own run", participantCtx(t, "writer-b", ""), guestRun.TaskID, true},
		{"owner steers participant run", authCtx(t, sharedSession), guestRun.TaskID, false},
		{"labelled owner steers participant run", participantCtx(t, "owner", "Ana"), guestRun.TaskID, false},
		{"other participant steers participant run", participantCtx(t, "writer-c", ""), guestRun.TaskID, false},
		{"participant steers owner run", participantCtx(t, "writer-b", ""), ownerRun.TaskID, false},
		{"owner steers own run", authCtx(t, sharedSession), ownerRun.TaskID, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cancel(tc.ctx, tc.run)
			if tc.allow && err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			if !tc.allow && codeOf(t, err) != protoerrors.CodeScopeMismatch {
				t.Fatalf("want scope_mismatch, got %v", err)
			}
		})
	}
}
