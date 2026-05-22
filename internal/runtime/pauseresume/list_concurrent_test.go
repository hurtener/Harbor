package pauseresume_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// TestList_ConcurrentReuse_D025 pins the D-025 concurrent-reuse
// contract (CLAUDE.md §5 + §11) for Coordinator.List: N≥100 goroutines
// call List against ONE shared Coordinator, each under its own identity
// quadruple and its own filter. The test asserts:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each goroutine filters to its own run and must
//     see ONLY its own pause record back; a cross-talk would surface as
//     a foreign tenant/run on the returned snapshot;
//   - no cross-cancellation — a subset of goroutines run List with a
//     pre-cancelled ctx; their call must fail while every other
//     goroutine's List succeeds untouched;
//   - no goroutine leak — runtime.NumGoroutine returns to baseline
//     after every goroutine has joined.
func TestList_ConcurrentReuse_D025(t *testing.T) {
	const N = 128 // ≥100 per the D-025 contract

	// One shared, immutable-after-construction Coordinator. Pre-seed it
	// with one pause per goroutine identity so each List has a row to
	// find under its own scope.
	c := pauseresume.New()

	ids := make([]identity.Identity, N)
	runIDs := make([]string, N)
	for i := range N {
		ids[i] = identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%03d", i),
			UserID:    fmt.Sprintf("user-%03d", i),
			SessionID: fmt.Sprintf("session-%03d", i),
		}
		runIDs[i] = fmt.Sprintf("run-%03d", i)
		requestPause(t, c, ids[i], runIDs[i], pauseresume.ReasonApprovalRequired)
	}

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)

	for i := range N {
		go func(i int) {
			defer wg.Done()

			id := ids[i]
			runID := runIDs[i]

			// Every fourth goroutine runs with a pre-cancelled ctx —
			// its List must fail; the cancellation must NOT bleed into
			// any sibling.
			cancelled := i%4 == 0
			ctx := context.Background()
			if cancelled {
				cctx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = cctx
			}

			req := pauseresume.ListRequest{
				Identity: id,
				Filter:   pauseresume.ListFilter{RunIDs: []string{runID}},
			}
			resp, err := c.List(ctx, req)
			if cancelled {
				if err == nil {
					errCh <- fmt.Errorf("g%d: List with cancelled ctx returned nil error", i)
				}
				return
			}
			if err != nil {
				errCh <- fmt.Errorf("g%d: List: %w", i, err)
				return
			}
			// Context-bleed check: this goroutine filtered to its OWN
			// run; it must see exactly one row, carrying its own
			// identity. A foreign tenant/run here is a cross-talk bug.
			if resp.TotalRows != 1 || len(resp.Snapshots) != 1 {
				errCh <- fmt.Errorf("g%d: TotalRows=%d len(Snapshots)=%d, want 1/1",
					i, resp.TotalRows, len(resp.Snapshots))
				return
			}
			got := resp.Snapshots[0].Identity
			if got.TenantID != id.TenantID || got.UserID != id.UserID || got.SessionID != id.SessionID {
				errCh <- fmt.Errorf("g%d: context bleed — snapshot identity %+v, want %+v", i, got, id)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak check: every List goroutine has joined; the count
	// must return to baseline (allow a small slack for the runtime's
	// own scheduler bookkeeping).
	if after := runtime.NumGoroutine(); after > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
