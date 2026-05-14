package methods_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
)

// The ten canonical task-control method names — the RFC §5.2 "Task
// control" row verbatim. This slice is the test's independent source of
// truth; if methods.go drifts from RFC §5.2, the exhaustiveness test
// below fails.
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
}

func TestMethods_ExhaustivenessAndWireStrings(t *testing.T) {
	got := methods.Methods()
	if len(got) != 10 {
		t.Fatalf("Methods() returned %d methods, want 10", len(got))
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
		methods.MethodStart:         "start",
		methods.MethodCancel:        "cancel",
		methods.MethodPause:         "pause",
		methods.MethodResume:        "resume",
		methods.MethodRedirect:      "redirect",
		methods.MethodInjectContext: "inject_context",
		methods.MethodApprove:       "approve",
		methods.MethodReject:        "reject",
		methods.MethodPrioritize:    "prioritize",
		methods.MethodUserMessage:   "user_message",
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

func TestIsControlMethod_StartIsNotAControl(t *testing.T) {
	if methods.IsControlMethod(methods.MethodStart) {
		t.Error("IsControlMethod(start) = true, want false — start maps to the task registry, not the steering inbox")
	}
	// Every non-start canonical method IS a control method.
	for _, m := range methods.Methods() {
		if m == methods.MethodStart {
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
