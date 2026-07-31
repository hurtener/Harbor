// coverage_start_test.go — pins that adding a FIELD to StartRequest
// changes no body-identity posture (Phase 219 / D-364).
//
// `caller_memory` is a new field on an existing request type, not a new
// request type, so the D-349 coverage table needs no edit and the gate
// stays green without one. That is asserted here rather than assumed:
// "we did not have to touch the table" is a claim, and a claim nobody
// checks is how a posture drifts silently.
package bodyscope

import "testing"

// TestCoverage_StartRequest_KeepsItsControlTaskRow asserts StartRequest
// is still joined to the task-control surface. A field addition must not
// move it, and a future refactor that re-homed it would change which
// body-identity policy governs every `start` on the Runtime.
func TestCoverage_StartRequest_KeepsItsControlTaskRow(t *testing.T) {
	got, ok := SurfaceForRequest("StartRequest")
	if !ok {
		t.Fatal("StartRequest has no body-scope coverage row — every identity-carrying request type needs one")
	}
	if got != SurfaceControlTask {
		t.Fatalf("StartRequest is joined to surface %q, want %q — a field addition must not move the row", got, SurfaceControlTask)
	}
	// ControlRequest shares the row; if the two ever diverge the `start`
	// and steering postures would silently differ.
	sibling, ok := SurfaceForRequest("ControlRequest")
	if !ok || sibling != got {
		t.Fatalf("ControlRequest is joined to %q (present=%v), want the same surface as StartRequest (%q)", sibling, ok, got)
	}
}
