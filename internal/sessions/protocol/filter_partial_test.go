package protocol

import (
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// A PARTIAL row's counters are unmeasured, not measured-as-zero. A filter
// that treats them as measured turns "we could not look" into "there were
// none" and DROPS the row from the page — the false-absence class arriving
// through the counter rather than the row.
//
// The tests below fail without the `&& !row.CountersPartial` guard on each
// branch: the row is excluded and the caller never learns it existed.

// TestFilterMatches_HasFailedTask_PartialRowIsNotDropped — a fleet admin
// asking for sessions with a failed task must still see a session whose
// task-registry read could not be taken. Its HasFailedTask is false
// because nobody measured it.
func TestFilterMatches_HasFailedTask_PartialRowIsNotDropped(t *testing.T) {
	t.Parallel()
	want := true
	f := prototypes.SessionFilter{HasFailedTask: &want}

	partial := prototypes.SessionRow{HasFailedTask: false, CountersPartial: true}
	if !filterMatches(f, partial) {
		t.Error("a partial row was dropped by has_failed_task=true; an unmeasured false must never be filtered as a measured false")
	}

	// The guard must not neuter the filter on rows that WERE measured.
	measured := prototypes.SessionRow{HasFailedTask: false, CountersPartial: false}
	if filterMatches(f, measured) {
		t.Error("a fully-measured row with no failed task matched has_failed_task=true; the filter must still narrow")
	}
	matching := prototypes.SessionRow{HasFailedTask: true, CountersPartial: false}
	if !filterMatches(f, matching) {
		t.Error("a fully-measured row WITH a failed task was dropped by has_failed_task=true")
	}
}

// TestFilterMatches_HasIntervention_PartialRowIsNotDropped — the same
// contract on the intervention queue. A session with an OPEN human-approval
// gate must not vanish because its pause read failed.
func TestFilterMatches_HasIntervention_PartialRowIsNotDropped(t *testing.T) {
	t.Parallel()
	want := true
	f := prototypes.SessionFilter{HasIntervention: &want}

	partial := prototypes.SessionRow{HasPendingIntervention: false, CountersPartial: true}
	if !filterMatches(f, partial) {
		t.Error("a partial row was dropped by has_intervention=true; a session with an open gate must not vanish from the intervention queue because the pause read failed")
	}

	measured := prototypes.SessionRow{HasPendingIntervention: false, CountersPartial: false}
	if filterMatches(f, measured) {
		t.Error("a fully-measured row with no pending intervention matched has_intervention=true; the filter must still narrow")
	}
	matching := prototypes.SessionRow{HasPendingIntervention: true, CountersPartial: false}
	if !filterMatches(f, matching) {
		t.Error("a fully-measured row WITH a pending intervention was dropped by has_intervention=true")
	}
}

// TestFilterMatches_PartialGuardsAreUniform — every counter-derived
// predicate holds the same rule, so a reader does not have to remember
// which one is guarded. This is the pin that catches the NEXT counter
// filter landing without its guard.
func TestFilterMatches_PartialGuardsAreUniform(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	above := int64(1_000_000)

	for name, f := range map[string]prototypes.SessionFilter{
		"has_failed_task=true":   {HasFailedTask: &yes},
		"has_failed_task=false":  {HasFailedTask: &no},
		"has_intervention=true":  {HasIntervention: &yes},
		"has_intervention=false": {HasIntervention: &no},
		"cost_above_cents":       {CostAboveCents: &above},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A row whose counters are ALL unmeasured is never excluded by a
			// counter-derived predicate, whichever way the predicate points.
			partial := prototypes.SessionRow{CountersPartial: true}
			if !filterMatches(f, partial) {
				t.Errorf("%s dropped a fully-partial row; a counter filter never silently excludes an unmeasured row", name)
			}
		})
	}
}
