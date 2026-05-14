package steering

import "testing"

// TestControlTypes_Exhaustiveness pins the nine-type taxonomy as a
// phase contract (RFC §6.3 — Settled). Adding or removing a type
// without updating this test is a drift signal.
func TestControlTypes_Exhaustiveness(t *testing.T) {
	got := ControlTypes()
	if len(got) != 9 {
		t.Fatalf("ControlTypes() returned %d types, want 9", len(got))
	}
	want := map[ControlType]bool{
		ControlInjectContext: true,
		ControlRedirect:      true,
		ControlCancel:        true,
		ControlPrioritize:    true,
		ControlPause:         true,
		ControlResume:        true,
		ControlApprove:       true,
		ControlReject:        true,
		ControlUserMessage:   true,
	}
	for _, tp := range got {
		if !want[tp] {
			t.Errorf("ControlTypes() returned unexpected type %q", tp)
		}
		delete(want, tp)
	}
	for tp := range want {
		t.Errorf("ControlTypes() missing canonical type %q", tp)
	}
}

// TestControlTypes_SortedDeterministic asserts ControlTypes() is
// lexicographically sorted (the Protocol projection's allow-list
// depends on a stable order).
func TestControlTypes_SortedDeterministic(t *testing.T) {
	got := ControlTypes()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ControlTypes() not sorted: %q >= %q at index %d", got[i-1], got[i], i)
		}
	}
}

func TestIsValidControlType_KnownAndUnknown(t *testing.T) {
	for _, tp := range ControlTypes() {
		if !IsValidControlType(tp) {
			t.Errorf("IsValidControlType(%q) = false, want true", tp)
		}
	}
	for _, bad := range []ControlType{"", "inject_context", "STOP", "PAUSE ", "ApPrOvE"} {
		if IsValidControlType(bad) {
			t.Errorf("IsValidControlType(%q) = true, want false", bad)
		}
	}
}

// TestControlTypes_WireStringsAreVerbatim asserts the wire strings
// are the RFC §6.3 verbatim uppercase identifiers — the Protocol
// projection (Phase 54) accepts exactly these.
func TestControlTypes_WireStringsAreVerbatim(t *testing.T) {
	cases := map[ControlType]string{
		ControlInjectContext: "INJECT_CONTEXT",
		ControlRedirect:      "REDIRECT",
		ControlCancel:        "CANCEL",
		ControlPrioritize:    "PRIORITIZE",
		ControlPause:         "PAUSE",
		ControlResume:        "RESUME",
		ControlApprove:       "APPROVE",
		ControlReject:        "REJECT",
		ControlUserMessage:   "USER_MESSAGE",
	}
	for tp, want := range cases {
		if string(tp) != want {
			t.Errorf("control type wire string = %q, want %q", string(tp), want)
		}
	}
}
