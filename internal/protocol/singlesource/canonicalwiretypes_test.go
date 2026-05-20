package singlesource_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// TestCanonicalWireTypes_Phase73bExtension pins the Phase 73b (D-126)
// CanonicalWireTypes extension: the Live Runtime page's header
// status-counter-strip aggregate is a new canonical Protocol wire type
// and MUST be recorded in singlesource.CanonicalWireTypes with the
// `types` home package.
//
// The broad lockstep test (TestSingleSource_CanonicalWireTypesInLockstep)
// already proves the type round-trips against the declared package; this
// test is the per-phase anchor so a future refactor that drops the
// entry fails with a Phase-73b-named message.
func TestCanonicalWireTypes_Phase73bExtension(t *testing.T) {
	home, ok := singlesource.CanonicalWireTypes["TasksListStatusCounterStrip"]
	if !ok {
		t.Fatal("TasksListStatusCounterStrip missing from singlesource.CanonicalWireTypes — Phase 73b (D-126) registers it")
	}
	if home != "types" {
		t.Fatalf("TasksListStatusCounterStrip home = %q, want %q", home, "types")
	}
}

// TestCanonicalWireTypes_NoNewTopologyOrEventsType pins the Phase 73b
// composition-only posture for the events.subscribe RunID filter and
// the topology surface. Phase 73b ships NO new events.* / topology.*
// wire type — the run-scoped filter rides the already-shipped
// EventFilter.RunIDs carrier (D-082) and the topology canvas consumes
// the already-shipped TopologyProjection (Phase 74 / D-114). If a
// future change adds a parallel RunID-filter type, this test fails and
// forces a §13 "no parallel implementations" review.
func TestCanonicalWireTypes_NoNewTopologyOrEventsType(t *testing.T) {
	forbidden := []string{
		"EventsSubscribeFilter",    // would be a parallel EventFilter
		"TopologySnapshotResponse", // topology.snapshot returns TopologyProjection directly
	}
	for _, name := range forbidden {
		if _, ok := singlesource.CanonicalWireTypes[name]; ok {
			t.Errorf("CanonicalWireTypes lists %q — Phase 73b is composition-only on events.* / topology.* (no new type)", name)
		}
	}
}
