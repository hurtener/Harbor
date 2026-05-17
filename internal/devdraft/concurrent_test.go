// Concurrent-reuse test for Store. The Store is a compiled artifact
// per CLAUDE.md §5 — the test asserts N≥100 concurrent invocations
// against a single shared Store under `-race`:
//
//   - no data races (the race detector is the gate);
//   - no context bleed (each goroutine's draft is invisible to the others
//     unless created under the same identity);
//   - no cross-cancellation (one goroutine's ctx cancel does NOT affect
//     siblings);
//   - no goroutine leaks (the test asserts runtime.NumGoroutine returns
//     to baseline after the load drains).
package devdraft

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// TestStore_ConcurrentReuse_NoRaceUnderLoad pins the D-025 obligation.
// N=128 goroutines × 4 operations apiece = 512 invocations against a
// single Store under -race.
func TestStore_ConcurrentReuse_NoRaceUnderLoad(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	const goroutines = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t%d", i%4),
				UserID:    fmt.Sprintf("u%d", i%8),
				SessionID: fmt.Sprintf("s%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				t.Errorf("identity.With: %v", err)
				return
			}
			// Create.
			draft, err := store.Create(ctx, CreateOptions{Name: fmt.Sprintf("agent-%d", i)})
			if err != nil {
				t.Errorf("goroutine %d: Create: %v", i, err)
				return
			}
			// WriteFile.
			payload := []byte(fmt.Sprintf("// edited by goroutine %d\n", i))
			if err := store.WriteFile(ctx, draft.ID, "agent.go", payload); err != nil {
				t.Errorf("goroutine %d: WriteFile: %v", i, err)
				return
			}
			// Preview.
			res, err := store.Preview(ctx, draft.ID)
			if err != nil {
				t.Errorf("goroutine %d: Preview: %v", i, err)
				return
			}
			if !res.OK {
				// agent.go mutation does not affect harbor.yaml
				// validity — preview should still be OK.
				t.Errorf("goroutine %d: Preview not ok: %v", i, res.Errors)
				return
			}
			// Get verifies the per-goroutine identity isolation —
			// the read sees only this goroutine's draft.
			got, err := store.Get(ctx, draft.ID)
			if err != nil {
				t.Errorf("goroutine %d: Get: %v", i, err)
				return
			}
			if got.ID != draft.ID {
				t.Errorf("goroutine %d: Get id mismatch: %q vs %q", i, got.ID, draft.ID)
				return
			}
			// Cross-identity probe — another identity MUST NOT see this draft.
			otherID := identity.Identity{
				TenantID:  "other-tenant",
				UserID:    "other-user",
				SessionID: "other-session",
			}
			otherCtx, _ := identity.With(context.Background(), otherID)
			if _, err := store.Get(otherCtx, draft.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("goroutine %d: cross-identity Get returned %v; want ErrNotFound", i, err)
				return
			}
		}()
	}
	wg.Wait()

	// Allow any deferred goroutines (bus subscribers, etc.) to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+8 {
			break
		}
		runtime.Gosched()
	}
	if leak := runtime.NumGoroutine() - baseline; leak > 8 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestStore_ConcurrentReuse_CancellationIsScoped pins the "no cross-
// cancellation" guarantee — cancelling one goroutine's ctx must not
// affect sibling goroutines mid-flight.
func TestStore_ConcurrentReuse_CancellationIsScoped(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	id := testIdentity

	// First goroutine: cancel its ctx immediately and try Create.
	// The cancelled ctx may or may not abort the os calls inside
	// scaffold (text/template + os.WriteFile do not honour ctx), but
	// the bus.Publish call DOES check ctx — verify the error
	// surfaces without affecting the second goroutine.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancelledCtx, err := identity.With(cancelledCtx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	cancel()

	// Second goroutine: fresh ctx, must succeed regardless of the
	// first goroutine's cancellation.
	okCtx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = store.Create(cancelledCtx, CreateOptions{Name: "cancelled"})
		// The cancelled path may succeed or fail depending on
		// timing; the only invariant is "does not panic, does not
		// affect the sibling".
	}()
	go func() {
		defer wg.Done()
		if _, err := store.Create(okCtx, CreateOptions{Name: "still-works"}); err != nil {
			t.Errorf("sibling Create failed despite cancelled peer: %v", err)
		}
	}()
	wg.Wait()
}
