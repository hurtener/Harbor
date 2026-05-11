package memory_test

// Registry-surface + sentinel-error unit tests for `internal/memory`.
// Driver + behaviour tests live alongside the driver under
// `internal/memory/drivers/inmem/`. The conformance suite lives at
// `internal/memory/conformancetest/`.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestRegister_PanicsOnEmptyName(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register with empty name did not panic")
		}
	}()
	memory.Register("", func(memory.ConfigSnapshot, memory.Deps) (memory.MemoryStore, error) {
		return nil, nil
	})
}

func TestRegister_PanicsOnNilFactory(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register with nil factory did not panic")
		}
	}()
	memory.Register("bogus-driver-name", nil)
}

func TestRegister_PanicsOnDuplicate(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register duplicate did not panic")
		}
	}()
	// "inmem" was registered by the blank import above; re-registering
	// must panic.
	memory.Register("inmem", func(memory.ConfigSnapshot, memory.Deps) (memory.MemoryStore, error) {
		return nil, nil
	})
}

func TestRegisteredDrivers_IncludesInMem(t *testing.T) {
	names := memory.RegisteredDrivers()
	found := false
	for _, n := range names {
		if n == "inmem" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("RegisteredDrivers()=%v, want to contain %q", names, "inmem")
	}
}

func TestOpen_UnknownDriver_WrapsErrUnknownDriver(t *testing.T) {
	deps := newTestDeps(t)
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "no-such-driver",
		Strategy: memory.StrategyNone,
	}, deps)
	if !errors.Is(err, memory.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
	if !strings.Contains(err.Error(), "registered:") {
		t.Errorf("err=%q does not list registered drivers", err)
	}
}

func TestOpen_MissingState_FailsLoudly(t *testing.T) {
	bus, cleanup := newTestBus(t)
	defer cleanup()
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{
		State: nil,
		Bus:   bus,
	})
	if err == nil || !strings.Contains(err.Error(), "Deps.State") {
		t.Fatalf("err=%v, want error mentioning Deps.State", err)
	}
}

func TestOpen_MissingBus_FailsLoudly(t *testing.T) {
	st, cleanup := newTestState(t)
	defer cleanup()
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{
		State: st,
		Bus:   nil,
	})
	if err == nil || !strings.Contains(err.Error(), "Deps.Bus") {
		t.Fatalf("err=%v, want error mentioning Deps.Bus", err)
	}
}

func TestOpen_DefaultsToInMemDriver(t *testing.T) {
	deps := newTestDeps(t)
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		// Driver intentionally empty — must default to "inmem".
		Strategy: memory.StrategyNone,
	}, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mem.Close(context.Background())
	// Confirm the resolved driver actually works against a valid identity.
	if _, err := mem.Health(context.Background(), validQuadruple()); err != nil {
		t.Errorf("Health on defaulted driver: %v", err)
	}
}

func TestOpen_StrategyNotImplemented_Truncation(t *testing.T) {
	deps := newTestDeps(t)
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyTruncation,
	}, deps)
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

func TestOpen_StrategyNotImplemented_RollingSummary(t *testing.T) {
	deps := newTestDeps(t)
	_, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyRollingSummary,
	}, deps)
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("err=%v, want errors.Is ErrStrategyNotImplemented", err)
	}
}

func TestValidateIdentity(t *testing.T) {
	cases := map[string]struct {
		in   identity.Quadruple
		want error
	}{
		"full triple": {
			in: identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
				RunID:    "r",
			},
			want: nil,
		},
		"empty run-id is fine": {
			in: identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			},
			want: nil,
		},
		"empty tenant": {
			in: identity.Quadruple{
				Identity: identity.Identity{UserID: "u", SessionID: "s"},
			},
			want: memory.ErrIdentityRequired,
		},
		"empty user": {
			in: identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", SessionID: "s"},
			},
			want: memory.ErrIdentityRequired,
		},
		"empty session": {
			in: identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u"},
			},
			want: memory.ErrIdentityRequired,
		},
		"empty everything": {
			in:   identity.Quadruple{},
			want: memory.ErrIdentityRequired,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := memory.ValidateIdentity(tc.in)
			if tc.want == nil {
				if got != nil {
					t.Errorf("ValidateIdentity: %v, want nil", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Errorf("ValidateIdentity: %v, want errors.Is %v", got, tc.want)
			}
		})
	}
}

func TestCtxHelpers_StoreRoundTrip(t *testing.T) {
	deps := newTestDeps(t)
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mem.Close(context.Background())

	ctx := memory.WithStore(context.Background(), mem)
	got, ok := memory.From(ctx)
	if !ok {
		t.Fatal("From: store not in ctx")
	}
	if got != mem {
		t.Error("From returned a different store instance")
	}
	must := memory.MustFrom(ctx)
	if must != mem {
		t.Error("MustFrom returned a different store instance")
	}
}

func TestMustFrom_PanicsWhenAbsent(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustFrom did not panic on empty ctx")
		}
	}()
	_ = memory.MustFrom(context.Background())
}

