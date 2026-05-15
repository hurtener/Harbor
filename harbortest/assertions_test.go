package harbortest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/harbortest"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// recordingT implements harbortest.TestingT and records every
// t.Errorf call. Used to drive the assertion-failure tests below
// without failing the real *testing.T.
type recordingT struct {
	failures []string
}

func (r *recordingT) Helper() {}
func (r *recordingT) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestAssertSequence_Happy — exact want sequence appears in the log.
func TestAssertSequence_Happy(t *testing.T) {
	log := publishToyLog(t,
		events.EventTypeAdminScopeUsed,
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
	)
	rt := &recordingT{}
	if !harbortest.AssertSequence(rt, log, []events.EventType{
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
	}) {
		t.Errorf("AssertSequence: expected match, got failures %v", rt.failures)
	}
}

// TestAssertSequence_OrderedSubsequence_AllowsIntervening — captured
// log has intervening events between the want sequence; assertion
// still passes.
func TestAssertSequence_OrderedSubsequence_AllowsIntervening(t *testing.T) {
	log := publishToyLog(t,
		events.EventTypeRuntimeError,
		events.EventTypeAdminScopeUsed,
		events.EventTypeRuntimeWarning,
		events.EventTypeBusDropped,
	)
	rt := &recordingT{}
	if !harbortest.AssertSequence(rt, log, []events.EventType{
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
	}) {
		t.Errorf("AssertSequence: ordered-subsequence match failed: %v", rt.failures)
	}
}

// TestAssertSequence_Fails_OnMissingType — captured log is missing
// a required want entry; t.Errorf is called and AssertSequence
// returns false.
func TestAssertSequence_Fails_OnMissingType(t *testing.T) {
	log := publishToyLog(t,
		events.EventTypeRuntimeError,
		events.EventTypeBusDropped,
	)
	rt := &recordingT{}
	ok := harbortest.AssertSequence(rt, log, []events.EventType{
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
	})
	if ok {
		t.Error("AssertSequence: expected failure on missing type")
	}
	if len(rt.failures) == 0 {
		t.Error("AssertSequence: expected t.Errorf to be called")
	}
	if len(rt.failures) > 0 && !strings.Contains(rt.failures[0], "missing") {
		t.Errorf("AssertSequence: error message = %q, expected 'missing' diagnostic", rt.failures[0])
	}
}

// TestAssertSequence_Fails_OnOutOfOrder — want is in a different
// order than the captured sequence; assertion fails.
func TestAssertSequence_Fails_OnOutOfOrder(t *testing.T) {
	log := publishToyLog(t,
		events.EventTypeRuntimeWarning,
		events.EventTypeRuntimeError,
	)
	rt := &recordingT{}
	ok := harbortest.AssertSequence(rt, log, []events.EventType{
		events.EventTypeRuntimeError,
		events.EventTypeRuntimeWarning,
	})
	if ok {
		t.Error("AssertSequence: expected failure on out-of-order")
	}
}

// TestAssertSequence_Empty_Want_Matches — vacuously true.
func TestAssertSequence_Empty_Want_Matches(t *testing.T) {
	log := publishToyLog(t)
	rt := &recordingT{}
	if !harbortest.AssertSequence(rt, log, nil) {
		t.Error("AssertSequence(empty want) = false, expected vacuous match")
	}
}

// TestAssertNoLeaks_Happy — single-identity log; assertion passes.
func TestAssertNoLeaks_Happy(t *testing.T) {
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	red := stubRedactor{}
	bus := openInmemBus(t, red)

	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		for i := 0; i < 3; i++ {
			_ = bus.Publish(ctx, events.Event{
				Type:     events.EventTypeRuntimeError,
				Identity: identity.Quadruple{Identity: id, RunID: "run-1"},
				Payload:  &events.RedactedMap{Data: map[string]any{"i": i}},
			})
		}
		return nil, nil
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &id,
		RunID:    "run-1",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rt := &recordingT{}
	if !harbortest.AssertNoLeaks(rt, log) {
		t.Errorf("AssertNoLeaks: expected pass, failures %v", rt.failures)
	}
}

