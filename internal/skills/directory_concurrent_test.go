package skills_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
)

// TestDirectory_ConcurrentReuse_D025 — N≥128 goroutines invoking
// View against ONE shared *Directory. Each goroutine carries a
// unique identity (tenant=t-i, user=u-i, session=s-i) and the spy
// store is seeded per-identity with the goroutine's own unique pin
// set + tail. Asserts:
//
//   - No data races (race detector — go test -race).
//   - No identity bleed: every returned row corresponds to a skill
//     seeded under the goroutine's exact identity.
//   - No goroutine leak: runtime.NumGoroutine returns to baseline.
//
// Phase 39 + CLAUDE.md §5 + §11 + D-025: a phase that builds a
// reusable artifact (Directory) ships this test.
func TestDirectory_ConcurrentReuse_D025(t *testing.T) {
	// NOT t.Parallel() — the NumGoroutine assertion races with any
	// sibling test bursting goroutines. Serial keeps the baseline +
	// current measurement honest.
	const N = 128

	bus := directoryTestBus(t)
	// memStore (defined in directory_test.go) is already thread-safe
	// via its mu.Lock — reuse it as the D-025 stress backend.
	store := newMemStore(bus)

	// One Directory shared across N goroutines. The DirectoryConfig
	// is a single shared config; the per-goroutine pin set is in the
	// spy store (via Skill.Extra["pinned"]=true) so the Directory
	// itself stays immutable after construction.
	dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{
		MaxEntries: 50,
		Selection:  skills.SelectionPinnedThenRecent,
	})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	// Seed the spy store: for each goroutine i, three skills under
	// identity (t-i, u-i, s-i). The first is Extra-pinned (so the
	// View MUST include it first); the rest are unpinned. UpdatedAt
	// is staggered so the unpinned remainder has a deterministic
	// order under pinned_then_recent.
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := range N {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", i),
			UserID:    fmt.Sprintf("u-%d", i),
			SessionID: fmt.Sprintf("s-%d", i),
		}
		pinnedSkill := skills.Skill{
			Name:      fmt.Sprintf("g%d-pin", i),
			Title:     "Pinned",
			Trigger:   "pinned trigger",
			Steps:     []string{"s"},
			Origin:    skills.OriginPack,
			Scope:     skills.ScopeProject,
			UpdatedAt: base,
			Extra:     map[string]any{skills.ExtraPinnedKey: true},
		}
		s2 := skills.Skill{
			Name:      fmt.Sprintf("g%d-s2", i),
			Title:     "Tail-2",
			Trigger:   "trigger",
			Steps:     []string{"s"},
			Origin:    skills.OriginPack,
			Scope:     skills.ScopeProject,
			UpdatedAt: base.Add(2 * time.Minute),
		}
		s3 := skills.Skill{
			Name:      fmt.Sprintf("g%d-s3", i),
			Title:     "Tail-3",
			Trigger:   "trigger",
			Steps:     []string{"s"},
			Origin:    skills.OriginPack,
			Scope:     skills.ScopeProject,
			UpdatedAt: base.Add(1 * time.Minute),
		}
		store.seed(id, pinnedSkill, s2, s3)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var (
		okCount atomic.Int64
		bleeds  atomic.Int64
	)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i)
			user := fmt.Sprintf("u-%d", i)
			session := fmt.Sprintf("s-%d", i)
			id := identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("r-%d", i))
			if err != nil {
				t.Errorf("goroutine %d: identity.WithRun: %v", i, err)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			view, err := dir.View(ctx, skills.DirectoryCapability{})
			if err != nil {
				t.Errorf("goroutine %d: View: %v", i, err)
				return
			}

			// Expected ordering: pinned first, then s2 (UpdatedAt+2m),
			// then s3 (UpdatedAt+1m) — pinned_then_recent on the
			// unpinned tail.
			wantNames := []string{
				fmt.Sprintf("g%d-pin", i),
				fmt.Sprintf("g%d-s2", i),
				fmt.Sprintf("g%d-s3", i),
			}
			if len(view) != 3 {
				bleeds.Add(1)
				t.Errorf("goroutine %d: View length=%d, want 3 (rows=%v)", i, len(view), viewNames(view))
				return
			}
			for j, wantName := range wantNames {
				if view[j].Name != wantName {
					bleeds.Add(1)
					t.Errorf("goroutine %d: view[%d].Name=%q, want %q (rows=%v)",
						i, j, view[j].Name, wantName, viewNames(view))
					return
				}
			}
			if !view[0].Pinned {
				bleeds.Add(1)
				t.Errorf("goroutine %d: view[0].Pinned=false, want true", i)
				return
			}
			if view[1].Pinned || view[2].Pinned {
				bleeds.Add(1)
				t.Errorf("goroutine %d: view[1]/view[2] should be Pinned=false", i)
				return
			}
			okCount.Add(1)
		}(i)
	}
	wg.Wait()

	if total := okCount.Load(); total != int64(N) {
		t.Fatalf("successful goroutines=%d, want %d (bleeds=%d)", total, N, bleeds.Load())
	}
	if bleeds.Load() != 0 {
		t.Fatalf("identity bleeds detected: %d", bleeds.Load())
	}

	// Goroutine-leak gate. Allow a small headroom (+5) for test-
	// framework goroutines that may not yet be reaped. Phase 38's
	// equivalent test uses the same headroom.
	runtime.GC()
	current := runtime.NumGoroutine()
	if current > baseline+5 {
		t.Fatalf("goroutine count grew: baseline=%d current=%d", baseline, current)
	}
}

