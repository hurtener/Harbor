package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsdurable "github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type admittedSessionsFixture struct {
	id        identity.Identity
	store     state.StateStore
	gate      *sessionadmission.Gate
	registry  *sessions.Registry
	projector *sessionsprotocol.ListerProjector
	eraser    *sessions.CascadeEraser
	redactor  audit.Redactor
	broad     context.Context
}

func newAdmittedSessionsFixture(t *testing.T) *admittedSessionsFixture {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	red := auditpatterns.New()
	bus, err := eventsdurable.New(t.Context(), config.EventsConfig{Driver: "durable", LegacyWritersDrained: true,
		MaxSubscribersPerSession: 16, SubscriberBufferSize: 64, IdleTimeout: time.Minute, DropWindow: time.Second, ReplayBufferSize: 64}, red, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	registry, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.CloseRegistry(context.Background()) })
	arts, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = arts.Close(context.Background()) })
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{Registry: registry, State: store, Artifacts: arts, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := sessionsprotocol.NewListerProjector(registry)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := sessionadmission.New(store)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "admission-tenant", UserID: "admission-user", SessionID: "admission-session"}
	base, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	broad := auth.WithTokenAuthority(auth.WithScopes(base, []auth.Scope{auth.ScopeAdmin}), auth.TokenAuthority{Issuer: "https://admission.test", Subject: "coordinator-a"})
	broad = sessionadmission.WithGate(broad, gate)
	if _, err := registry.Open(broad, id.SessionID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Enroll(broad, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	return &admittedSessionsFixture{id: id, store: store, gate: gate, registry: registry, projector: projector, eraser: eraser, redactor: red, broad: broad}
}

func (f *admittedSessionsFixture) current(method methods.Method) context.Context {
	return auth.WithSessionAdmission(auth.WithMethodReach(f.broad, []methods.Method{method}),
		&auth.SessionAdmissionAuthority{Epoch: 1, Coordinator: "coordinator-a", Identity: f.id})
}
func (f *admittedSessionsFixture) wire() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID}
}
func admissionScopeDenied(t *testing.T, err error) {
	t.Helper()
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("expected scoped admission denial: %v", err)
	}
}

func TestSessionAdmission_SetTitle_RealRegistryAndValidationRelease(t *testing.T) {
	f := newAdmittedSessionsFixture(t)
	svc, err := sessionsprotocol.NewService(f.projector, sessionsprotocol.WithTitleSetter(f.registry))
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.SessionsSetTitleRequest{Identity: f.wire(), SessionID: f.id.SessionID, Title: "Current title"}
	_, err = svc.SetTitle(f.broad, request)
	admissionScopeDenied(t, err)
	before, err := f.registry.Inspect(f.broad, f.id.SessionID)
	if err != nil || before.Title != "" {
		t.Fatalf("old bearer changed actual title: %#v, %v", before, err)
	}
	current := f.current(methods.MethodSessionsSetTitle)
	invalid := request
	invalid.Title = "invalid\ntitle"
	if _, err := svc.SetTitle(current, invalid); !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("title validation: %v", err)
	}
	if _, err := svc.SetTitle(current, request); err != nil {
		t.Fatalf("validation rejection latched title admission: %v", err)
	}
	after, err := f.registry.Inspect(f.broad, f.id.SessionID)
	if err != nil || after.Title != request.Title {
		t.Fatalf("narrow authority did not set actual title: %#v, %v", after, err)
	}
	if _, err := f.gate.Enroll(f.broad, f.id, 1, 2); err != nil {
		t.Fatalf("title success retained acceptance: %v", err)
	}
}

func TestSessionAdmission_Delete_RealCascadePreservesAuthorityRecord(t *testing.T) {
	f := newAdmittedSessionsFixture(t)
	svc, err := sessionsprotocol.NewService(f.projector, sessionsprotocol.WithEraser(f.eraser))
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.SessionsDeleteRequest{Identity: f.wire()}
	_, err = svc.Delete(f.broad, request)
	admissionScopeDenied(t, err)
	if _, err := f.registry.Get(f.broad, f.id.SessionID); err != nil {
		t.Fatalf("old bearer erased actual session: %v", err)
	}
	response, err := svc.Delete(f.current(methods.MethodSessionsDelete), request)
	if err != nil || !response.Deleted {
		t.Fatalf("narrow delete did not run actual cascade: %#v, %v", response, err)
	}
	if _, err := f.registry.Get(f.broad, f.id.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Fatalf("session survives deletion: %v", err)
	}
	policy, err := f.gate.PolicyFor(f.broad, f.id)
	if err != nil || policy.Epoch != 1 {
		t.Fatalf("scope deletion erased admission authority: %#v, %v", policy, err)
	}
	if _, err := f.gate.Enroll(f.broad, f.id, 1, 2); err != nil {
		t.Fatalf("delete success retained acceptance: %v", err)
	}
}

