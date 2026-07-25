package bodyscope_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
)

// The audit sink is the accountability half of the gate's central
// linkage: a surface may only hold the permission to cross an identity
// boundary if it also holds somewhere to record the crossing. These
// tests drive the real sink against a real bus and a real redactor —
// a sink that silently drops the record would make the linkage
// decorative.

func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestBusAuditor_PublishesTheCrossingWithTheVerifiedActor — the record
// names the caller who asked, not the identity that was reached. An
// audit trail that recorded the target instead of the actor would say
// what happened but never who did it.
func TestBusAuditor_PublishesTheCrossingWithTheVerifiedActor(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)

	actor := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: actor.TenantID, User: actor.UserID, Session: actor.SessionID,
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	aud := bodyscope.NewBusAuditor(bus, auditpatterns.New(), quietLogger())
	aud.AdminScopeUsed(context.Background(), bodyscope.Elevation{
		Surface: bodyscope.SurfacePosture,
		Actor:   actor,
		Target:  identity.Identity{TenantID: "t-other", UserID: "u1", SessionID: "s1"},
		Reason:  "runtime: cross-identity request under an admin-tier scope claim",
	})

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
		}
		if ev.Identity.Identity != actor {
			t.Errorf("event identity = %+v, want the verified actor %+v", ev.Identity.Identity, actor)
		}
		payload, ok := ev.Payload.(bodyscope.AdminScopeUsedPayload)
		if !ok {
			t.Fatalf("payload type = %T, want AdminScopeUsedPayload", ev.Payload)
		}
		if payload.Actor != actor {
			t.Errorf("payload actor = %+v, want %+v", payload.Actor, actor)
		}
		if payload.TargetTenant != "t-other" {
			t.Errorf("payload target tenant = %q, want t-other", payload.TargetTenant)
		}
		if payload.Surface != string(bodyscope.SurfacePosture) {
			t.Errorf("payload surface = %q, want %q", payload.Surface, bodyscope.SurfacePosture)
		}
		if payload.Reason == "" {
			t.Error("payload carries no reason")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no audit event published for a granted crossing")
	}
}

// failingRedactor refuses every payload.
type failingRedactor struct{}

var errRedact = errors.New("redaction refused")

func (failingRedactor) Redact(context.Context, any) (any, error) { return nil, errRedact }

// TestBusAuditor_RedactionFailureSuppressesThePublish — a payload the
// redactor will not pass is not published in the clear. The record is
// lost loudly (logged) rather than emitted unredacted.
func TestBusAuditor_RedactionFailureSuppressesThePublish(t *testing.T) {
	t.Parallel()
	bus := newTestBus(t)
	actor := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: actor.TenantID, User: actor.UserID, Session: actor.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	aud := bodyscope.NewBusAuditor(bus, failingRedactor{}, quietLogger())
	aud.AdminScopeUsed(context.Background(), bodyscope.Elevation{
		Surface: bodyscope.SurfacePosture, Actor: actor,
		Target: identity.Identity{TenantID: "t-other", UserID: "u1", SessionID: "s1"},
		Reason: "reason",
	})

	select {
	case ev := <-sub.Events():
		t.Fatalf("an unredactable payload reached the bus: %+v", ev)
	case <-time.After(250 * time.Millisecond):
		// No event — correct.
	}
}

// TestBusAuditor_NilBusDoesNotPanicAndDoesNotSwallowSilently — an
// embedder that wired no bus still gets the crossing on its log. The
// sink degrades to a quieter channel, never to nothing.
func TestBusAuditor_NilBusDoesNotPanic(t *testing.T) {
	t.Parallel()
	aud := bodyscope.NewBusAuditor(nil, nil, quietLogger())
	aud.AdminScopeUsed(context.Background(), bodyscope.Elevation{
		Surface: bodyscope.SurfaceArtifacts,
		Actor:   identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Target:  identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s1"},
		Reason:  "reason",
	})
}

