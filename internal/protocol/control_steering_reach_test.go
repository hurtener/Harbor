package protocol_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// D-513: steering_requires_agent_reach. The surface uses the real signed
// reach gate (no allow-all fixture authorizer) so reach is decided exactly
// as production decides it.

var steerAuthor = identity.Identity{TenantID: "tenant-a", UserID: "author", SessionID: "steer"}

func steerReachSurface(t *testing.T, fx *surfaceFixture, required bool) *protocol.ControlSurface {
	t.Helper()
	surface, err := protocol.NewControlSurface(fx.tasks, fx.steering,
		protocol.WithAgentResolver(fixtureAgentResolver{}),
		protocol.WithSteeringRequiresAgentReach(required))
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	return surface
}

// reachCtx is a verified caller context with the given signed reach seated;
// reach == nil seats no reach at all (a read-only bearer).
func reachCtx(t *testing.T, id identity.Identity, reach []string, scopes ...auth.Scope) context.Context {
	t.Helper()
	ctx := authCtx(t, id, scopes...)
	if reach != nil {
		ctx = auth.WithAgentReach(ctx, reach)
	}
	return ctx
}

// startSteerRun starts a run as id under agentID (empty = default agent)
// and opens its steering inbox, returning the run quadruple.
func startSteerRun(t *testing.T, fx *surfaceFixture, s *protocol.ControlSurface, id identity.Identity, agentID, key string) identity.Quadruple {
	t.Helper()
	effective := agentID
	if effective == "" {
		effective = "fixture-default"
	}
	resp, err := s.Dispatch(reachCtx(t, id, []string{effective}), methods.MethodStart, &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "hello",
		AgentID:        agentID,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := identity.Quadruple{Identity: id, RunID: resp.(*types.StartResponse).TaskID}
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	return run
}

var steeringCases = []struct {
	method  methods.Method
	payload map[string]any
	adds    bool
}{
	{methods.MethodUserMessage, map[string]any{"message": "more input"}, true},
	{methods.MethodRedirect, map[string]any{"goal": "another goal"}, true},
	{methods.MethodInjectContext, map[string]any{"note": "extra context"}, true},
	{methods.MethodCancel, nil, false},
	{methods.MethodPause, nil, false},
	{methods.MethodResume, nil, false},
	{methods.MethodApprove, map[string]any{"reason": "ok"}, false},
	{methods.MethodReject, nil, false},
	{methods.MethodReject, map[string]any{"reason": ""}, false},
}

func steer(ctx context.Context, s *protocol.ControlSurface, m methods.Method, run identity.Quadruple, payload map[string]any) error {
	_, err := s.Dispatch(ctx, m, &types.ControlRequest{
		Identity: types.IdentityScope{Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID},
		Payload:  payload,
		EventID:  "evt-" + string(m),
	})
	return err
}

func TestSteeringRequiresAgentReach_On(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := steerReachSurface(t, fx, true)
	named := startSteerRun(t, fx, s, steerAuthor, "agent-a", "named")
	defaulted := startSteerRun(t, fx, s, steerAuthor, "", "defaulted")

	for _, tc := range steeringCases {
		t.Run(string(tc.method), func(t *testing.T) {
			// A reach-less author bearer: additive controls refused with
			// scope_mismatch; reductions and decisions still accepted.
			err := steer(reachCtx(t, steerAuthor, nil), s, tc.method, named, tc.payload)
			if tc.adds {
				if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
					t.Fatalf("reach-less %s = %v, want scope_mismatch", tc.method, err)
				}
			} else if err != nil {
				t.Fatalf("reach-less %s = %v, want accepted (authorship-only)", tc.method, err)
			}
			if !tc.adds {
				return
			}
			// An explicitly empty reach is refused the same way.
			if got := codeOf(t, steer(reachCtx(t, steerAuthor, []string{}), s, tc.method, named, tc.payload)); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("empty-reach %s code = %q, want scope_mismatch", tc.method, got)
			}
			// Reach to another agent only: refused.
			if got := codeOf(t, steer(reachCtx(t, steerAuthor, []string{"agent-b"}), s, tc.method, named, tc.payload)); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("other-agent %s code = %q, want scope_mismatch", tc.method, got)
			}
			// A caller-supplied agent id in the payload cannot redirect the
			// check: the recorded agent is agent-a, the bearer reaches agent-b.
			spoof := map[string]any{"agent_id": "agent-b"}
			for k, v := range tc.payload {
				spoof[k] = v
			}
			if got := codeOf(t, steer(reachCtx(t, steerAuthor, []string{"agent-b"}), s, tc.method, named, spoof)); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("payload agent_id spoof %s code = %q, want scope_mismatch", tc.method, got)
			}
			// Reach to the recorded agent: accepted.
			if err := steer(reachCtx(t, steerAuthor, []string{"agent-b", "agent-a"}), s, tc.method, named, tc.payload); err != nil {
				t.Fatalf("reach to recorded agent %s = %v, want accepted", tc.method, err)
			}
			// A default-agent run records no agent id; its agent is the
			// configured default, which the bearer must reach.
			if got := codeOf(t, steer(reachCtx(t, steerAuthor, []string{"agent-a"}), s, tc.method, defaulted, tc.payload)); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("default-agent run without default reach %s code = %q", tc.method, got)
			}
			if err := steer(reachCtx(t, steerAuthor, []string{"fixture-default"}), s, tc.method, defaulted, tc.payload); err != nil {
				t.Fatalf("default-agent run with default reach %s = %v", tc.method, err)
			}
			// The admin scope is not an exemption.
			if got := codeOf(t, steer(reachCtx(t, steerAuthor, nil, auth.ScopeAdmin), s, tc.method, named, tc.payload)); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("reach-less admin %s code = %q, want scope_mismatch", tc.method, got)
			}
			// Authorship stays first: another user with reach to the agent
			// is refused as a non-author.
			other := identity.Identity{TenantID: steerAuthor.TenantID, UserID: "someone-else", SessionID: steerAuthor.SessionID}
			err = steer(reachCtx(t, other, []string{"agent-a"}), s, tc.method, named, tc.payload)
			if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
				t.Fatalf("non-author with reach %s = %v, want scope_mismatch", tc.method, err)
			}
		})
	}
}

