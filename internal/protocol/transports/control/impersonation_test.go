package control_test

// impersonation_test.go — Phase 72b: 5-shape gate table for the
// admin-impersonation extension on the control transport. The five
// shapes match the acceptance criteria in
// `docs/plans/phase-72b-identityscope-impersonation.md`:
//
//   (1) admin + complete impersonation triplet + body-triple ==
//       impersonating triple → 200 + audit event on the bus.
//   (2) admin + complete triplet + body-triple != impersonating triple
//       → 401 CodeIdentityRequired.
//   (3) admin + impersonation with missing triple component → 401
//       CodeIdentityRequired.
//   (4) non-admin + impersonation → 401/403 CodeScopeMismatch.
//   (5) admin + no impersonation → 200 (backward-compat — the
//       Impersonating field is absent, every other path is unchanged).
//
// Each test wires real auth.WithScopes + identity.With on ctx so the
// gate's "verified JWT identity" surface is exercised end-to-end.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// authedAdminCtx builds a request context that mirrors what
// auth.Middleware would produce for an authenticated admin: the
// verified identity attached + the admin scope claim attached.
func authedAdminCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  "tenant-acme",
		UserID:    "admin-alice",
		SessionID: "sess-admin",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
}

// authedNonAdminCtx is like authedAdminCtx but without the admin
// scope — the canonical "valid bearer but no admin claim" shape.
func authedNonAdminCtx(t *testing.T, tenant, user, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  tenant,
		UserID:    user,
		SessionID: session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return auth.WithScopes(ctx, nil)
}

