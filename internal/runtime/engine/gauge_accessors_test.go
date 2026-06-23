package engine

// White-box tests for the observability size accessors (the runtime
// gauges read these). They are pure, lock-safe reads — no per-run state
// is stored on the engine, so the counts reflect only live internal map
// sizes.

import "testing"

// TestCapacityEntryCount_TracksLiveTrackers asserts CapacityEntryCount
// reflects the live per-run streaming-capacity map: zero on a fresh
// engine, then the count of distinct runs that have acquired a tracker.
func TestCapacityEntryCount_TracksLiveTrackers(t *testing.T) {
	e := reapTestEngine(t)

	if n := e.CapacityEntryCount(); n != 0 {
		t.Fatalf("CapacityEntryCount on a fresh engine = %d, want 0", n)
	}
	e.acquireCapacity("runA", 4)
	e.acquireCapacity("runB", 4)
	if n := e.CapacityEntryCount(); n != 2 {
		t.Errorf("CapacityEntryCount after 2 acquisitions = %d, want 2", n)
	}
	// Re-acquiring an existing run is idempotent — no new entry.
	e.acquireCapacity("runA", 4)
	if n := e.CapacityEntryCount(); n != 2 {
		t.Errorf("CapacityEntryCount after re-acquiring runA = %d, want 2", n)
	}
}

// TestActiveRunCount_TracksDistinctActiveRuns asserts ActiveRunCount
// reflects the number of distinct runs with a worker mid-invocation:
// it counts distinct runs (not invocations) and returns to zero once
// every worker has marked done.
func TestActiveRunCount_TracksDistinctActiveRuns(t *testing.T) {
	e := reapTestEngine(t)

	if n := e.ActiveRunCount(); n != 0 {
		t.Fatalf("ActiveRunCount on a fresh engine = %d, want 0", n)
	}
	e.markRunActive("runA")
	e.markRunActive("runA") // a second concurrent worker on the same run
	e.markRunActive("runB")
	if n := e.ActiveRunCount(); n != 2 {
		t.Errorf("ActiveRunCount with 2 distinct active runs = %d, want 2", n)
	}
	e.markRunDone("runA") // runA still has one worker outstanding
	if n := e.ActiveRunCount(); n != 2 {
		t.Errorf("ActiveRunCount after one of runA's two workers done = %d, want 2", n)
	}
	e.markRunDone("runA")
	e.markRunDone("runB")
	if n := e.ActiveRunCount(); n != 0 {
		t.Errorf("ActiveRunCount after all workers done = %d, want 0", n)
	}
}
