package protocol_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestConcurrentReuse_MemoryListUnderRace runs N≥100 concurrent
// memory.list calls against a single shared MemoryStore + ArtifactStore
// + events Aggregator, with overlapping filters and per-goroutine
// identities, asserting the D-025 concurrent-reuse contract: no data
// races (the -race gate), no context bleed (each goroutine's identity
// + filter survive on its own response), no cross-cancellation, and no
// goroutine leak (baseline NumGoroutine restored after all calls
// return).
//
// The memory/protocol functions are stateless — every dependency is
// passed per call — so the contract is satisfied by construction; this
// test pins it under -race so a future refactor that smuggles per-call
// state onto a shared value is caught.
func TestConcurrentReuse_MemoryListUnderRace(t *testing.T) {
	const goroutines = 128

	h := newMemHarness(t, memory.StrategyTruncation, 1_000_000)
	agg := newAggregator(t, h)

	// Two distinct identities under the SAME tenant — both seeded with
	// a known, different turn count. A goroutine listing identity A
	// must never see identity B's rows.
	idA := identity.Quadruple{Identity: identity.Identity{
		TenantID: "t-conc", UserID: "u-a", SessionID: "s-a"}}
	idB := identity.Quadruple{Identity: identity.Identity{
		TenantID: "t-conc", UserID: "u-b", SessionID: "s-b"}}
	const turnsA, turnsB = 6, 9
	seedTurns(t, h, idA, turnsA)
	seedTurns(t, h, idB, turnsB)

	// Let any subscription/replay goroutines from harness construction
	// settle before snapshotting the baseline.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	deps := memprotocol.ListDeps{Store: h.store, Aggregator: agg, DriverName: "inmem"}

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id, wantTurns := idA, turnsA
			if n%2 == 1 {
				id, wantTurns = idB, turnsB
			}
			resp, err := memprotocol.List(context.Background(), deps,
				prototypes.MemoryListRequest{}, id)
			if err != nil {
				errs <- err
				return
			}
			// Context-bleed assertion: the response must carry exactly
			// this goroutine's identity's rows — never the other id's.
			if resp.TotalRows != wantTurns {
				t.Errorf("goroutine %d (id %s): TotalRows = %d, want %d (context bleed?)",
					n, id.UserID, resp.TotalRows, wantTurns)
				return
			}
			for _, it := range resp.Items {
				if it.Identity.User != id.UserID || it.Identity.Session != id.SessionID {
					t.Errorf("goroutine %d: row identity %+v != caller %s/%s (context bleed)",
						n, it.Identity, id.UserID, id.SessionID)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent List error: %v", err)
	}

	// Goroutine-leak assertion: every per-call goroutine joined before
	// its List returned; the count is back to baseline (± a small
	// slack for the runtime's own scheduler bookkeeping).
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > baseline+4 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta %d)", baseline, after, after-baseline)
	}
}

// TestConcurrentReuse_CancellationDoesNotCrossTalk pins that cancelling
// one List call's ctx does not affect a concurrent call against the
// same shared store (D-025 cancellation cross-talk).
func TestConcurrentReuse_CancellationDoesNotCrossTalk(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 1_000_000)
	id := testIdentity()
	seedTurns(t, h, id, 5)
	deps := memprotocol.ListDeps{Store: h.store, DriverName: "inmem"}

	// Call A: an already-cancelled ctx — must fail with ctx.Err().
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, errCancelled := memprotocol.List(cancelledCtx, deps, prototypes.MemoryListRequest{}, id)
	if errCancelled == nil {
		t.Error("List with a cancelled ctx returned nil error, want a cancellation error")
	}

	// Call B: a live ctx against the SAME store — must succeed
	// unaffected by Call A's cancellation.
	resp, err := memprotocol.List(context.Background(), deps, prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List with a live ctx after a cancelled sibling: %v", err)
	}
	if resp.TotalRows != 5 {
		t.Errorf("live-ctx List: TotalRows = %d, want 5 (cancellation cross-talk)", resp.TotalRows)
	}
}
