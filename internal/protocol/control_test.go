package protocol_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// TestDispatch_AllNineControls_RoundTrip exercises every one of the nine
// steering-control Protocol methods through the in-process surface: each
// is submitted by a VERIFIED caller (identity on ctx) at a sufficient
// privilege tier with a method-appropriate payload, and the test asserts
// (a) Dispatch returns a *types.ControlResponse with Accepted=true and
// the right Method echo, and (b) the control event actually landed on the
// run's steering inbox — proving the surface reached the real Inbox, not
// a stub. The owning user satisfies the owner_user/session_user controls;
// PRIORITIZE requires the admin scope claim.
func TestDispatch_AllNineControls_RoundTrip(t *testing.T) {
	cases := []struct {
		method  methods.Method
		admin   bool // PRIORITIZE is admin-only; the rest pass as the owning user
		payload map[string]any
		ctrl    steering.ControlType
	}{
		{methods.MethodCancel, false, nil, steering.ControlCancel},
		{methods.MethodPause, false, nil, steering.ControlPause},
		{methods.MethodResume, false, nil, steering.ControlResume},
		{methods.MethodApprove, false, nil, steering.ControlApprove},
		{methods.MethodReject, false, nil, steering.ControlReject},
		{methods.MethodRedirect, false, map[string]any{"goal": "new goal"}, steering.ControlRedirect},
		{methods.MethodInjectContext, false, map[string]any{"note": "context"}, steering.ControlInjectContext},
		{methods.MethodUserMessage, false, map[string]any{"message": "hello agent"}, steering.ControlUserMessage},
		{methods.MethodPrioritize, true, map[string]any{"priority": 7}, steering.ControlPrioritize},
	}

	for _, tc := range cases {
		t.Run(string(tc.method), func(t *testing.T) {
			fx := newSurfaceFixture(t)
			run := testRun("run-" + string(tc.method))
			inbox, err := fx.steering.Open(run)
			if err != nil {
				t.Fatalf("steering.Open: %v", err)
			}

			// The caller is the run's owning user. PRIORITIZE additionally
			// requires the verified admin scope.
			var scopes []auth.Scope
			if tc.admin {
				scopes = append(scopes, auth.ScopeAdmin)
			}
			ctx := authCtx(t, run.Identity, scopes...)

			resp, err := fx.surface.Dispatch(ctx, tc.method, &types.ControlRequest{
				Identity: types.IdentityScope{
					Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
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
			// CallerTenant is the VERIFIED caller's tenant, not echoed from
			// the request body.
			if ev.CallerTenant != run.TenantID {
				t.Errorf("Dispatch(%s): enqueued CallerTenant = %q, want %q (verified caller tenant)", tc.method, ev.CallerTenant, run.TenantID)
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
	run := testRun("run-adminlow")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	ctx := authCtx(t, run.Identity, auth.ScopeAdmin)
	_, err := fx.surface.Dispatch(ctx, methods.MethodInjectContext, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
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
	runA := testRun("run-A")
	runB := testRun("run-B")
	inboxA, err := fx.steering.Open(runA)
	if err != nil {
		t.Fatalf("steering.Open(A): %v", err)
	}
	inboxB, err := fx.steering.Open(runB)
	if err != nil {
		t.Fatalf("steering.Open(B): %v", err)
	}

	ctx := authCtx(t, runA.Identity)
	_, err = fx.surface.Dispatch(ctx, methods.MethodPause, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: runA.TenantID, User: runA.UserID, Session: runA.SessionID, Run: runA.RunID,
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

// TestDispatch_BodyScopeClaimIsIgnored_NoEscalation is the core
// privilege-escalation regression. A non-admin caller (the run's owning
// user) submits PRIORITIZE — an admin-only control — while asserting
// Scope:"admin" in the request BODY. The surface must ignore the body
// claim and derive owner_user from the verified ctx, so the control is
// rejected with CodeScopeMismatch. A green here proves a body-supplied
// scope string can no longer elevate a caller.
func TestDispatch_BodyScopeClaimIsIgnored_NoEscalation(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-escalate")
	inbox, err := fx.steering.Open(run)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	// Caller authenticates as the owning user with NO admin scope.
	ctx := authCtx(t, run.Identity)
	_, err = fx.surface.Dispatch(ctx, methods.MethodPrioritize, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "admin", // the lie — must be ignored
		},
		Payload: map[string]any{"priority": 9},
	})
	if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("Dispatch(prioritize, body-claims-admin) code = %q, want %q — the body scope claim must NOT elevate the caller", code, protoerrors.CodeScopeMismatch)
	}
	if inbox.Len() != 0 {
		t.Fatalf("escalated control reached the inbox (Len=%d) — body scope claim was trusted", inbox.Len())
	}
}

// TestDispatch_NoVerifiedIdentity_FailsClosed proves the surface no
// longer falls back to the request body when ctx carries no verified
// identity (the auth middleware did not run). A fully-populated body,
// including Scope:"admin", must still be rejected with
// CodeIdentityRequired.
func TestDispatch_NoVerifiedIdentity_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-noident")
	inbox, err := fx.steering.Open(run)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	_, err = fx.surface.Dispatch(t.Context(), methods.MethodPause, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "admin",
		},
	})
	if code := codeOf(t, err); code != protoerrors.CodeIdentityRequired {
		t.Fatalf("Dispatch(no verified identity) code = %q, want %q", code, protoerrors.CodeIdentityRequired)
	}
	if inbox.Len() != 0 {
		t.Fatalf("control with no verified identity reached the inbox (Len=%d)", inbox.Len())
	}
}

// TestDispatch_CrossTenantNonAdmin_Rejected proves a caller verified in
// one tenant cannot steer a run in another: deriveSteeringScope grants no
// authority (the caller is neither admin nor the run's owning user), so
// the control is rejected with CodeScopeMismatch before it reaches the
// inbox. This is the gate the old code could never fire, because it set
// CallerTenant from the body (always == the run tenant).
func TestDispatch_CrossTenantNonAdmin_Rejected(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-xtenant") // tenant-a
	inbox, err := fx.steering.Open(run)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	// Caller is verified in a DIFFERENT tenant, no admin scope.
	attacker := identity.Identity{TenantID: "tenant-b", UserID: "user-1", SessionID: "session-x"}
	ctx := authCtx(t, attacker)
	_, err = fx.surface.Dispatch(ctx, methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
		},
	})
	if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("Dispatch(cross-tenant, non-admin) code = %q, want %q", code, protoerrors.CodeScopeMismatch)
	}
	if inbox.Len() != 0 {
		t.Fatalf("cross-tenant non-admin control reached the inbox (Len=%d)", inbox.Len())
	}
}

// TestDispatch_SessionIDCollision_NonAdminCannotSteerOtherUsersRun is the
// load-bearing collision-safety assertion for the non-admin token
// contract. Two users in the same tenant chose the SAME session-id string
// ("session-x"); user-A owns the target run. A verified non-admin token
// for (tenant-a, user-B, session-x) must NOT be able to steer
// (tenant-a, user-A, session-x)'s run just because the session-id
// collides — deriveSteeringScope compares the user component, so user-B
// earns no authority and the control is rejected CodeScopeMismatch before
// it reaches the inbox. A green here proves a session-id collision across
// users cannot escalate.
func TestDispatch_SessionIDCollision_NonAdminCannotSteerOtherUsersRun(t *testing.T) {
	fx := newSurfaceFixture(t)
	// user-A owns the run; session id is the colliding string.
	run := identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-x"},
		RunID:    "run-collision",
	}
	inbox, err := fx.steering.Open(run)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	// user-B holds a VERIFIED non-admin token for the SAME (tenant,
	// session) but a different user. The transport forces body==JWT, so
	// the only run user-B can name is user-B's own; here we exercise the
	// surface directly with a body that names user-A's run (the worst
	// case) to prove deriveSteeringScope refuses it even if the body-vs-JWT
	// transport gate were bypassed.
	attacker := identity.Identity{TenantID: "tenant-a", UserID: "user-B", SessionID: "session-x"}
	ctx := authCtx(t, attacker) // no admin scope

	for _, method := range []methods.Method{methods.MethodInjectContext, methods.MethodCancel, methods.MethodUserMessage} {
		_, err := fx.surface.Dispatch(ctx, method, &types.ControlRequest{
			Identity: types.IdentityScope{
				Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			},
			Payload: map[string]any{"note": "collision"},
		})
		if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
			t.Fatalf("Dispatch(%s) by session-id collision: code = %q, want %q", method, code, protoerrors.CodeScopeMismatch)
		}
	}
	if inbox.Len() != 0 {
		t.Fatalf("a session-id collision control reached user-A's inbox (Len=%d) — collision-safety breached", inbox.Len())
	}
}

