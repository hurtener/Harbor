package protocol_test

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// surfaceFixture bundles a ControlSurface with the real runtime
// dependencies behind it — a real tasks.TaskRegistry (inprocess over a
// real in-mem state.StateStore) + a real steering.Registry. No mocks at
// the seam (CLAUDE.md §17.3 #1 — this is what makes the surface tests
// real consumers).
type surfaceFixture struct {
	surface  *protocol.ControlSurface
	tasks    tasks.TaskRegistry
	steering *steering.Registry
	bus      events.EventBus
	state    state.StateStore
}

func newSurfaceFixture(t *testing.T) *surfaceFixture {
	t.Helper()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1000,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	steerReg := steering.NewRegistry()

	surface, err := protocol.NewControlSurface(taskReg, steerReg)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	t.Cleanup(func() {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
	})

	return &surfaceFixture{
		surface:  surface,
		tasks:    taskReg,
		steering: steerReg,
		bus:      bus,
		state:    store,
	}
}

// testRun is a documented dummy run quadruple — no secrets.
func testRun(run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-a",
			UserID:    "user-1",
			SessionID: "session-x",
		},
		RunID: run,
	}
}

// codeOf extracts the stable protocol error Code from err, failing the
// test if err is not a *protoerrors.Error.
func codeOf(t *testing.T, err error) protoerrors.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected a *protocol/errors.Error, got nil")
	}
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected a *protocol/errors.Error, got %T: %v", err, err)
	}
	return pe.Code
}

func TestNewControlSurface_FailsClosedOnNilDependency(t *testing.T) {
	steerReg := steering.NewRegistry()

	if _, err := protocol.NewControlSurface(nil, steerReg); !stderrors.Is(err, protocol.ErrMisconfigured) {
		t.Errorf("NewControlSurface(nil tasks) error = %v, want ErrMisconfigured", err)
	}
	// A real task registry is needed for the nil-steering case.
	fx := newSurfaceFixture(t)
	if _, err := protocol.NewControlSurface(fx.tasks, nil); !stderrors.Is(err, protocol.ErrMisconfigured) {
		t.Errorf("NewControlSurface(nil steering) error = %v, want ErrMisconfigured", err)
	}
}

func TestDispatch_UnknownMethod_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	_, err := fx.surface.Dispatch(context.Background(), methods.Method("teleport"), &types.StartRequest{})
	if got := codeOf(t, err); got != protoerrors.CodeUnknownMethod {
		t.Fatalf("Dispatch(unknown method) code = %q, want %q", got, protoerrors.CodeUnknownMethod)
	}
}

func TestDispatch_Start_RoutesToTaskRegistry(t *testing.T) {
	fx := newSurfaceFixture(t)
	resp, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
		Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"},
		Query:    "do the thing",
	})
	if err != nil {
		t.Fatalf("Dispatch(start) error = %v", err)
	}
	sr, ok := resp.(*types.StartResponse)
	if !ok {
		t.Fatalf("Dispatch(start) returned %T, want *types.StartResponse", resp)
	}
	if sr.TaskID == "" {
		t.Fatal("Dispatch(start) returned an empty TaskID")
	}
	if sr.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("StartResponse.ProtocolVersion = %q, want %q", sr.ProtocolVersion, types.ProtocolVersion)
	}
	// The task must actually exist in the registry — proves the surface
	// reached the real Phase 20 TaskRegistry, not a stub.
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "tenant-a", UserID: "user-1", SessionID: "session-x",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got, err := fx.tasks.Get(ctx, tasks.TaskID(sr.TaskID))
	if err != nil {
		t.Fatalf("tasks.Get(%q): %v — start did not reach the real registry", sr.TaskID, err)
	}
	if got.Kind != tasks.KindForeground {
		t.Errorf("spawned task Kind = %q, want %q", got.Kind, tasks.KindForeground)
	}
}

func TestDispatch_Start_Idempotency(t *testing.T) {
	fx := newSurfaceFixture(t)
	req := &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"},
		Query:          "idempotent run",
		IdempotencyKey: "key-abc",
	}
	first, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, req)
	if err != nil {
		t.Fatalf("first Dispatch(start): %v", err)
	}
	second, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, req)
	if err != nil {
		t.Fatalf("second Dispatch(start): %v", err)
	}
	f := first.(*types.StartResponse)
	s := second.(*types.StartResponse)
	if f.TaskID != s.TaskID {
		t.Fatalf("idempotency: first TaskID %q != second %q", f.TaskID, s.TaskID)
	}
	if !s.Reused {
		t.Error("second Dispatch(start) with same idempotency key: Reused = false, want true")
	}
}

