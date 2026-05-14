package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// TestDispatch_AllNineControls_RoundTrip exercises every one of the nine
// steering-control Protocol methods through the in-process surface: each
// is submitted at a sufficient scope with a method-appropriate payload,
// and the test asserts (a) Dispatch returns a *types.ControlResponse
// with Accepted=true and the right Method echo, and (b) the control
// event actually landed on the run's steering inbox — proving the
// surface reached the real Phase 52 Inbox, not a stub.
func TestDispatch_AllNineControls_RoundTrip(t *testing.T) {
	cases := []struct {
		method  methods.Method
		scope   string
		payload map[string]any
		ctrl    steering.ControlType
	}{
		{methods.MethodCancel, "owner_user", nil, steering.ControlCancel},
		{methods.MethodPause, "owner_user", nil, steering.ControlPause},
		{methods.MethodResume, "owner_user", nil, steering.ControlResume},
		{methods.MethodApprove, "owner_user", nil, steering.ControlApprove},
		{methods.MethodReject, "owner_user", nil, steering.ControlReject},
		{methods.MethodRedirect, "owner_user", map[string]any{"goal": "new goal"}, steering.ControlRedirect},
		{methods.MethodInjectContext, "session_user", map[string]any{"note": "context"}, steering.ControlInjectContext},
		{methods.MethodUserMessage, "session_user", map[string]any{"message": "hello agent"}, steering.ControlUserMessage},
		{methods.MethodPrioritize, "admin", map[string]any{"priority": 7}, steering.ControlPrioritize},
	}

	for _, tc := range cases {
		t.Run(string(tc.method), func(t *testing.T) {
			fx := newSurfaceFixture(t)
			run := testRun("tenant-a", "run-"+string(tc.method))
			inbox, err := fx.steering.Open(run)
			if err != nil {
				t.Fatalf("steering.Open: %v", err)
			}

			resp, err := fx.surface.Dispatch(context.Background(), tc.method, &types.ControlRequest{
				Identity: types.IdentityScope{
					Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
					Scope: tc.scope,
				},
				Payload: tc.payload,
				EventID: "evt-" + string(tc.method),
			})
			if err != nil {
				t.Fatalf("Dispatch(%s) error = %v", tc.method, err)
			}

			cr, ok := resp.(*types.ControlResponse)
			if !ok {
				t.Fatalf("Dispatch(%s) returned %T, want *types.ControlResponse", tc.method, resp)
			}
			if !cr.Accepted {
				t.Errorf("Dispatch(%s): Accepted = false, want true", tc.method)
			}
			if cr.Method != string(tc.method) {
				t.Errorf("Dispatch(%s): Method echo = %q, want %q", tc.method, cr.Method, tc.method)
			}
			if cr.ProtocolVersion != types.ProtocolVersion {
				t.Errorf("Dispatch(%s): ProtocolVersion = %q, want %q", tc.method, cr.ProtocolVersion, types.ProtocolVersion)
			}

			// The control event must have landed on the inbox.
			if inbox.Len() != 1 {
				t.Fatalf("Dispatch(%s): inbox.Len() = %d, want 1 — the control did not reach the real steering inbox", tc.method, inbox.Len())
			}
			drained, err := inbox.Drain()
			if err != nil {
				t.Fatalf("inbox.Drain: %v", err)
			}
			if len(drained) != 1 {
				t.Fatalf("Dispatch(%s): drained %d events, want 1", tc.method, len(drained))
			}
			ev := drained[0]
			if ev.Type != tc.ctrl {
				t.Errorf("Dispatch(%s): enqueued control type = %q, want %q", tc.method, ev.Type, tc.ctrl)
			}
			if ev.Identity != run {
				t.Errorf("Dispatch(%s): enqueued event identity = %+v, want %+v", tc.method, ev.Identity, run)
			}
			if ev.EventID != "evt-"+string(tc.method) {
				t.Errorf("Dispatch(%s): enqueued event id = %q, want %q", tc.method, ev.EventID, "evt-"+string(tc.method))
			}
		})
	}
}

// TestDispatch_AdminSatisfiesLowerScopes confirms the scope total order
// holds end-to-end: an admin caller can submit a session_user-minimum
// control (INJECT_CONTEXT). steering.CheckScope encodes the order; this
// test proves the surface does not add a stricter check.
func TestDispatch_AdminSatisfiesLowerScopes(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("tenant-a", "run-adminlow")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "admin",
		},
		Payload: map[string]any{"note": "admin-injected context"},
	})
	if err != nil {
		t.Fatalf("Dispatch(inject_context, admin) error = %v, want nil — admin satisfies session_user", err)
	}
}

// TestDispatch_PerRunIsolation confirms a control submitted for run A
// lands only on run A's inbox, never run B's — the per-run isolation
// gate (CLAUDE.md §6). Two runs in the same session get distinct inboxes.
func TestDispatch_PerRunIsolation(t *testing.T) {
	fx := newSurfaceFixture(t)
	runA := testRun("tenant-a", "run-A")
	runB := testRun("tenant-a", "run-B")
	inboxA, err := fx.steering.Open(runA)
	if err != nil {
		t.Fatalf("steering.Open(A): %v", err)
	}
	inboxB, err := fx.steering.Open(runB)
	if err != nil {
		t.Fatalf("steering.Open(B): %v", err)
	}

	_, err = fx.surface.Dispatch(context.Background(), methods.MethodPause, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: runA.TenantID, User: runA.UserID, Session: runA.SessionID, Run: runA.RunID,
			Scope: "owner_user",
		},
	})
	if err != nil {
		t.Fatalf("Dispatch(pause, runA): %v", err)
	}

	if inboxA.Len() != 1 {
		t.Errorf("runA inbox.Len() = %d, want 1", inboxA.Len())
	}
	if inboxB.Len() != 0 {
		t.Errorf("runB inbox.Len() = %d, want 0 — a control for run A leaked onto run B", inboxB.Len())
	}
}