func TestSnapshot_IsEmpty(t *testing.T) {
	cases := map[string]struct {
		snap memory.Snapshot
		want bool
	}{
		"zero value":           {memory.Snapshot{}, true},
		"strategy only":        {memory.Snapshot{Strategy: memory.StrategyNone}, false},
		"bytes only":           {memory.Snapshot{Bytes: []byte("x")}, false},
		"strategy and bytes":   {memory.Snapshot{Strategy: memory.StrategyTruncation, Bytes: []byte("x")}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.snap.IsEmpty(); got != tc.want {
				t.Errorf("IsEmpty=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpenDriver_RoutesByName(t *testing.T) {
	deps := newTestDeps(t)
	mem, err := memory.OpenDriver("inmem", memory.ConfigSnapshot{
		Strategy: memory.StrategyNone,
	}, deps)
	if err != nil {
		t.Fatalf("OpenDriver inmem: %v", err)
	}
	defer mem.Close(context.Background())
}

func TestOpenDriver_RejectsMissingDeps(t *testing.T) {
	_, err := memory.OpenDriver("inmem", memory.ConfigSnapshot{
		Strategy: memory.StrategyNone,
	}, memory.Deps{})
	if err == nil {
		t.Fatal("err=nil, want non-nil")
	}
}

func TestOpenDriver_UnknownDriverWraps(t *testing.T) {
	deps := newTestDeps(t)
	_, err := memory.OpenDriver("no-such-driver", memory.ConfigSnapshot{
		Strategy: memory.StrategyNone,
	}, deps)
	if !errors.Is(err, memory.ErrUnknownDriver) {
		t.Fatalf("err=%v, want errors.Is ErrUnknownDriver", err)
	}
}

// TestEmitIdentityRejected_PublishesEvent exercises the cross-driver
// helper directly so its coverage is credited to internal/memory (the
// inmem driver's tests cover the helper transitively but per-package
// coverage doesn't see it).
func TestEmitIdentityRejected_PublishesEvent(t *testing.T) {
	bus, _ := newTestBus(t)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{memory.EventTypeMemoryIdentityRejected},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Empty identity — every component missing.
	err = memory.EmitIdentityRejected(context.Background(), bus,
		identity.Quadruple{}, "TestOp")
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("err=%v, want errors.Is ErrIdentityRequired", err)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryIdentityRejected {
			t.Errorf("event type=%q, want %q", ev.Type, memory.EventTypeMemoryIdentityRejected)
		}
		payload, ok := ev.Payload.(memory.MemoryIdentityRejectedPayload)
		if !ok {
			t.Fatalf("payload type=%T, want MemoryIdentityRejectedPayload", ev.Payload)
		}
		if payload.Operation != "TestOp" {
			t.Errorf("payload.Operation=%q, want %q", payload.Operation, "TestOp")
		}
		if !strings.Contains(payload.Reason, "tenant_id") {
			t.Errorf("payload.Reason=%q does not name missing component", payload.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for rejection event")
	}
}

// TestEmitIdentityRejected_ReasonStrings pins the rejection-reason
// string shape across the supported missing-component combinations.
// The strings flow into the bus payload's `Reason` field where audit
// pipelines may key off them.
func TestEmitIdentityRejected_ReasonStrings(t *testing.T) {
	bus, _ := newTestBus(t)
	cases := map[string]struct {
		q          identity.Quadruple
		wantSubstr string
	}{
		"missing tenant only": {
			identity.Quadruple{Identity: identity.Identity{UserID: "U", SessionID: "S"}},
			"tenant_id empty",
		},
		"missing user only": {
			identity.Quadruple{Identity: identity.Identity{TenantID: "T", SessionID: "S"}},
			"user_id empty",
		},
		"missing session only": {
			identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U"}},
			"session_id empty",
		},
		"missing tenant and user": {
			identity.Quadruple{Identity: identity.Identity{SessionID: "S"}},
			"tenant_id and user_id empty",
		},
		"missing all three": {
			identity.Quadruple{},
			"tenant_id, user_id and session_id empty",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sub, err := bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{memory.EventTypeMemoryIdentityRejected},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			emitErr := memory.EmitIdentityRejected(context.Background(), bus, tc.q, "AnyOp")
			if !errors.Is(emitErr, memory.ErrIdentityRequired) {
				t.Fatalf("err=%v, want ErrIdentityRequired", emitErr)
			}
			if !strings.Contains(emitErr.Error(), tc.wantSubstr) {
				t.Errorf("err=%q does not mention %q", emitErr.Error(), tc.wantSubstr)
			}

			select {
			case ev := <-sub.Events():
				payload := ev.Payload.(memory.MemoryIdentityRejectedPayload)
				if !strings.Contains(payload.Reason, tc.wantSubstr) {
					t.Errorf("payload.Reason=%q does not mention %q", payload.Reason, tc.wantSubstr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for rejection event")
			}
		})
	}
}

// --- helpers ---

func newTestDeps(t *testing.T) memory.Deps {
	t.Helper()
	bus, _ := newTestBus(t)
	st, _ := newTestState(t)
	return memory.Deps{State: st, Bus: bus}
}

func newTestBus(t *testing.T) (events.EventBus, func()) {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60_000_000_000,
		DropWindow:               1_000_000_000,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	cleanup := func() { _ = bus.Close(context.Background()) }
	t.Cleanup(cleanup)
	return bus, cleanup
}

func newTestState(t *testing.T) (state.StateStore, func()) {
	t.Helper()
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cleanup := func() { _ = st.Close(context.Background()) }
	t.Cleanup(cleanup)
	return st, cleanup
}

func validQuadruple() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
}
