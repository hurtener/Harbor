package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestSessionAdmission_SetOverrides_NarrowsOldAuthorityAndReleasesValidation(t *testing.T) {
	durable, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close(context.Background()) })
	gate, err := sessionadmission.New(durable)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}
	base, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	broad := auth.WithTokenAuthority(auth.WithScopes(base, []auth.Scope{auth.ScopeAdmin}), auth.TokenAuthority{Issuer: "https://admission.test", Subject: "coordinator-a"})
	broad = sessionadmission.WithGate(broad, gate)
	svc, store := newService(t)
	request := wireReq(prototypes.RunOverrides{SessionID: testSession, Temperature: f64Ptr(0.2)})
	if _, err := svc.SetOverrides(broad, request); err != nil {
		t.Fatalf("same broad authority before enrollment: %v", err)
	}
	if _, err := gate.Enroll(broad, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	request.Overrides.Temperature = f64Ptr(0.7)
	_, err = svc.SetOverrides(broad, request)
	var perr *protoerrors.Error
	if !errors.As(err, &perr) || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("old broad override authority after enrollment: %v", err)
	}
	pending, ok := store.Peek(id)
	if !ok || pending.Temperature == nil || *pending.Temperature != 0.2 {
		t.Fatalf("denied call changed actual override store: %#v", pending)
	}
	current := auth.WithSessionAdmission(auth.WithMethodReach(broad, []methods.Method{methods.MethodRunsSetOverrides}),
		&auth.SessionAdmissionAuthority{Epoch: 1, Coordinator: "coordinator-a", Identity: id})
	invalid := request
	invalid.Overrides.Temperature = f64Ptr(-1)
	if _, err := svc.SetOverrides(current, invalid); err == nil {
		t.Fatal("malformed override accepted")
	}
	if _, err := svc.SetOverrides(current, request); err != nil {
		t.Fatalf("known validation rejection latched admission: %v", err)
	}
	pending, ok = store.Peek(id)
	if !ok || pending.Temperature == nil || *pending.Temperature != 0.7 {
		t.Fatalf("current narrow authority did not update actual store: %#v", pending)
	}
	if _, err := gate.Enroll(broad, id, 1, 2); err != nil {
		t.Fatalf("successful override left acceptance pending: %v", err)
	}
}

func TestSessionAdmission_SetOverrides_CanceledBeforeAdmissionDoesNotWrite(t *testing.T) {
	// Cancellation before the gate can reserve acceptance must not write the
	// actual override store. Unknown outcomes after acceptance are covered at
	// the gate and by the domain mutation regression tests.
	durable, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close(context.Background()) })
	gate, err := sessionadmission.New(durable)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: testTenant, UserID: testUser, SessionID: testSession}
	base, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := sessionadmission.WithGate(base, gate)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	svc, store := newService(t)
	if _, err := svc.SetOverrides(canceled, wireReq(prototypes.RunOverrides{SessionID: testSession, Temperature: f64Ptr(0.3)})); err == nil {
		t.Fatal("canceled request accepted")
	}
	if _, ok := store.Peek(id); ok {
		t.Fatal("canceled request changed override state")
	}
}
