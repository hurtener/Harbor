package events_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"

	// Side-effect import: register the inmem driver.
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

func validCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60_000_000_000, // 60s
		DropWindow:               1_000_000_000,  // 1s
	}
}

func TestEventTypes_Exhaustiveness(t *testing.T) {
	expect := []events.EventType{
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
		events.EventTypeBusDropped,
		events.EventTypeBusSubscriptionIdleClosed,
		events.EventTypeAuditRedactionFailed,
		events.EventTypeAdminScopeUsed,
		events.EventTypeGovernanceBudgetExceeded,
		events.EventTypeGovernanceRateLimited,
	}
	for _, et := range expect {
		if !events.IsValidEventType(et) {
			t.Errorf("EventType %q not in canonical registry", et)
		}
	}
	got := events.EventTypes()
	for _, et := range expect {
		found := false
		for _, g := range got {
			if g == et {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("EventType %q missing from EventTypes() snapshot", et)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("EventTypes() not sorted: %v >= %v", got[i-1], got[i])
		}
	}
}

func TestIsValidEventType_RejectsUnknown(t *testing.T) {
	if events.IsValidEventType("not.real.event") {
		t.Error("IsValidEventType returned true for unknown type")
	}
}

func TestValidateEvent_Cases(t *testing.T) {
	id := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
		RunID:    "R",
	}
	cases := []struct {
		name string
		ev   events.Event
		want error
	}{
		{
			"happy",
			events.Event{Type: events.EventTypeRuntimeError, Identity: id, Payload: events.SubscriptionIdleClosedPayload{}},
			nil,
		},
		{
			"unknown type",
			events.Event{Type: "made.up", Identity: id, Payload: events.BusDroppedPayload{}},
			events.ErrUnknownEventType,
		},
		{
			"missing tenant",
			events.Event{Type: events.EventTypeRuntimeError, Identity: identity.Quadruple{Identity: identity.Identity{UserID: "U", SessionID: "S"}}, Payload: events.BusDroppedPayload{}},
			events.ErrIdentityRequired,
		},
		{
			"sequence pre-filled",
			events.Event{Type: events.EventTypeRuntimeError, Identity: id, Sequence: 42, Payload: events.BusDroppedPayload{}},
			events.ErrSequenceProvided,
		},
		{
			"nil payload",
			events.Event{Type: events.EventTypeRuntimeError, Identity: id, Payload: nil},
			events.ErrInvalidEvent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := events.ValidateEvent(tc.ev)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateEvent returned %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestFilter_HasFullTriple(t *testing.T) {
	if (events.Filter{Tenant: "T", User: "U", Session: "S"}).HasFullTriple() == false {
		t.Error("full filter reported as incomplete")
	}
	if (events.Filter{Tenant: "T", User: "U"}).HasFullTriple() == true {
		t.Error("partial filter reported as complete")
	}
}

func TestFilter_Matches(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
	ev := events.Event{Type: events.EventTypeRuntimeError, Identity: q}

	// Full-match.
	f := events.Filter{Tenant: "T", User: "U", Session: "S"}
	if !f.Matches(ev) {
		t.Error("full-triple filter did not match own identity")
	}
	// Wrong tenant.
	f2 := events.Filter{Tenant: "OTHER", User: "U", Session: "S"}
	if f2.Matches(ev) {
		t.Error("wrong-tenant filter matched")
	}
	// Wrong user.
	f3 := events.Filter{Tenant: "T", User: "X", Session: "S"}
	if f3.Matches(ev) {
		t.Error("wrong-user filter matched")
	}
	// Wrong session.
	f4 := events.Filter{Tenant: "T", User: "U", Session: "X"}
	if f4.Matches(ev) {
		t.Error("wrong-session filter matched")
	}
	// Admin matches anything.
	f5 := events.Filter{Admin: true}
	if !f5.Matches(ev) {
		t.Error("admin filter did not match")
	}
	// Type filter.
	f6 := events.Filter{Tenant: "T", User: "U", Session: "S", Types: []events.EventType{events.EventTypeRuntimeError}}
	if !f6.Matches(ev) {
		t.Error("matching type filter rejected event")
	}
	f7 := events.Filter{Tenant: "T", User: "U", Session: "S", Types: []events.EventType{events.EventTypeBusDropped}}
	if f7.Matches(ev) {
		t.Error("non-matching type filter accepted event")
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register did not panic on duplicate")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "inmem") {
			t.Errorf("panic message %v missing duplicate driver", r)
		}
	}()
	events.Register("inmem", func(_ config.EventsConfig, _ audit.Redactor) (events.EventBus, error) {
		return nil, nil
	})
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on empty name")
		}
	}()
	events.Register("", func(_ config.EventsConfig, _ audit.Redactor) (events.EventBus, error) { return nil, nil })
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register did not panic on nil factory")
		}
	}()
	events.Register("nil-factory-test", nil)
}

func TestRegisteredDrivers_ContainsInmem(t *testing.T) {
	got := events.RegisteredDrivers()
	found := false
	for _, n := range got {
		if n == "inmem" {
			found = true
		}
	}
	if !found {
		t.Errorf("inmem driver not in registered list: %v", got)
	}
}

