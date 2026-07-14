package protocol_test

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// widenSpyRetention records the `widened` argument each Retention call
// received and returns a fixed set of horizons stamped by scope.
type widenSpyRetention struct {
	widenedSeen []bool
}

func (w *widenSpyRetention) seam(runtimeAt time.Time) func(context.Context, identity.Identity, bool) []types.RetentionHorizon {
	return func(_ context.Context, _ identity.Identity, widened bool) []types.RetentionHorizon {
		w.widenedSeen = append(w.widenedSeen, widened)
		scope := types.RetentionScopeSession
		if widened {
			scope = types.RetentionScopeRuntime
		}
		return []types.RetentionHorizon{
			{Surface: "events", Scope: types.RetentionScopeRuntime, OldestRetainedAt: &runtimeAt},
			{Surface: "tasks", Scope: scope, OldestRetainedAt: &runtimeAt},
		}
	}
}

// TestPostureHealth_WidenedByVerifiedScope_EmitsExactlyOneAdminScopeUsed
// pins D-310 half (1): a verified admin OR console:fleet caller widens the
// tasks/sessions horizons to runtime scope AND the widened fan-in emits
// exactly one audit.admin_scope_used. An ordinary caller does neither.
func TestPostureHealth_WidenedByVerifiedScope_EmitsExactlyOneAdminScopeUsed(t *testing.T) {
	for _, scope := range []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			bus := newPostureBus(t)
			sub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
			if err != nil {
				t.Fatalf("bus.Subscribe: %v", err)
			}
			defer sub.Cancel()

			at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
			spy := &widenSpyRetention{}
			deps := basePostureDeps(t)
			deps.Bus = bus
			deps.Retention = spy.seam(at)
			s, err := protocol.NewPostureSurface(deps)
			if err != nil {
				t.Fatalf("NewPostureSurface: %v", err)
			}

			verified := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
			ctxFleet := auth.WithScopes(mustCtx(t, verified), []auth.Scope{scope})
			req := &types.RuntimeInfoRequest{
				Identity: types.IdentityScope{Tenant: "svc", User: "coordinator", Session: "fleet"},
			}
			out, err := s.Dispatch(ctxFleet, methods.MethodRuntimeHealth, req)
			if err != nil {
				t.Fatalf("Dispatch(runtime.health): %v", err)
			}
			h := out.(*types.RuntimeHealth)
			var tasksScope string
			for _, r := range h.Retention {
				if r.Surface == "tasks" {
					tasksScope = r.Scope
				}
			}
			if tasksScope != types.RetentionScopeRuntime {
				t.Fatalf("tasks scope = %q, want runtime (widened)", tasksScope)
			}
			if len(spy.widenedSeen) != 1 || !spy.widenedSeen[0] {
				t.Fatalf("Retention widened arg = %+v, want [true]", spy.widenedSeen)
			}

			// EXACTLY ONE admin_scope_used anchored on the actor's identity
			// (the bus's own Admin:true subscribe-time notice carries an
			// empty identity, so it is excluded).
			count := drainAdminScopeUsed(t, sub, "svc", 300*time.Millisecond)
			if count != 1 {
				t.Fatalf("admin_scope_used count = %d, want exactly 1", count)
			}
		})
	}
}