// TestDispatch_NonAdminOwnerPrioritize_RejectedBeforeLookup proves the
// per-control scope minimum is enforced at the edge BEFORE the inbox
// Lookup: a non-admin owner submitting the admin-only PRIORITIZE is
// rejected CodeScopeMismatch even when the run has NO live inbox — the
// authorization failure wins over a would-be not-found, so an
// unauthorised caller cannot probe run existence. This is the ordering the
// live smoke relies on (PRIORITIZE → 403 on a ghost run).
func TestDispatch_NonAdminOwnerPrioritize_RejectedBeforeLookup(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-ghost-prio") // NO inbox opened — a ghost run.

	ctx := authCtx(t, run.Identity) // the owning user, no admin scope.
	_, err := fx.surface.Dispatch(ctx, methods.MethodPrioritize, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
		},
		Payload: map[string]any{"priority": 9},
	})
	if code := codeOf(t, err); code != protoerrors.CodeScopeMismatch {
		t.Fatalf("Dispatch(prioritize, non-admin owner, ghost run) code = %q, want %q (scope check must precede inbox lookup)", code, protoerrors.CodeScopeMismatch)
	}
}

// TestDispatch_NonAdminOwner_OwnRunControls confirms a non-admin owner can
// exercise every control RFC §6.3 grants the owning user — the positive
// half of the non-admin token contract. The owner submits the eight
// owner/session-level controls on their own run with NO scopes attached
// (a non-admin token), and each lands on the inbox.
func TestDispatch_NonAdminOwner_OwnRunControls(t *testing.T) {
	ownerControls := []struct {
		method  methods.Method
		payload map[string]any
	}{
		{methods.MethodCancel, nil},
		{methods.MethodPause, nil},
		{methods.MethodResume, nil},
		{methods.MethodApprove, nil},
		{methods.MethodReject, nil},
		{methods.MethodRedirect, map[string]any{"goal": "new goal"}},
		{methods.MethodInjectContext, map[string]any{"note": "ctx"}},
		{methods.MethodUserMessage, map[string]any{"message": "hi"}},
	}
	for _, tc := range ownerControls {
		t.Run(string(tc.method), func(t *testing.T) {
			fx := newSurfaceFixture(t)
			run := testRun("run-nonadmin-" + string(tc.method))
			inbox, err := fx.steering.Open(run)
			if err != nil {
				t.Fatalf("steering.Open: %v", err)
			}
			ctx := authCtx(t, run.Identity) // non-admin owner.
			if _, err := fx.surface.Dispatch(ctx, tc.method, &types.ControlRequest{
				Identity: types.IdentityScope{
					Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
				},
				Payload: tc.payload,
			}); err != nil {
				t.Fatalf("Dispatch(%s) by non-admin owner: %v", tc.method, err)
			}
			if inbox.Len() != 1 {
				t.Fatalf("Dispatch(%s) by non-admin owner did not reach the inbox (Len=%d)", tc.method, inbox.Len())
			}
		})
	}
}