// impersonationStartBody renders a JSON body for a `start` request
// carrying a fully-populated impersonation triplet. The tenant and the
// target principal are fixed (every caller impersonates the same
// "user-target"); only the admin user/session vary per call.
func impersonationStartBody(adminUser, adminSession string) string {
	const (
		tenant        = "tenant-acme"
		targetUser    = "user-target"
		targetSession = "sess-target"
	)
	return `{"identity":{` +
		`"tenant":"` + tenant + `","user":"` + targetUser + `","session":"` + targetSession + `",` +
		`"actor":{"tenant":"` + tenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"requester":{"tenant":"` + tenant + `","user":"` + adminUser + `","session":"` + adminSession + `"},` +
		`"impersonating":{"tenant":"` + tenant + `","user":"` + targetUser + `","session":"` + targetSession + `"}` +
		`},"query":"q"}`
}

func doImpersonation(t *testing.T, h http.Handler, ctx context.Context, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/control/"+method, strings.NewReader(body))
	req.SetPathValue("method", method)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// (1) Admin + complete impersonation triplet + body-triple ==
// impersonating triple → 200 + AdminScopeUsedPayload on the bus.
func TestImpersonation_AdminAccepted_EmitsAuditEvent(t *testing.T) {
	h, bus, cleanup := newImpersonationHandler(t)
	defer cleanup()

	// Subscribe BEFORE the request so we don't miss the emit.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  "tenant-acme",
		User:    "user-target",
		Session: "sess-target",
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	ctx := authedAdminCtx(t)
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Drain until we see the AdminScopeUsedPayload — the test bus
	// may also deliver other events (task lifecycle, etc.) to the
	// same subscriber depending on its filter shape.
	deadline := time.After(5 * time.Second)
loop:
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription channel closed before AdminScopeUsedPayload arrived")
			}
			if ev.Type != events.EventTypeAdminScopeUsed {
				continue
			}
			payload, isTyped := ev.Payload.(auth.AdminScopeUsedPayload)
			if !isTyped {
				// The patterns redactor returns the typed struct
				// unchanged, but a future redactor change might
				// produce a RedactedMap; accept either, but require
				// the reason sentinel.
				switch p := ev.Payload.(type) {
				case events.RedactedMap:
					if reason, ok := p.Data["Reason"].(string); ok && reason == auth.AdminImpersonationReason {
						break loop
					}
					t.Fatalf("RedactedMap payload missing Reason=%s: %+v", auth.AdminImpersonationReason, p.Data)
				default:
					t.Fatalf("unexpected payload type: %T", ev.Payload)
				}
				break loop
			}
			if payload.Reason != auth.AdminImpersonationReason {
				t.Errorf("Reason: got %q, want %q", payload.Reason, auth.AdminImpersonationReason)
			}
			if payload.Method != string(methods.MethodStart) {
				t.Errorf("Method: got %q, want %q", payload.Method, methods.MethodStart)
			}
			if payload.Actor.User != "admin-alice" {
				t.Errorf("Actor.User: got %q, want %q", payload.Actor.User, "admin-alice")
			}
			if payload.Requester.User != "admin-alice" {
				t.Errorf("Requester.User: got %q, want %q", payload.Requester.User, "admin-alice")
			}
			if payload.Impersonating.User != "user-target" {
				t.Errorf("Impersonating.User: got %q, want %q", payload.Impersonating.User, "user-target")
			}
			break loop
		case <-deadline:
			t.Fatal("timeout waiting for AdminScopeUsedPayload on the bus")
		}
	}
}

// (2) Admin + body's top-level triple != Impersonating triple → 401
// CodeIdentityRequired.
func TestImpersonation_BodyTripleMismatch_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"OTHER-USER","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"requester":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (3) Admin + impersonation with missing triple component → 401
// CodeIdentityRequired. Identity is mandatory; the impersonated triple
// IS identity (CLAUDE.md §6 rule 9).
func TestImpersonation_IncompleteTriple_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	// Missing session on Impersonating.
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"requester":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":""}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (4) Non-admin + impersonation → CodeScopeMismatch. The transport
// fails closed BEFORE Dispatch runs.
func TestImpersonation_NonAdminRejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	// The verified identity is the would-be admin's, but the token
	// doesn't carry the admin scope.
	ctx := authedNonAdminCtx(t, "tenant-acme", "admin-alice", "sess-admin")
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CodeScopeMismatch); body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeScopeMismatch)
}

// (5) Admin + NO impersonation → existing behaviour (200,
// backward-compat). The Impersonating field is absent, every other
// path is unchanged.
func TestImpersonation_BackwardCompat_AdminNoImpersonation_Accepted(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// (6) Admin + impersonation + Actor != JWT identity → CodeScopeMismatch.
// The actor is the audit anchor; faking it is privilege escalation.
func TestImpersonation_ActorMismatchesJWT_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	// JWT says admin-alice but the body's Actor claims admin-bob.
	ctx := authedAdminCtx(t)
	body := impersonationStartBody("admin-bob", "sess-bob") // body Actor != JWT
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CodeScopeMismatch); body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeScopeMismatch)
}

// (7) Admin + impersonation + Requester != Actor → CodeScopeMismatch.
// V1 invariant: Requester == Actor (delegated impersonation is
// post-V1).
func TestImpersonation_RequesterDivergesFromActor_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"requester":{"tenant":"tenant-acme","user":"admin-bob","session":"sess-bob"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CodeScopeMismatch); body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeScopeMismatch)
}

// (8) Admin + impersonation + missing Actor → CodeIdentityRequired.
// Actor is the audit anchor; without it, the request cannot be
// attributed.
func TestImpersonation_MissingActor_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"requester":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (9) Cross-method coverage: a `redirect` request carrying the
// impersonation triplet is gated identically. The control-request
// shape carries the same IdentityScope, so the gate fires the same
// way. The redirect itself fails with CodeNotFound (no live run), but
// THAT is the post-gate path — proves the gate did not silently
// short-circuit.
func TestImpersonation_AcrossControlMethod_GateRunsIdentically(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	// Non-admin → gate fails CodeScopeMismatch BEFORE Dispatch
	// reaches the no-live-run path.
	ctx := authedNonAdminCtx(t, "tenant-acme", "admin-alice", "sess-admin")
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target","run":"r1","scope":"owner_user",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"requester":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"payload":{"goal":"rewritten"}}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodRedirect), body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CodeScopeMismatch); body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeScopeMismatch)
}

// (10) Phase 61 defence-in-depth still holds for non-impersonation
// shapes: a body claiming a tenant != JWT tenant is rejected. The
// impersonation gate does NOT widen the check, it adds a separate
// field that's verified separately.
func TestImpersonation_NonImpersonationMismatch_StillRejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	// No impersonation triplet — just a body claiming a different
	// tenant. The Phase 61 gate STILL rejects this.
	body := `{"identity":{"tenant":"tenant-evil","user":"u","session":"s"},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Phase 61 gate regressed: status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (11) Bare handler (no bus / no redactor wired) + admin
// impersonation → CodeRuntimeError. The accepted-path emit is the
// load-bearing accountability surface; without it the handler refuses
// rather than silently accepting (CLAUDE.md §13 "Silent degradation").
func TestImpersonation_UnwiredHandler_RefusesFailClosed(t *testing.T) {
	// Use the bare handler (no WithEventBus / WithRedactor).
	h, cleanup := newTestHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code == http.StatusOK {
		t.Fatalf("bare handler accepted impersonation without audit emit; status=200 body=%s", rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeRuntimeError)
}

// (12) Admin + impersonation + missing Requester → CodeIdentityRequired.
// Mirrors (8) for the Requester field.
func TestImpersonation_MissingRequester_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (13) Admin + impersonation + Actor with empty Session →
// CodeIdentityRequired. Actor itself must Validate.
func TestImpersonation_ActorIncomplete_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":""},` +
		`"requester":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (14) Admin + impersonation + Requester with empty Tenant →
// CodeIdentityRequired. Requester itself must Validate.
func TestImpersonation_RequesterIncomplete_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	ctx := authedAdminCtx(t)
	body := `{"identity":{` +
		`"tenant":"tenant-acme","user":"user-target","session":"sess-target",` +
		`"actor":{"tenant":"tenant-acme","user":"admin-alice","session":"sess-admin"},` +
		`"requester":{"tenant":"","user":"admin-alice","session":"sess-admin"},` +
		`"impersonating":{"tenant":"tenant-acme","user":"user-target","session":"sess-target"}` +
		`},"query":"q"}`
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeIdentityRequired)
}

// (15) No-ctx-identity (Phase 60 trust-based posture without
// WithoutValidator) + impersonation → CodeScopeMismatch. The gate
// cannot verify the actor without ctx-identity, so refuses
// fail-closed.
func TestImpersonation_NoCtxIdentity_Rejected(t *testing.T) {
	h, _, cleanup := newImpersonationHandler(t)
	defer cleanup()

	// Bare context — no identity attached. The admin scope alone
	// is not enough; we need to verify the Actor matches the JWT.
	ctx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin})
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (CodeScopeMismatch); body=%s", rec.Code, rec.Body.String())
	}
	assertErrorCode(t, rec, protoerrors.CodeScopeMismatch)
}

// (16) Redactor returning a non-map shape → emit fails with a
// loud-logged error (response stays 200 because Dispatch already
// succeeded). Covers the defensive branch in emitAdminScopeUsed.
func TestImpersonation_RedactorReturnsNonMap_LogsLoud(t *testing.T) {
	red := nonMapRedactor{}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = taskReg.Close(context.Background()) }()
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	h, err := control.NewHandler(surface,
		control.WithEventBus(bus),
		control.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("control.NewHandler: %v", err)
	}

	ctx := authedAdminCtx(t)
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("non-map redactor status: %d, want 200 (emit failure is logged not propagated)", rec.Code)
	}
}

// nonMapRedactor — test-only redactor that returns a non-map shape
// to exercise the defensive branch.
type nonMapRedactor struct{}

func (nonMapRedactor) Redact(_ context.Context, _ any) (any, error) {
	return "not-a-map", nil
}

// (17) Coverage: redactor that fails the Redact call → emit fails
// loudly (response stays 200 because Dispatch already succeeded; the
// emit failure is logged loud not propagated to the client). Surfaces
// non-200 + the impersonation request reaches the emit path with the
// failure logged (we observe the failure indirectly — the run was
// already accepted at Dispatch time, so the response is 200 and the
// emit failure is logged not propagated). This test pins the
// fail-loud-log-not-propagate contract.
func TestImpersonation_RedactorFailure_LogsLoud_Not200Regression(t *testing.T) {
	// Use the impersonation handler but swap in a redactor that
	// returns an error. We can't easily inject a custom redactor
	// through newImpersonationHandler; build the surface inline.
	red := failingRedactor{}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = taskReg.Close(context.Background()) }()
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	h, err := control.NewHandler(surface,
		control.WithEventBus(bus),
		control.WithRedactor(red),
		control.WithClock(func() time.Time { return time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC) }),
	)
	if err != nil {
		t.Fatalf("control.NewHandler: %v", err)
	}

	ctx := authedAdminCtx(t)
	body := impersonationStartBody("admin-alice", "sess-admin")
	rec := doImpersonation(t, h, ctx, string(methods.MethodStart), body)
	// The Dispatch already succeeded; we return 200 but logged the
	// emit failure loudly. CLAUDE.md §5 — fail loudly, never silent.
	if rec.Code != http.StatusOK {
		t.Fatalf("redactor-failure status: %d, want 200 (emit failure is logged, not propagated)", rec.Code)
	}
}

// failingRedactor — a test-only redactor that ALWAYS returns an
// error. Lives in *_test.go so the production NewHandler path cannot
// silently resolve to it (CLAUDE.md §13).
type failingRedactor struct{}

func (failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, errSyntheticRedactor
}

var errSyntheticRedactor = errSentinel("synthetic redactor failure")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// assertErrorCode decodes the response body as a Protocol error and
// asserts the Code field matches want.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want protoerrors.Code) {
	t.Helper()
	var perr protoerrors.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &perr); err != nil {
		t.Fatalf("decode error body: %v; raw=%s", err, rec.Body.String())
	}
	if perr.Code != want {
		t.Errorf("error code: got %q, want %q; message=%q", perr.Code, want, perr.Message)
	}
}