func TestOpen_DefaultDriver(t *testing.T) {
	bus, err := events.Open(context.Background(), validCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	if bus == nil {
		t.Fatal("Open returned nil bus")
	}
}

// TestOpen_HonoursCfgDriver verifies Open routes by cfg.Driver, not
// just by DefaultDriver. Pins the seam for Phase 06's replay-equipped
// driver and Phase 57's durable-log driver — both will register
// alternative names that Open must select via cfg.
func TestOpen_HonoursCfgDriver(t *testing.T) {
	// Register a sentinel driver under a non-default name. Use a
	// unique name so this test doesn't conflict with concurrent
	// re-runs (registration is process-wide).
	const name = "test-honour-driver-ad9c2e"
	called := false
	events.Register(name, func(_ config.EventsConfig, _ audit.Redactor) (events.EventBus, error) {
		called = true
		return nil, errInvariantNotABus
	})
	cfg := validCfg()
	cfg.Driver = name
	_, err := events.Open(context.Background(), cfg, auditpatterns.New())
	if !called {
		t.Fatal("registered driver factory was never called")
	}
	if !errors.Is(err, errInvariantNotABus) {
		t.Fatalf("err=%v, want errors.Is errInvariantNotABus", err)
	}
}

// errInvariantNotABus is the sentinel the test driver returns to
// prove its factory ran.
var errInvariantNotABus = errors.New("test: not a real bus")

// TestErrAdminScopeRequired_Reserved compile-pins ErrAdminScopeRequired
// as live surface. Phase 05 trusts the Filter.Admin boolean (the
// cryptographic scope claim wires up in Phase 61 Protocol auth);
// until then this sentinel is reserved but the package keeps it
// stable so callers can errors.Is against it from day one.
func TestErrAdminScopeRequired_Reserved(t *testing.T) {
	if events.ErrAdminScopeRequired == nil {
		t.Fatal("ErrAdminScopeRequired is nil — sentinel reservation broken")
	}
	wrapped := errors.Join(events.ErrAdminScopeRequired, errors.New("scope claim missing"))
	if !errors.Is(wrapped, events.ErrAdminScopeRequired) {
		t.Fatal("errors.Is on ErrAdminScopeRequired no longer works")
	}
}

func TestOpen_NilRedactorErrors(t *testing.T) {
	_, err := events.Open(context.Background(), validCfg(), nil)
	if err == nil {
		t.Fatal("Open with nil redactor returned nil error")
	}
}

func TestOpenDriver_UnknownNameWrapsSentinel(t *testing.T) {
	_, err := events.OpenDriver("does-not-exist", validCfg(), auditpatterns.New())
	if err == nil {
		t.Fatal("OpenDriver returned nil error for unknown driver")
	}
	if !errors.Is(err, events.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "inmem") {
		t.Errorf("err=%q does not list registered drivers", err.Error())
	}
}

func TestWithBus_RoundTrip(t *testing.T) {
	bus, err := events.Open(context.Background(), validCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	ctx := events.WithBus(context.Background(), bus)
	got, ok := events.From(ctx)
	if !ok {
		t.Fatal("From returned ok=false after WithBus")
	}
	if got != bus {
		t.Errorf("From returned different bus")
	}
}

func TestFrom_AbsentReturnsZeroAndFalse(t *testing.T) {
	got, ok := events.From(context.Background())
	if ok {
		t.Errorf("From on bare ctx returned ok=true")
	}
	if got != nil {
		t.Errorf("From on bare ctx returned non-nil: %v", got)
	}
}

func TestMustFrom_PanicsWithSentinelOnAbsence(t *testing.T) {
	defer func() {
		v := recover()
		if v == nil {
			t.Fatal("MustFrom did not panic on bare ctx")
		}
		err, ok := v.(error)
		if !ok || !errors.Is(err, events.ErrBusClosed) {
			t.Fatalf("panic value %v is not ErrBusClosed", v)
		}
	}()
	_ = events.MustFrom(context.Background())
}

func TestSeal_ConcreteTypesImplementEventPayload(t *testing.T) {
	// Compile-time pin: every bus-internal payload satisfies the seal.
	var _ events.EventPayload = events.BusDroppedPayload{}
	var _ events.EventPayload = events.SubscriptionIdleClosedPayload{}
	var _ events.EventPayload = events.AuditRedactionFailedPayload{}
	var _ events.EventPayload = events.AdminScopeUsedPayload{}
	var _ events.EventPayload = events.RedactedMap{}

	// And SafePayload for the bus-internals.
	var _ events.SafePayload = events.BusDroppedPayload{}
	var _ events.SafePayload = events.SubscriptionIdleClosedPayload{}
	var _ events.SafePayload = events.AuditRedactionFailedPayload{}
	var _ events.SafePayload = events.AdminScopeUsedPayload{}

	// RedactedMap is NOT SafePayload — it's the post-redaction form.
	if _, ok := any(events.RedactedMap{}).(events.SafePayload); ok {
		t.Error("RedactedMap should NOT implement SafePayload")
	}
}
