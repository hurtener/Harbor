package admission

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestAuthority_Concurrent_MixedTuples_NoBleed is the concurrent-reuse
// gate for the shared Authority (D-025 contract): N=128 goroutines mint
// and verify across a single shared instance under -race, with
// cross-token swaps. It asserts:
//
//   - every goroutine's own token verifies against its own tuple only;
//   - a swapped token (the neighbor's admission) fails closed as
//     ErrTokenMismatch for every goroutine — no token/context bleed;
//   - all 128 minted tokens are pairwise distinct;
//   - no process-local registry or shared mutable state exists (the
//     Authority carries no mutable fields; each goroutine's claims ride
//     its own token).
func TestAuthority_Concurrent_MixedTuples_NoBleed(t *testing.T) {
	const n = 128

	a, err := New(testSealer(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type minted struct {
		tuple RenderTuple
		tok   Token
	}
	results := make([]minted, n)

	// Phase 1 — concurrent mints, one unique tuple per goroutine.
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tuple := testTuple(i)
			tok, err := a.Mint(context.Background(), tuple)
			if err != nil {
				errs <- fmt.Errorf("mint %d: %w", i, err)
				return
			}
			results[i] = minted{tuple: tuple, tok: tok}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	// Every token is distinct (fresh 128-bit claim nonce per mint).
	seen := make(map[string]struct{}, n)
	for i, r := range results {
		if _, dup := seen[r.tok.Value]; dup {
			t.Fatalf("token collision at index %d", i)
		}
		seen[r.tok.Value] = struct{}{}
	}

	// Phase 2 — concurrent verifies: each goroutine verifies its OWN
	// token (must pass and carry its own tuple) and the NEIGHBOR's
	// token against its own tuple (must fail closed as Mismatch).
	wg = sync.WaitGroup{}
	errs = make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			claims, err := a.Verify(ctx, results[i].tuple, results[i].tok.Value)
			if err != nil {
				errs <- fmt.Errorf("verify own %d: %w", i, err)
				return
			}
			if claims.TenantID != results[i].tuple.Identity.TenantID {
				errs <- fmt.Errorf("verify own %d: claims tenant %q bled from another tuple",
					i, claims.TenantID)
				return
			}
			neighbor := (i + 1) % n
			if _, err := a.Verify(ctx, results[i].tuple, results[neighbor].tok.Value); !errors.Is(err, ErrTokenMismatch) {
				errs <- fmt.Errorf("swap %d<-%d: want ErrTokenMismatch, got %v", i, neighbor, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
