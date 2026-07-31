package conformance

import (
	"os"
	"strings"
	"testing"
)

// TestConformance_FaultRowsAreRegistered closes the one gap this suite's own
// shape leaves open: a row can be DEREGISTERED without breaking the build.
//
// Deleting a `t.Run(...)` line leaves its `func test…` declared and its
// FaultFactory parameter merely unused, and an unused function parameter is
// legal Go — so the suite compiles, every driver's conformance test still
// reports PASS, and the invariant simply stops being asserted. That is the
// pass-value-equals-can't-tell-value shape, and it is the reason the phase
// smoke counts `t.Run` REGISTRATIONS rather than declarations. This test is
// the same guard expressed in Go, so it travels with the package rather than
// with one phase's script, and it fires under `go test` rather than only under
// preflight.
//
// It asserts registration, not behaviour: the rows themselves assert the
// behaviour, and they cannot do so if they never run.
func TestConformance_FaultRowsAreRegistered(t *testing.T) {
	src, err := os.ReadFile("conformance.go")
	if err != nil {
		t.Fatalf("read conformance.go: %v", err)
	}
	body := string(src)

	// Both fault arms are consumed. The two model OPPOSITE disk states behind
	// one identical error value — a write that did not land, and a write that
	// landed and was reported as failed — and a cleanup verified against only
	// one of them is either useless or destructive against the other.
	for _, want := range []struct {
		registration string
		callee       string
		why          string
	}{
		{
			registration: `t.Run("WriteAtomicity_`,
			callee:       "testWriteAtomicity(t, mkFaulty, scope)",
			why:          "a write that did not complete must leave no revision behind",
		},
		{
			registration: `t.Run("CompensationSafety_`,
			callee:       "testCompensationSafety(t, mkCommitted, scope)",
			why:          "a write REPORTED as failed may have landed, and cleaning it up then dangles the active pointer and makes the agent unrecoverable",
		},
	} {
		if n := strings.Count(body, want.registration); n != 1 {
			t.Errorf("conformance.go registers %s...) %d time(s), want exactly 1 — the row asserting that %s is not running", want.registration, n, want.why)
		}
		if !strings.Contains(body, want.callee) {
			t.Errorf("conformance.go does not call %s — the registered row is not driven by its fault factory (%s)", want.callee, want.why)
		}
	}

	// The fault factories are MANDATORY parameters rather than optional
	// capabilities, so a second driver cannot compile without arming both.
	for _, param := range []string{"mkFaulty FaultFactory", "mkCommitted CommittedFaultFactory"} {
		if !strings.Contains(body, param) {
			t.Errorf("conformance.Run no longer takes %q — a driver could then ship the interface without arming the fault", param)
		}
	}
}