// A run that has no task record cannot have its agent determined, so an
// additive control is refused even for the run's author.
func TestSteeringRequiresAgentReach_UnknownTaskFailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := steerReachSurface(t, fx, true)
	run := identity.Quadruple{Identity: steerAuthor, RunID: "no-such-task"}
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	err := steer(reachCtx(t, steerAuthor, []string{"agent-a", "fixture-default"}), s, methods.MethodUserMessage, run, map[string]any{"message": "hi"})
	if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("user_message on a run without a task = %v, want scope_mismatch", err)
	}
}

func TestSteeringRequiresAgentReach_Off(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := steerReachSurface(t, fx, false)
	run := startSteerRun(t, fx, s, steerAuthor, "agent-a", "off")
	for _, tc := range steeringCases {
		if err := steer(reachCtx(t, steerAuthor, nil), s, tc.method, run, tc.payload); err != nil {
			t.Fatalf("setting off: reach-less %s = %v, want accepted", tc.method, err)
		}
	}
}

// The same agent id string exists in two tenants. A tenant-A author with
// reach to it never steers tenant B's run.
func TestSteeringRequiresAgentReach_CrossTenant(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := steerReachSurface(t, fx, true)
	idB := identity.Identity{TenantID: "tenant-b", UserID: steerAuthor.UserID, SessionID: steerAuthor.SessionID}
	runA := startSteerRun(t, fx, s, steerAuthor, "shared-agent", "a")
	runB := startSteerRun(t, fx, s, idB, "shared-agent", "b")
	ctxA := reachCtx(t, steerAuthor, []string{"shared-agent"})
	for _, m := range []methods.Method{methods.MethodUserMessage, methods.MethodRedirect, methods.MethodInjectContext} {
		var payload map[string]any
		switch m {
		case methods.MethodUserMessage:
			payload = map[string]any{"message": "x"}
		case methods.MethodRedirect:
			payload = map[string]any{"goal": "x"}
		case methods.MethodInjectContext:
			payload = map[string]any{"note": "x"}
		}
		if got := codeOf(t, steer(ctxA, s, m, runB, payload)); got != protoerrors.CodeScopeMismatch {
			t.Fatalf("tenant-A bearer %s on tenant-B run code = %q, want scope_mismatch", m, got)
		}
		if err := steer(ctxA, s, m, runA, payload); err != nil {
			t.Fatalf("tenant-A bearer %s on own run = %v", m, err)
		}
	}
}

// A reject reason reaches the model as the rejected tool's error text, so
// with the setting on a non-empty reason needs reach to the run's agent. A
// reject without a reason stays authorship-only.
func TestSteeringRequiresAgentReach_RejectReason(t *testing.T) {
	fx := newSurfaceFixture(t)
	s := steerReachSurface(t, fx, true)
	run := startSteerRun(t, fx, s, steerAuthor, "agent-a", "reject")
	for _, reason := range []any{"not this tool", " ", 42, map[string]any{"text": "x"}} {
		err := steer(reachCtx(t, steerAuthor, nil), s, methods.MethodReject, run, map[string]any{"reason": reason})
		if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
			t.Fatalf("reach-less reject with reason %v = %v, want scope_mismatch", reason, err)
		}
		if got := codeOf(t, steer(reachCtx(t, steerAuthor, []string{"agent-b"}), s, methods.MethodReject, run, map[string]any{"reason": reason})); got != protoerrors.CodeScopeMismatch {
			t.Fatalf("other-agent reject with reason %v code = %q", reason, got)
		}
	}
	for _, payload := range []map[string]any{nil, {"reason": ""}, {"reason": nil}} {
		if err := steer(reachCtx(t, steerAuthor, nil), s, methods.MethodReject, run, payload); err != nil {
			t.Fatalf("reach-less reject without reason %v = %v, want accepted", payload, err)
		}
	}
	if err := steer(reachCtx(t, steerAuthor, []string{"agent-a"}), s, methods.MethodReject, run, map[string]any{"reason": "not this tool"}); err != nil {
		t.Fatalf("reject with reason and reach = %v, want accepted", err)
	}
	// Approve reasons reach only events and stay authorship-only.
	if err := steer(reachCtx(t, steerAuthor, nil), s, methods.MethodApprove, run, map[string]any{"reason": "fine"}); err != nil {
		t.Fatalf("reach-less approve with reason = %v, want accepted", err)
	}
	// Setting off: a reach-less reject with a reason is accepted.
	off := steerReachSurface(t, fx, false)
	if err := steer(reachCtx(t, steerAuthor, nil), off, methods.MethodReject, run, map[string]any{"reason": "x"}); err != nil {
		t.Fatalf("setting off: reject with reason = %v", err)
	}
}