// TestAssertNoLeaks_CatchesCrossSessionLeak — THE load-bearing
// regression test. A deliberately-broken Agent publishes an event
// tagged with identity A but carrying RunID "run-b" — a cross-session
// leak. AssertNoLeaks must catch it.
//
// This test is the acceptance-criterion fixture from RFC §6.13 +
// brief 06 §3: "AssertNoLeaks(log) (cross-tenant/session leakage
// detector)" lives or dies on this regression.
func TestAssertNoLeaks_CatchesCrossSessionLeak(t *testing.T) {
	red := stubRedactor{}
	bus := openInmemBus(t, red)

	idA := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "session-a"}
	idB := identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "session-b"}

	// The deliberately-broken Agent. Two publish calls:
	//   1) An event under idB with RunID "run-b" — this establishes
	//      ownership: run-b belongs to tenant-b/session-b.
	//   2) An event claiming triple A but carrying RunID "run-b" —
	//      this is the leak: an agent running under identity A
	//      should NOT publish events tagged with run-b.
	// AssertNoLeaks must flag (2).
	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		// Establish run-b's ownership under idB.
		_ = bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: idB, RunID: "run-b"},
			Payload:  &events.RedactedMap{Data: map[string]any{"ok": true}},
		})
		// The leak.
		_ = bus.Publish(ctx, events.Event{
			Type:     events.EventTypeRuntimeError,
			Identity: identity.Quadruple{Identity: idA, RunID: "run-b"}, // <-- cross-talk
			Payload:  &events.RedactedMap{Data: map[string]any{"leak": true}},
		})
		return nil, nil
	})

	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &idA,
		RunID:    "run-a",
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rt := &recordingT{}
	ok := harbortest.AssertNoLeaks(rt, log)
	if ok {
		t.Fatalf("AssertNoLeaks: expected to catch cross-session leak, but assertion passed; log=%+v", log.All())
	}
	if len(rt.failures) == 0 {
		t.Fatal("AssertNoLeaks: failed but did not call t.Errorf")
	}
	combined := strings.Join(rt.failures, "\n")
	if !strings.Contains(combined, "cross-talk") {
		t.Errorf("AssertNoLeaks: error message = %q, expected 'cross-talk' diagnostic", combined)
	}
}

// TestAssertNoLeaks_NilLog_ErrorPath — defensive; nil log surfaces
// t.Errorf and returns false.
func TestAssertNoLeaks_NilLog_ErrorPath(t *testing.T) {
	rt := &recordingT{}
	if harbortest.AssertNoLeaks(rt, nil) {
		t.Error("AssertNoLeaks(nil log) = true, expected false")
	}
	if len(rt.failures) == 0 {
		t.Error("AssertNoLeaks(nil log) did not call t.Errorf")
	}
}

// TestAssertSequence_NilLog_ErrorPath — same defensive shape.
func TestAssertSequence_NilLog_ErrorPath(t *testing.T) {
	rt := &recordingT{}
	if harbortest.AssertSequence(rt, nil, []events.EventType{events.EventTypeRuntimeError}) {
		t.Error("AssertSequence(nil log) = true, expected false")
	}
}

// publishToyLog publishes the given event types under a canonical
// identity into a fresh bus, captures the resulting EventLog via a
// RunOnce call whose Agent body publishes during the captured
// window, and returns it. Used by the assertion tests that need a
// hand-crafted log shape.
func publishToyLog(t *testing.T, types ...events.EventType) *harbortest.EventLog {
	t.Helper()
	red := stubRedactor{}
	bus := openInmemBus(t, red)
	id := identity.Identity{TenantID: "harbortest", UserID: "harbortest", SessionID: "harbortest"}

	agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
		bus := events.MustFrom(ctx)
		for _, ty := range types {
			_ = bus.Publish(ctx, events.Event{
				Type:     ty,
				Identity: identity.Quadruple{Identity: id, RunID: "run-1"},
				Payload:  &events.RedactedMap{Data: map[string]any{}},
			})
		}
		return nil, nil
	})
	_, log, err := harbortest.RunOnce(t.Context(), agent, nil, harbortest.Deps{
		Bus:      bus,
		Identity: &id,
		RunID:    "run-1",
	})
	if err != nil {
		t.Fatalf("publishToyLog RunOnce: %v", err)
	}
	return log
}