func TestDispatch_Start_WrongRequestType_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	// A *ControlRequest handed to `start` is a wire-type mismatch.
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.ControlRequest{})
	if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("Dispatch(start, wrong type) code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}
	// A nil request.
	_, err = fx.surface.Dispatch(context.Background(), methods.MethodStart, nil)
	if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("Dispatch(start, nil) code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}
}

func TestDispatch_Start_IncompleteIdentity_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	cases := map[string]types.IdentityScope{
		"no tenant":  {User: "u", Session: "s"},
		"no user":    {Tenant: "t", Session: "s"},
		"no session": {Tenant: "t", User: "u"},
		"all empty":  {},
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{Identity: id})
			if got := codeOf(t, err); got != protoerrors.CodeIdentityRequired {
				t.Fatalf("Dispatch(start, %s) code = %q, want %q", name, got, protoerrors.CodeIdentityRequired)
			}
		})
	}
}

func TestDispatch_Control_IncompleteIdentity_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	// Missing triple component.
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{User: "u", Session: "s", Run: "run-1", Scope: "owner_user"},
	})
	if got := codeOf(t, err); got != protoerrors.CodeIdentityRequired {
		t.Fatalf("Dispatch(cancel, no tenant) code = %q, want %q", got, protoerrors.CodeIdentityRequired)
	}
	// Missing run id — a steering control must target a specific run.
	_, err = fx.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s", Scope: "owner_user"},
	})
	if got := codeOf(t, err); got != protoerrors.CodeIdentityRequired {
		t.Fatalf("Dispatch(cancel, no run) code = %q, want %q", got, protoerrors.CodeIdentityRequired)
	}
}

func TestDispatch_Control_WrongRequestType_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodPause, &types.StartRequest{})
	if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("Dispatch(pause, wrong type) code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}
}

func TestDispatch_Control_UnknownScope_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-scope")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "superadmin", // not a canonical steering scope
		},
	})
	if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("Dispatch(cancel, unknown scope) code = %q, want %q", got, protoerrors.CodeScopeMismatch)
	}
}

func TestDispatch_Control_NoLiveInbox_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	// No steering.Open for this run — there is no inbox.
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: "tenant-a", User: "user-1", Session: "session-x", Run: "run-ghost",
			Scope: "owner_user",
		},
	})
	if got := codeOf(t, err); got != protoerrors.CodeNotFound {
		t.Fatalf("Dispatch(cancel, no inbox) code = %q, want %q", got, protoerrors.CodeNotFound)
	}
}

func TestDispatch_Control_ScopeBelowMinimum_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-lowscope")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	// PRIORITIZE requires admin (RFC §6.3). A session_user caller is
	// below the minimum — steering.CheckScope rejects it, the surface
	// maps it to CodeScopeMismatch.
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodPrioritize, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "session_user",
		},
		Payload: map[string]any{"priority": 9},
	})
	if got := codeOf(t, err); got != protoerrors.CodeScopeMismatch {
		t.Fatalf("Dispatch(prioritize, session_user) code = %q, want %q", got, protoerrors.CodeScopeMismatch)
	}
}

func TestDispatch_Control_OversizePayload_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	run := testRun("run-oversize")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	// A string leaf well over the RFC §6.3 4096-rune cap — Phase 52's
	// ValidatePayload (inside Inbox.Enqueue) rejects it; the surface
	// maps steering.ErrPayloadInvalid to CodePayloadInvalid.
	huge := make([]byte, 5000)
	for i := range huge {
		huge[i] = 'x'
	}
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodInjectContext, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: run.TenantID, User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "session_user",
		},
		Payload: map[string]any{"note": string(huge)},
	})
	if got := codeOf(t, err); got != protoerrors.CodePayloadInvalid {
		t.Fatalf("Dispatch(inject_context, oversize) code = %q, want %q", got, protoerrors.CodePayloadInvalid)
	}
}

func TestDispatch_Control_CrossTenantNonAdmin_FailsClosed(t *testing.T) {
	fx := newSurfaceFixture(t)
	// The run belongs to tenant-a; a caller authenticating under
	// tenant-b without admin is a cross-tenant steering attempt. The
	// surface sets CallerTenant from the request's tenant — but the run
	// inbox's identity is tenant-a, so steering.CheckScope's cross-tenant
	// gate fires. To exercise this we open the inbox under tenant-a but
	// send the control with tenant-b in the identity scope: the inbox
	// Enqueue rejects it because the event identity != inbox identity.
	run := testRun("run-xtenant")
	if _, err := fx.steering.Open(run); err != nil {
		t.Fatalf("steering.Open: %v", err)
	}
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodCancel, &types.ControlRequest{
		Identity: types.IdentityScope{
			Tenant: "tenant-b", User: run.UserID, Session: run.SessionID, Run: run.RunID,
			Scope: "owner_user",
		},
	})
	// The inbox for run-xtenant is keyed under tenant-a; an event with
	// tenant-b identity cannot be looked up (CodeNotFound) — the inbox
	// Registry.Lookup keys on the full quadruple.
	if got := codeOf(t, err); got != protoerrors.CodeNotFound {
		t.Fatalf("Dispatch(cancel, cross-tenant) code = %q, want %q", got, protoerrors.CodeNotFound)
	}
}
