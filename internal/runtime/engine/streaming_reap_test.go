package engine

// Capacity-reap tests for the idle-TTL sweeper. The per-run streaming
// trackers (capacities) and overrides used to grow one entry per run
// forever; reapIdleCapacities prunes a tracker once it has been idle
// past the TTL AND has no pending frames, so a live-but-quiet run keeps
// its seq bookkeeping. These are white-box: they drive reapIdleCapacities
// directly with a chosen `now` (mirroring sweepExpiredCancellations).

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func reapTestEngine(t *testing.T) *engine {
	t.Helper()
	nop := func(_ context.Context, in messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
		return in, nil
	}
	in := Node{Name: "in", Func: nop}
	out := Node{Name: "out", Func: nop}
	eng, err := New([]Adjacency{{From: in, To: []Node{out}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e, ok := eng.(*engine)
	if !ok {
		t.Fatalf("New returned %T, want *engine", eng)
	}
	return e
}

func (e *engine) hasCapacity(runID string) bool {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	_, ok := e.capacities[runID]
	return ok
}

// TestReapIdleCapacities_ReapsIdleNotActive: an idle, fully-drained
// tracker is reaped; an aged tracker with a pending frame is NOT (a
// mid-run reap would lose its seq bookkeeping).
func TestReapIdleCapacities_ReapsIdleNotActive(t *testing.T) {
	e := reapTestEngine(t)

	// Run A: reserve a frame and DON'T release it → pending > 0.
	rcA := e.acquireCapacity("runA", 4)
	if _, err := rcA.reserve(context.Background(), "sA", false); err != nil {
		t.Fatalf("reserve A: %v", err)
	}

	// Run B: reserve then release → pending == 0 (idle).
	rcB := e.acquireCapacity("runB", 4)
	if _, err := rcB.reserve(context.Background(), "sB", true); err != nil {
		t.Fatalf("reserve B: %v", err)
	}
	rcB.release("sB", true)

	// Age BOTH trackers well past the TTL.
	old := time.Now().Add(-2 * DefaultCapacityTTL)
	for _, rc := range []*runCapacity{rcA, rcB} {
		rc.mu.Lock()
		rc.lastActivity = old
		rc.mu.Unlock()
	}

	e.reapIdleCapacities(time.Now())

	if !e.hasCapacity("runA") {
		t.Error("runA (pending>0) was reaped — mid-run reap would lose seq bookkeeping")
	}
	if e.hasCapacity("runB") {
		t.Error("runB (idle, drained, aged) was not reaped")
	}
}

// TestReapIdleCapacities_SteadyState: N runs that each stream-and-drain
// then go idle return the maps to baseline after a reap.
func TestReapIdleCapacities_SteadyState(t *testing.T) {
	e := reapTestEngine(t)
	const N = 50
	for i := range N {
		runID := "run-" + itoa(i)
		rc := e.acquireCapacity(runID, 2)
		if _, err := rc.reserve(context.Background(), runID, true); err != nil {
			t.Fatalf("reserve %s: %v", runID, err)
		}
		rc.release(runID, true)
		rc.mu.Lock()
		rc.lastActivity = time.Now().Add(-2 * DefaultCapacityTTL)
		rc.mu.Unlock()
	}

	e.reapIdleCapacities(time.Now())

	e.capMu.Lock()
	nCap := len(e.capacities)
	nOver := len(e.runCapacityOverrides)
	e.capMu.Unlock()
	if nCap != 0 {
		t.Errorf("capacities len = %d after reaping %d idle runs, want 0", nCap, N)
	}
	if nOver != 0 {
		t.Errorf("runCapacityOverrides len = %d, want 0", nOver)
	}
}

// (itoa is shared from topology_concurrent_test.go in package engine.)

// TestReapIdleCapacities_OverrideLifecycle: an override is consumed at
// tracker creation; an orphan override (run set a cap but never
// streamed) is reaped once older than the TTL, but a fresh one survives.
func TestReapIdleCapacities_OverrideLifecycle(t *testing.T) {
	e := reapTestEngine(t)

	// Consumed-on-create: record an override, then create the tracker.
	e.recordRunCapacityOverride("streamed", 8)
	_ = e.acquireCapacity("streamed", e.runCapacityFor("streamed", "in"))
	e.capMu.Lock()
	_, stillHasOverride := e.runCapacityOverrides["streamed"]
	e.capMu.Unlock()
	if stillHasOverride {
		t.Error("override for a streamed run was not consumed at tracker creation")
	}

	// Orphan (never streamed): a fresh one survives, an aged one is reaped.
	e.recordRunCapacityOverride("fresh-orphan", 8)
	e.recordRunCapacityOverride("aged-orphan", 8)
	e.capMu.Lock()
	ov := e.runCapacityOverrides["aged-orphan"]
	ov.setAt = time.Now().Add(-2 * DefaultCapacityTTL)
	e.runCapacityOverrides["aged-orphan"] = ov
	e.capMu.Unlock()

	e.reapIdleCapacities(time.Now())

	e.capMu.Lock()
	_, hasFresh := e.runCapacityOverrides["fresh-orphan"]
	_, hasAged := e.runCapacityOverrides["aged-orphan"]
	e.capMu.Unlock()
	if !hasFresh {
		t.Error("fresh orphan override was reaped — a run may still be about to stream")
	}
	if hasAged {
		t.Error("aged orphan override (never streamed, past TTL) was not reaped")
	}
}
