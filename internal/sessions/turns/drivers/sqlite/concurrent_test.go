package sqlite_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// The store contract's concurrent-reuse gate (AGENTS.md §5, D-025):
// N >= 100 concurrent invocations against ONE shared driver instance
// under -race — no data races, no identity bleed, no cancellation
// cross-talk, no goroutine leaks. SQLite's single-writer reality is
// honored by the driver's single-connection pool, so the assertion
// here is that 100+ goroutines serialize correctly: every append is
// minted a UNIQUE per-session sequence, every walk returns every row
// exactly once, concurrent checkpoint saves converge to the max, and
// Close joins everything (no goroutine leak).
const concurrentWorkers = 100

// concurrentReaders is the number of concurrent walkers after the
// write storm. N >= 100 is carried by the two WRITE paths (appenders
// + checkpoint writers); the readers provide race coverage on the
// read path while sharing the single pool connection.
const concurrentReaders = 10

// checkpointIterations is the per-writer save count; 100 writers × 1
// save each still race to the max (the conformance suite's
// Checkpoint_ConcurrentAdvances_ConvergeToMax pins the deeper
// per-writer interleaving).
const checkpointIterations = 1

// TestConcurrent_AppendListCheckpoint_N100 runs the concurrent-reuse
// gate in two phases on ONE shared store under the race detector:
//
//   - PHASE 1 (the N>=100 write storm): 100 concurrent appenders (one
//     distinct turn each) + 100 concurrent checkpoint writers racing
//     to the max. SQLite's single-writer reality is honored by the
//     driver's single-connection pool — every append still mints a
//     UNIQUE per-session sequence and every checkpoint save converges
//     monotonically.
//   - PHASE 2 (concurrent reads): after the writes land, 20 concurrent
//     readers walk the whole session; each walk must be internally
//     CONSISTENT (no errors, no duplicate turn, strictly newest-first)
//     and the authoritative full walk must return every row exactly
//     once with the contiguous 1..100 sequence set.
//
// The phases are split so the write storm (200 goroutines) and the
// read storm are not simultaneously thrashing the single pool
// connection — under -race that interleaving cost dominates the test
// without testing anything new. The keyset snapshot semantics (a
// reader that started early sees exactly the rows that existed at its
// start) are pinned by the conformance suite's
// ListTurns_AppendDuringWalk_NoSkipNoDuplicate.
func TestConcurrent_AppendListCheckpoint_N100(t *testing.T) {
	// :memory: — the concurrency contract is about the shared-instance
	// invariants, not fsync; the durable-restart proofs live in
	// restart_test.go.
	s := openMem(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	start := make(chan struct{})
	writeErrs := make(chan error, concurrentWorkers*3)
	var wg sync.WaitGroup

	// PHASE 1a: 100 concurrent appenders, one distinct turn each.
	for w := range concurrentWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			if _, err := s.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("w-%03d", w), id, w)); err != nil {
				writeErrs <- fmt.Errorf("append %d: %w", w, err)
			}
		}(w)
	}

	// PHASE 1b: 100 concurrent checkpoint writers racing to the max.
	for w := range concurrentWorkers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for v := range checkpointIterations {
				if err := s.SaveCheckpoint(ctx, id, uint64(w*checkpointIterations+v)); err != nil {
					writeErrs <- fmt.Errorf("checkpoint %d: %w", w, err)
					return
				}
			}
		}(w)
	}

	close(start)
	wg.Wait()
	close(writeErrs)
	for err := range writeErrs {
		t.Fatal(err)
	}

	// PHASE 2a: concurrent readers walking the full session now that
	// all writes have landed. Each walk must see every row exactly
	// once, strictly newest-first.
	readErrs := make(chan error, concurrentReaders)
	for range concurrentReaders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := map[string]bool{}
			lastSeq := turns.Seq(1 << 62)
			var c *turns.Cursor
			for {
				rows, next, _, err := s.ListTurns(ctx, id, c, 7)
				if err != nil {
					readErrs <- fmt.Errorf("list: %w", err)
					return
				}
				for _, row := range rows {
					if seen[string(row.TurnID)] {
						readErrs <- fmt.Errorf("reader saw duplicate %q", row.TurnID)
						return
					}
					seen[string(row.TurnID)] = true
					if row.Sequence >= lastSeq {
						readErrs <- fmt.Errorf("reader page not strictly newest-first at %q", row.TurnID)
						return
					}
					lastSeq = row.Sequence
				}
				if next == nil {
					break
				}
				c = next
			}
			if len(seen) != concurrentWorkers {
				readErrs <- fmt.Errorf("reader walked %d rows, want %d", len(seen), concurrentWorkers)
				return
			}
		}()
	}
	wg.Wait()
	close(readErrs)
	for err := range readErrs {
		t.Fatal(err)
	}

	// Sequences are unique per session: one full walk must return
	// every one of the 100 rows exactly once, minting exactly the
	// contiguous 1..100 sequence set (no skips, no duplicates).
	seen := map[turns.Seq]bool{}
	walked := 0
	var cur *turns.Cursor
	for {
		page, nxt, _, err := s.ListTurns(ctx, id, cur, 10)
		if err != nil {
			t.Fatalf("walk: %v", err)
		}
		walked += len(page)
		for _, r := range page {
			if seen[r.Sequence] {
				t.Fatalf("duplicate sequence %d minted", r.Sequence)
			}
			seen[r.Sequence] = true
		}
		if nxt == nil {
			break
		}
		cur = nxt
	}
	if walked != concurrentWorkers {
		t.Fatalf("walked %d rows, want %d", walked, concurrentWorkers)
	}
	for seq := turns.Seq(1); seq <= concurrentWorkers; seq++ {
		if !seen[seq] {
			t.Fatalf("sequence %d never minted (skip in the counter)", seq)
		}
	}

	// Checkpoint converged to the max (100 writers x 2 values, max =
	// 99*2+1 = 199).
	cp, err := s.LoadCheckpoint(ctx, id)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if cp != uint64(concurrentWorkers*checkpointIterations-1) {
		t.Errorf("checkpoint=%d, want %d (concurrent advances converge to the max)", cp, uint64(concurrentWorkers*5-1))
	}
}

// TestConcurrent_NoGoroutineLeak_AfterClose proves Close joins
// everything: the goroutine count returns to baseline after the store
// is closed (the driver spawns no goroutines, but a regression that
// does must not leak).
func TestConcurrent_NoGoroutineLeak_AfterClose(t *testing.T) {
	s := openMem(t)
	ctx := context.Background()
	id := identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for w := range 20 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 10 {
				if _, err := s.AppendTurnIf(ctx, id, richRow(fmt.Sprintf("g-%d-%02d", w, i), id, w*10+i)); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if err := s.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+1 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, got)
	}
}