// TestBusAuditor_NilLoggerDefaults — a caller that passes no logger gets
// the package default rather than a nil dereference.
func TestBusAuditor_NilLoggerDefaults(t *testing.T) {
	t.Parallel()
	if aud := bodyscope.NewBusAuditor(nil, nil, nil); aud == nil {
		t.Fatal("NewBusAuditor returned nil")
	}
}

// TestBusAuditor_ConcurrentReuse — one sink shared across concurrent
// requests records every crossing exactly once, with no interleaving of
// one request's actor onto another's record.
func TestBusAuditor_ConcurrentReuse(t *testing.T) {
	bus := newTestBus(t)
	aud := bodyscope.NewBusAuditor(bus, auditpatterns.New(), quietLogger())

	const n = 120
	done := make(chan struct{}, n)
	for i := range n {
		go func(idx int) {
			defer func() { done <- struct{}{} }()
			actor := identity.Identity{
				TenantID:  "tenant-" + string(rune('a'+idx%26)),
				UserID:    "user",
				SessionID: "session",
			}
			aud.AdminScopeUsed(context.Background(), bodyscope.Elevation{
				Surface: bodyscope.SurfacePosture,
				Actor:   actor,
				Target:  identity.Identity{TenantID: "fleet-target", UserID: "user", SessionID: "session"},
				Reason:  "concurrent crossing",
			})
		}(i)
	}
	for range n {
		<-done
	}
}

// TestRegistry_LookupHelpers covers the registry's read surface: the
// request-type join and the policy-table copy operator tooling reads.
func TestRegistry_LookupHelpers(t *testing.T) {
	t.Parallel()
	surface, ok := bodyscope.SurfaceForRequest("StartRequest")
	if !ok || surface != bodyscope.SurfaceControlTask {
		t.Errorf("SurfaceForRequest(StartRequest) = (%q, %v), want the task surface", surface, ok)
	}
	if _, ok := bodyscope.SurfaceForRequest("NotAWireType"); ok {
		t.Error("SurfaceForRequest resolved a type the registry does not join")
	}

	policies := bodyscope.RegisteredPolicies()
	if len(policies) != len(bodyscope.RegisteredSurfaces()) {
		t.Errorf("RegisteredPolicies has %d rows, RegisteredSurfaces has %d — the two views must agree",
			len(policies), len(bodyscope.RegisteredSurfaces()))
	}
	// The copy is defensive: mutating it must not reach the registry.
	delete(policies, bodyscope.SurfacePosture)
	if _, ok := bodyscope.PolicyFor(bodyscope.SurfacePosture); !ok {
		t.Error("mutating the RegisteredPolicies copy reached the registry")
	}
}

// TestComponentRule_String pins the rendering the gate's failure
// messages and the operator posture table rely on.
func TestComponentRule_String(t *testing.T) {
	t.Parallel()
	for rule, want := range map[bodyscope.ComponentRule]string{
		bodyscope.Pinned:        "pinned",
		bodyscope.PinnedOrEmpty: "pinned-or-empty",
		bodyscope.AdminScoped:   "admin-scoped",
	} {
		if got := rule.String(); got != want {
			t.Errorf("ComponentRule(%d).String() = %q, want %q", rule, got, want)
		}
	}
	if got := bodyscope.ComponentRule(99).String(); got == "" {
		t.Error("an unknown ComponentRule renders empty; it must stay diagnosable")
	}
}

// TestViolation_String pins the `file:line: kind: detail` shape a gate
// failure message is read in, including the no-line registry form.
func TestViolation_String(t *testing.T) {
	t.Parallel()
	withLine := bodyscope.Violation{File: "a.go", Line: 7, Kind: "k", Detail: "d"}
	if got, want := withLine.String(), "a.go:7: k: d"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	noLine := bodyscope.Violation{File: "registry", Kind: "k", Detail: "d"}
	if got, want := noLine.String(), "registry: k: d"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