// TestDispatch_CrossTenantAdmin_Allowed proves the admin scope is the one
// legitimate path to cross-tenant steering: an admin verified in tenant-b
// may cancel a run in tenant-a. The control reaches the inbox carrying
// the verified caller tenant.
func TestDispatch_CrossTenantAdmin_Allowed(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-xtenant-admin") // tenant-a
	inbox, err := fx.steering.Open(run)
	if err != nil {
		t.Fatalf("steering.Open: %v", err)
	}

	admin := identity.Identity{TenantID: "tenant-b", UserID: "ops", SessionID: "session-ops"}
	ctx := authCtx(t, admin, auth.ScopeAdmin)
	_, err = fx.surface.Dispatch(ctx, methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
		},
	})
	if err != nil {
		t.Fatalf("Dispatch(cross-tenant, admin) error = %v, want nil", err)
	}
	if inbox.Len() != 1 {
		t.Fatalf("admin cross-tenant control did not reach the inbox (Len=%d)", inbox.Len())
	}
	drained, err := inbox.Drain()
	if err != nil {
		t.Fatalf("inbox.Drain: %v", err)
	}
	if drained[0].CallerTenant != "tenant-b" {
		t.Errorf("CallerTenant = %q, want %q (verified admin tenant)", drained[0].CallerTenant, "tenant-b")
	}
}
