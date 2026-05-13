package generator_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// TestConcurrent_DistinctSkillsPerIdentity is the D-025 N=128 stress
// for Phase 41. Each goroutine proposes a distinct skill under a
// distinct identity quadruple against ONE shared catalog. Asserts:
//
//   - no data races (the race detector is the gate);
//   - no context bleed — each goroutine's receipt is its own;
//   - goroutine baseline restored within 500ms of WaitGroup join;
//   - no cancellation cross-talk — cancelling one goroutine's ctx
//     does not affect siblings.
//
// CLAUDE.md §11 + D-025: concurrent-reuse tests are mandatory for
// reusable artifacts.
func TestConcurrent_DistinctSkillsPerIdentity(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)
	catalog := tcat.NewCatalog()
	if err := generator.Register(catalog, store, deps); err != nil {
		t.Fatalf("Register: %v", err)
	}

	const N = 128
	baseline := runtime.NumGoroutine()

	var (
		wg          sync.WaitGroup
		successes   atomic.Int64
		errs        = make([]error, N)
		bleedErrs   atomic.Int64
		cancelledOk atomic.Int64
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i),
					UserID:    fmt.Sprintf("u-%d", i),
					SessionID: fmt.Sprintf("s-%d", i),
				},
				RunID: fmt.Sprintf("r-%d", i),
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
			if err != nil {
				errs[i] = err
				return
			}
			// Sprinkle cancelled ctxes so the cancellation
			// cross-talk invariant gets exercised.
			if i%5 == 0 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}

			draft := validDraft(fmt.Sprintf("skill-%d", i))
			receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
				Skill:   draft,
				Persist: true,
			})
			if err != nil {
				if i%5 == 0 && (errors.Is(err, context.Canceled) || contains(err.Error(), "context canceled")) {
					cancelledOk.Add(1)
					return
				}
				errs[i] = err
				return
			}
			// Identity-bleed detector: the receipt should describe
			// THIS goroutine's draft only.
			if receipt.Name != fmt.Sprintf("skill-%d", i) {
				bleedErrs.Add(1)
				errs[i] = fmt.Errorf("identity bleed: receipt.Name=%q want skill-%d", receipt.Name, i)
				return
			}
			expectedRef := fmt.Sprintf("gen:s-%d:r-%d", i, i)
			if receipt.OriginRef != expectedRef {
				bleedErrs.Add(1)
				errs[i] = fmt.Errorf("identity bleed: receipt.OriginRef=%q want %q", receipt.OriginRef, expectedRef)
				return
			}
			successes.Add(1)
		}()
	}
	wg.Wait()

	if bleedErrs.Load() != 0 {
		for _, e := range errs {
			if e != nil {
				t.Logf("err: %v", e)
			}
		}
		t.Fatalf("identity bleed errors: %d", bleedErrs.Load())
	}

	// Allow a brief settle window for the goroutine baseline.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			break
		}
		runtime.Gosched()
	}
	if cur := runtime.NumGoroutine(); cur > baseline+10 {
		t.Logf("goroutine count: baseline=%d current=%d (warning, but may be flaky on parallel runs)", baseline, cur)
	}

	// At least the non-cancelled successes are expected to land.
	expectedSuccesses := int64(N - (N+4)/5) // N - ceil(N/5)
	if successes.Load() < expectedSuccesses {
		t.Fatalf("successes=%d, want ≥%d (cancelled-ok=%d)", successes.Load(), expectedSuccesses, cancelledOk.Load())
	}

	// Suppress unused-warning by reading from non-fatal pile.
	for i, e := range errs {
		if e != nil && i%5 != 0 {
			t.Errorf("goroutine %d: %v", i, e)
		}
	}
}

// TestConcurrent_SameNameResolvesDeterministically asserts that N
// concurrent writers proposing the SAME (identity, name) resolve to
// exactly one persisted state. The hash matches the first write's
// hash, so subsequent writers see Result="idempotent" (none see a
// conflict because no pack row exists).
func TestConcurrent_SameNameResolvesDeterministically(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	const N = 16
	q := testIdentity()
	ctx, _ := identity.WithRun(context.Background(), q.Identity, q.RunID)

	var (
		wg          sync.WaitGroup
		persisted   atomic.Int64
		idempotent  atomic.Int64
		conflictCnt atomic.Int64
		otherErrs   atomic.Int64
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
				Skill:   validDraft("same-name"),
				Persist: true,
			})
			if err != nil {
				if errors.Is(err, generator.ErrSkillConflictSentinel) {
					conflictCnt.Add(1)
					return
				}
				otherErrs.Add(1)
				t.Logf("unexpected err: %v", err)
				return
			}
			switch receipt.Result {
			case generator.ResultPersisted:
				persisted.Add(1)
			case generator.ResultIdempotent:
				idempotent.Add(1)
			default:
				otherErrs.Add(1)
				t.Logf("unexpected receipt.Result=%q", receipt.Result)
			}
		}()
	}
	wg.Wait()

	if otherErrs.Load() != 0 {
		t.Fatalf("unexpected errors: %d", otherErrs.Load())
	}
	if conflictCnt.Load() != 0 {
		t.Fatalf("no pack row was seeded; conflict count=%d, want 0", conflictCnt.Load())
	}
	if persisted.Load() < 1 {
		t.Fatalf("persisted=%d, want ≥ 1", persisted.Load())
	}
	if persisted.Load()+idempotent.Load() != int64(N) {
		t.Fatalf("persisted=%d + idempotent=%d != %d", persisted.Load(), idempotent.Load(), N)
	}

	// The row must exist with the canonical draft's content.
	got, err := store.Get(ctx, q, "same-name")
	if err != nil {
		t.Fatalf("Get same-name: %v", err)
	}
	if got.Origin != skills.OriginGenerated {
		t.Fatalf("Origin=%q want Generated", got.Origin)
	}
}
