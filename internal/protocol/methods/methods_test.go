package methods_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
)

// The canonical method names — the Phase 54 task-control row + the
// Phase 72 streaming-events anchor. This slice is the test's
// independent source of truth; if methods.go drifts, the exhaustiveness
// test below fails.
var wantMethods = []methods.Method{
	methods.MethodStart,
	methods.MethodCancel,
	methods.MethodPause,
	methods.MethodResume,
	methods.MethodRedirect,
	methods.MethodInjectContext,
	methods.MethodApprove,
	methods.MethodReject,
	methods.MethodPrioritize,
	methods.MethodUserMessage,
	methods.MethodEventsSubscribe,
}

func TestMethods_ExhaustivenessAndWireStrings(t *testing.T) {
	got := methods.Methods()
	if len(got) != 11 {
		t.Fatalf("Methods() returned %d methods, want 11 (10 task-control + events.subscribe)", len(got))
	}
	if len(got) != len(wantMethods) {
		t.Fatalf("Methods() count %d != wantMethods count %d", len(got), len(wantMethods))
	}

	// Methods() is documented to return a deterministic sorted snapshot.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Methods() not sorted: %q >= %q at index %d", got[i-1], got[i], i)
		}
	}

	// Every wanted method must be valid and present.
	gotSet := map[methods.Method]struct{}{}
	for _, m := range got {
		gotSet[m] = struct{}{}
	}
	for _, want := range wantMethods {
		if !methods.IsValidMethod(want) {
			t.Errorf("IsValidMethod(%q) = false, want true", want)
		}
		if _, ok := gotSet[want]; !ok {
			t.Errorf("Methods() snapshot missing %q", want)
		}
	}

	// Wire strings are the RFC §5.2 verbatim lowercase snake_case.
	wireStrings := map[methods.Method]string{
		methods.MethodStart:           "start",
		methods.MethodCancel:          "cancel",
		methods.MethodPause:           "pause",
		methods.MethodResume:          "resume",
		methods.MethodRedirect:        "redirect",
		methods.MethodInjectContext:   "inject_context",
		methods.MethodApprove:         "approve",
		methods.MethodReject:          "reject",
		methods.MethodPrioritize:      "prioritize",
		methods.MethodUserMessage:     "user_message",
		methods.MethodEventsSubscribe: "events.subscribe",
	}
	for m, want := range wireStrings {
		if string(m) != want {
			t.Errorf("method wire string = %q, want %q", string(m), want)
		}
	}
}

func TestIsValidMethod_RejectsUnknown(t *testing.T) {
	for _, bad := range []methods.Method{
		"", "START", "Start", "cancel_task", "inject-context",
		"INJECT_CONTEXT", "usermessage", "unknown",
	} {
		if methods.IsValidMethod(bad) {
			t.Errorf("IsValidMethod(%q) = true, want false", bad)
		}
	}
}

func TestIsControlMethod_StartAndEventsSubscribeAreNotControls(t *testing.T) {
	if methods.IsControlMethod(methods.MethodStart) {
		t.Error("IsControlMethod(start) = true, want false — start maps to the task registry, not the steering inbox")
	}
	if methods.IsControlMethod(methods.MethodEventsSubscribe) {
		t.Error("IsControlMethod(events.subscribe) = true, want false — events.subscribe is a streaming-events method, not a steering-control method (Phase 72 / D-105)")
	}
	// Every other canonical method IS a control method.
	for _, m := range methods.Methods() {
		if m == methods.MethodStart || m == methods.MethodEventsSubscribe {
			continue
		}
		if !methods.IsControlMethod(m) {
			t.Errorf("IsControlMethod(%q) = false, want true", m)
		}
	}
	// An unknown method is not a control method.
	if methods.IsControlMethod(methods.Method("bogus")) {
		t.Error("IsControlMethod(bogus) = true, want false")
	}
}

// TestMethods_EventsSubscribe_Registered — pins the Phase 72 anchor:
// MethodEventsSubscribe is registered, IsValidMethod returns true,
// IsControlMethod returns false, the wire string is exactly
// "events.subscribe" (third-party Consoles branch on it).
func TestMethods_EventsSubscribe_Registered(t *testing.T) {
	if string(methods.MethodEventsSubscribe) != "events.subscribe" {
		t.Fatalf("MethodEventsSubscribe wire string = %q, want %q",
			string(methods.MethodEventsSubscribe), "events.subscribe")
	}
	if !methods.IsValidMethod(methods.MethodEventsSubscribe) {
		t.Error("IsValidMethod(events.subscribe) = false, want true")
	}
	if methods.IsControlMethod(methods.MethodEventsSubscribe) {
		t.Error("IsControlMethod(events.subscribe) = true, want false — streaming-events, not steering-control")
	}
	// String-form stability: a third-party Console computes the
	// canonical name as a literal and expects parity.
	if !methods.IsValidMethod(methods.Method("events.subscribe")) {
		t.Error(`IsValidMethod(Method("events.subscribe")) = false, want true — wire string stability broken`)
	}
}