// TestPostureHealth_NoScope_StaysScoped_NoAudit pins D-310's fail-closed
// invariant: a caller with no elevated scope reads the scoped fold and
// emits NO audit. The trust-based dev path (no verified identity on ctx)
// is the same — widening is server-derived, never body-derived.
func TestPostureHealth_NoScope_StaysScoped_NoAudit(t *testing.T) {
	bus := newPostureBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	spy := &widenSpyRetention{}
	deps := basePostureDeps(t)
	deps.Bus = bus
	deps.Retention = spy.seam(at)
	s, err := protocol.NewPostureSurface(deps)
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}

	verified := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "sess"}
	req := &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "tenant-a", User: "u", Session: "sess"},
	}
	out, err := s.Dispatch(mustCtx(t, verified), methods.MethodRuntimeHealth, req)
	if err != nil {
		t.Fatalf("Dispatch(runtime.health): %v", err)
	}
	h := out.(*types.RuntimeHealth)
	for _, r := range h.Retention {
		if r.Surface == "tasks" && r.Scope != types.RetentionScopeSession {
			t.Fatalf("tasks scope = %q, want session (not widened)", r.Scope)
		}
	}
	if len(spy.widenedSeen) != 1 || spy.widenedSeen[0] {
		t.Fatalf("Retention widened arg = %+v, want [false]", spy.widenedSeen)
	}
	if count := drainAdminScopeUsed(t, sub, "tenant-a", 300*time.Millisecond); count != 0 {
		t.Fatalf("admin_scope_used count = %d, want 0 (no widening)", count)
	}
}

// TestPostureHealth_BodyScopeNeverWidens proves the elevation is
// server-derived: a request whose BODY names a fleet identity but whose
// ctx carries no verified scope does NOT widen (D-299 discipline).
func TestPostureHealth_BodyScopeNeverWidens(t *testing.T) {
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	spy := &widenSpyRetention{}
	deps := basePostureDeps(t)
	deps.Retention = spy.seam(at)
	s, err := protocol.NewPostureSurface(deps)
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}
	// No auth middleware ran — no verified scope on ctx (trust-based dev).
	req := &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "svc", User: "coordinator", Session: "fleet"},
	}
	if _, err := s.Dispatch(context.Background(), methods.MethodRuntimeHealth, req); err != nil {
		t.Fatalf("Dispatch(runtime.health): %v", err)
	}
	if len(spy.widenedSeen) != 1 || spy.widenedSeen[0] {
		t.Fatalf("Retention widened arg = %+v, want [false] (body never widens)", spy.widenedSeen)
	}
}

// TestPostureHealth_WidenedAuditEmitFailure_FailsLoud pins the fail-loud
// audit contract: when the redactor refuses the widened admin_scope_used
// payload, the read FAILS LOUD with CodeRuntimeError — the fan-in already
// crossed the tenant boundary, so the operator MUST see the audit.
func TestPostureHealth_WidenedAuditEmitFailure_FailsLoud(t *testing.T) {
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	spy := &widenSpyRetention{}
	deps := basePostureDeps(t)
	deps.Redactor = failingRedactor{} // forces the widened emit to fail
	deps.Retention = spy.seam(at)
	s, err := protocol.NewPostureSurface(deps)
	if err != nil {
		t.Fatalf("NewPostureSurface: %v", err)
	}

	verified := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	ctxFleet := auth.WithScopes(mustCtx(t, verified), []auth.Scope{auth.ScopeConsoleFleet})
	req := &types.RuntimeInfoRequest{
		Identity: types.IdentityScope{Tenant: "svc", User: "coordinator", Session: "fleet"},
	}
	_, err = s.Dispatch(ctxFleet, methods.MethodRuntimeHealth, req)
	if err == nil {
		t.Fatal("widened read with a refusing redactor returned nil error, want CodeRuntimeError")
	}
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) || pe.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("error = %v, want CodeRuntimeError", err)
	}
}

// drainAdminScopeUsed counts the audit.admin_scope_used events anchored
// on actorTenant observed within the window. The bus's own Admin:true
// subscribe-time notice carries an empty identity, so filtering on the
// actor tenant isolates the emits the widened read produced.
func drainAdminScopeUsed(t *testing.T, sub events.Subscription, actorTenant string, window time.Duration) int {
	t.Helper()
	count := 0
	deadline := time.After(window)
	for {
		select {
		case ev := <-sub.Events():
			if ev.Type == events.EventTypeAdminScopeUsed && ev.Identity.TenantID == actorTenant {
				count++
			}
		case <-deadline:
			return count
		}
	}
}
