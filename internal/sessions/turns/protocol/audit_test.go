package protocol

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// This file covers the operations-lane edge paths: typed not-found,
// the audit emit's redactor / bus / no-bus branches, and the loud
// projector-failure wrap (which never falls back).

func TestService_Get_Operations_NotFoundPassthrough(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	// A missing turn under the operations lane answers the SAME typed
	// not-found as the consumer lane — never an existence oracle.
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-missing", Projection: ProjectionOperations}); !errIs(err, ErrTurnNotFound) {
		t.Fatalf("ops get missing turn: err = %v, want ErrTurnNotFound", err)
	}
}

// passthroughRedactor accepts every payload unchanged.
type passthroughRedactor struct{}

func (passthroughRedactor) Redact(_ context.Context, payload any) (any, error) { return payload, nil }

// erroringRedactor refuses every payload — the "do not emit" contract.
type erroringRedactor struct{}

func (erroringRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("redaction failed")
}

// failingBus publishes with an error — the emit logs and the read still
// succeeds (the admin action is never allowed to break the read it
// records).
type failingBus struct{}

func (failingBus) Publish(context.Context, events.Event) error { return errors.New("publish failed") }
func (failingBus) PublishLive(context.Context, events.Event) error { return errors.New("live publish failed") }

func (failingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, errors.New("not wired")
}

func (failingBus) Close(context.Context) error { return nil }

// widenedOpsRead performs one widened (sibling-session) operations read
// on a Service with the given options and returns the bus it recorded
// onto.
func widenedOpsRead(t *testing.T, opts ...Option) *recordingBus {
	t.Helper()
	svc, st, _, bus := newTestService(t, opts...)
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, ""))
	other := identity.Identity{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, SessionID: "session-other"}
	mustSeedRow(t, st, other, fixtureRow("turn-x", turns.StatusComplete, ""))
	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-other", TaskID: "turn-x", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("widened ops read: %v", err)
	}
	return bus
}

func TestService_Get_Operations_Audit_RedactorPassesThrough(t *testing.T) {
	bus := widenedOpsRead(t, WithRedactor(passthroughRedactor{}))
	if n := len(bus.adminEvents()); n != 1 {
		t.Fatalf("redactor pass-through emitted %d admin audits, want 1", n)
	}
}

func TestService_Get_Operations_Audit_RedactionFailureLogsOnly(t *testing.T) {
	// A redaction refusal means "do not emit" — the widened read still
	// succeeds, the event is NOT published, and the failure is logged
	// loudly (never a silent drop, never an unredacted publish).
	bus := widenedOpsRead(t, WithRedactor(erroringRedactor{}))
	if n := len(bus.adminEvents()); n != 0 {
		t.Fatalf("redaction-failed emit published %d admin audits, want 0", n)
	}
}

func TestService_Get_Operations_Audit_PublishFailureDoesNotBreakRead(t *testing.T) {
	// A bus publish failure is logged, never fatal to the read the
	// audit records.
	svc, st, _, _ := newTestService(t, WithBus(failingBus{}))
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, ""))
	other := identity.Identity{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, SessionID: "session-other"}
	mustSeedRow(t, st, other, fixtureRow("turn-x", turns.StatusComplete, ""))
	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-other", TaskID: "turn-x", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("widened ops read with failing bus: %v", err)
	}
}

func TestService_Get_Operations_Audit_NoBusLogsOnly(t *testing.T) {
	// With no bus wired, the widened read is logged at Info — never
	// fully silent. The service is constructed without the default
	// recording bus (WithBus(nil) means "not supplied").
	st := newMemStore(true)
	proj := mustProjector(t, st)
	svc, err := NewService(proj,
		WithSessionReachAuthorizer(auth.NewSessionReachAuthorizer()),
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	mustSeedRow(t, st, fixtureID, fixtureRow("turn-a", turns.StatusComplete, ""))
	other := identity.Identity{TenantID: fixtureID.TenantID, UserID: fixtureID.UserID, SessionID: "session-other"}
	mustSeedRow(t, st, other, fixtureRow("turn-x", turns.StatusComplete, ""))
	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-other", TaskID: "turn-x", Projection: ProjectionOperations}); err != nil {
		t.Fatalf("widened ops read without bus: %v", err)
	}
}

// opsFailingProjector fails OpsTurn with a NON-not-found error — the
// loud no-fallback gate for the operations lane.
type opsFailingProjector struct{ failingProjector }

func (opsFailingProjector) OpsTurn(context.Context, identity.Identity, turns.TurnID) (turns.OpsTurnRow, error) {
	return turns.OpsTurnRow{}, errors.New("boom")
}

func TestService_Get_Operations_ProjectorFailure_FailsLoudNoFallback(t *testing.T) {
	svc, err := NewService(opsFailingProjector{},
		WithAgentReachAuthorizer(auth.NewAgentReachAuthorizer()),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := auth.WithScopes(verifiedCtx(t, fixtureID), []auth.Scope{auth.ScopeAdmin})
	if _, err := svc.Get(ctx, GetRequest{SessionID: "session-1", TaskID: "turn-a", Projection: ProjectionOperations}); err == nil {
		t.Fatal("ops get over a failing projector must fail loud")
	}
	if errIs(err, ErrTurnNotFound) {
		t.Fatalf("ops projector failure must NOT be masked as not-found: %v", err)
	}
}