// TestDirectory_ConcurrentCancellationIsolation — a fraction of
// goroutines pre-cancel their ctx BEFORE calling View. Asserts that
// sibling goroutines (with live ctx) complete their View
// successfully — no cross-cancellation through the shared
// Directory.
//
// The in-memory store may return synchronously before any ctx-check
// fires, so a pre-cancelled View MAY return success. The invariant
// we test is the converse — live goroutines MUST complete
// successfully even when siblings are cancelling.
func TestDirectory_ConcurrentCancellationIsolation(t *testing.T) {
	const N = 64

	bus := directoryTestBus(t)
	store := newMemStore(bus)
	for i := range N {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("c-t-%d", i),
			UserID:    fmt.Sprintf("c-u-%d", i),
			SessionID: fmt.Sprintf("c-s-%d", i),
		}
		store.seed(id, skills.Skill{
			Name:    fmt.Sprintf("c-g%d", i),
			Title:   "c-skill",
			Trigger: "trigger",
			Steps:   []string{"s"},
			Origin:  skills.OriginPack,
			Scope:   skills.ScopeProject,
		})
	}

	dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{})
	if err != nil {
		t.Fatalf("NewDirectory: %v", err)
	}

	var liveOK atomic.Int64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("c-t-%d", i),
				UserID:    fmt.Sprintf("c-u-%d", i),
				SessionID: fmt.Sprintf("c-s-%d", i),
			}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("r-%d", i))
			if err != nil {
				t.Errorf("goroutine %d: identity.WithRun: %v", i, err)
				return
			}
			ctx, cancel := context.WithCancel(ctx)
			if i%5 == 0 {
				// Pre-cancel for a fraction. The View MAY succeed
				// (in-memory store completes synchronously) or fail
				// with ctx.Err; both are acceptable. The invariant
				// is "siblings remain healthy."
				cancel()
				_, _ = dir.View(ctx, skills.DirectoryCapability{})
				return
			}
			defer cancel()
			view, err := dir.View(ctx, skills.DirectoryCapability{})
			if err != nil {
				t.Errorf("goroutine %d (live): View: %v", i, err)
				return
			}
			if len(view) != 1 {
				t.Errorf("goroutine %d: View length=%d, want 1", i, len(view))
				return
			}
			expectedName := fmt.Sprintf("c-g%d", i)
			if view[0].Name != expectedName {
				t.Errorf("goroutine %d: view[0].Name=%q, want %q", i, view[0].Name, expectedName)
				return
			}
			liveOK.Add(1)
		}(i)
	}
	wg.Wait()

	// Live goroutines (i % 5 != 0): all must have completed.
	wantLive := int64(0)
	for i := range N {
		if i%5 != 0 {
			wantLive++
		}
	}
	if liveOK.Load() != wantLive {
		t.Fatalf("live goroutines completed=%d, want %d (cancellation cross-talk detected)",
			liveOK.Load(), wantLive)
	}
}