// This adapter invokes the actual durable reconciliation primitive; it does
// not duplicate journal/recovery behavior from the runtime-owned service.
type admissionRetainedReconciler struct {
	store    state.StateStore
	redactor audit.Redactor
}

func (r admissionRetainedReconciler) ReconcileContext(ctx context.Context, id identity.Identity, run string) error {
	return sessionmemory.ReconcileRetainedRun(ctx, r.store, r.redactor, identity.Quadruple{Identity: id, RunID: run}, 4, nil)
}

func TestSessionAdmission_ReconcileContext_RealJournalAndValidationRelease(t *testing.T) {
	f := newAdmittedSessionsFixture(t)
	q := identity.Quadruple{Identity: f.id, RunID: "settled-source"}
	base := planner.RunContext{Quadruple: q, Query: "save once", Trajectory: &planner.Trajectory{Query: "save once"}}
	old, err := sessionmemory.BeginRetainedRun(f.broad, f.store, f.redactor, q, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := old.Start(f.broad, base); err != nil {
		t.Fatal(err)
	}
	step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "settled-save", Args: json.RawMessage(`{"id":"document"}`)}}
	if err := old.BeforeDispatch(f.broad, base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"saved":true}`)
	if err := old.AfterDispatch(f.broad, base, step); err != nil {
		t.Fatal(err)
	}
	svc, err := sessionsprotocol.NewService(f.projector, sessionsprotocol.WithContextReconciler(admissionRetainedReconciler{store: f.store, redactor: f.redactor}))
	if err != nil {
		t.Fatal(err)
	}
	request := prototypes.SessionsReconcileContextRequest{Identity: f.wire(), SourceRunID: q.RunID}
	_, err = svc.ReconcileContext(f.broad, request)
	admissionScopeDenied(t, err)
	current := f.current(methods.MethodSessionsReconcileContext)
	invalid := request
	invalid.SourceRunID = " "
	if _, err := svc.ReconcileContext(current, invalid); !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("reconcile validation: %v", err)
	}
	response, err := svc.ReconcileContext(current, request)
	if err != nil || !response.Reconciled {
		t.Fatalf("narrow reconciliation or rejection release failed: %#v, %v", response, err)
	}
	if err := old.BeforeDispatch(f.broad, base, step); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
		t.Fatalf("real source journal was not fenced: %v", err)
	}
	if _, err := f.gate.Enroll(f.broad, f.id, 1, 2); err != nil {
		t.Fatalf("reconciliation success retained acceptance: %v", err)
	}
}

type uncertainAdmissionTitleSetter struct {
	registry *sessions.Registry
	failure  error
}

func (s uncertainAdmissionTitleSetter) SetTitle(ctx context.Context, id string, owner identity.Identity, title string) error {
	if err := s.registry.SetTitle(ctx, id, owner, title); err != nil {
		return err
	}
	return s.failure
}

func TestSessionAdmission_SetTitle_UnknownDomainOutcomeRetainsFence(t *testing.T) {
	f := newAdmittedSessionsFixture(t)
	unknown := errors.New("title driver response lost after accepted update")
	svc, err := sessionsprotocol.NewService(f.projector, sessionsprotocol.WithTitleSetter(uncertainAdmissionTitleSetter{registry: f.registry, failure: unknown}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SetTitle(f.current(methods.MethodSessionsSetTitle), prototypes.SessionsSetTitleRequest{Identity: f.wire(), SessionID: f.id.SessionID, Title: "accepted title"})
	if !errors.Is(err, unknown) {
		t.Fatalf("unknown domain error lost: %v", err)
	}
	actual, err := f.registry.Inspect(f.broad, f.id.SessionID)
	if err != nil || actual.Title != "accepted title" {
		t.Fatalf("fixture must have real accepted title: %#v, %v", actual, err)
	}
	if _, err := f.gate.Enroll(f.broad, f.id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("unknown title outcome released fence: %v", err)
	}
}
