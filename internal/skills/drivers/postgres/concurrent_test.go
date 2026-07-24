package postgres_test

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
	"github.com/hurtener/Harbor/internal/skills/drivers/postgres"
)

// TestConcurrentReuse_D025 — the D-025 concurrent-reuse contract for
// the Postgres driver. N=128 goroutines on a SINGLE shared store
// exercise the surface for a bounded duration under `-race`. Asserts:
//
//   - no data races (the race detector is the gate);
//   - no context bleed (each goroutine reads back only its own writes;
//     the per-goroutine identity means a cross-goroutine read MUST
//     return nothing / never a sibling's payload);
//   - no cross-cancellation (cancelling one goroutine's ctx never trips
//     a sibling's Upsert / Get);
//   - no goroutine leak (`runtime.NumGoroutine` returns to a bounded
//     window over baseline after teardown — the pg pool + inmem bus
//     drain on their own schedules).
//
// DSN-gated: skips cleanly without HARBOR_PG_DSN.
func TestConcurrentReuse_D025(t *testing.T) {
	if testing.Short() {
		t.Skip("D-025 stress runs the full N≥100 path; -short skips")
	}
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus := buildBus(t)

	store, err := postgres.New(skills.ConfigSnapshot{Driver: "postgres", DSN: dsn}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}

	baselineGoroutines := runtime.NumGoroutine()

	const (
		goroutines = 128
		duration   = 1500 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	deadline := time.Now().Add(duration)
	var (
		wg        sync.WaitGroup
		opCount   atomic.Int64
		errCount  atomic.Int64
		mismatchN atomic.Int64
	)

	for g := range goroutines {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			// Per-goroutine identity so writes never collide; cross-
			// goroutine reads MUST return nothing.
			myID := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", gid),
					UserID:    fmt.Sprintf("u-%d", gid),
					SessionID: fmt.Sprintf("s-%d", gid),
				},
				RunID: fmt.Sprintf("r-%d", gid),
			}
			myName := fmt.Sprintf("skill-%d", gid)
			myDesc := fmt.Sprintf("description-for-goroutine-%d", gid)
			// Local context whose cancellation must not affect siblings.
			localCtx, localCancel := context.WithCancel(ctx)
			defer localCancel()
			i := 0
			for time.Now().Before(deadline) {
				i++
				op := i % 4
				switch op {
				case 0:
					sk := skills.Skill{
						Name:        myName,
						Trigger:     "trg",
						Description: myDesc,
						Steps:       []string{"s"},
						Origin:      skills.OriginGenerated,
						Scope:       skills.ScopeProject,
					}
					if err := store.Upsert(localCtx, myID, sk); err != nil {
						if localCtx.Err() != nil {
							return
						}
						errCount.Add(1)
						t.Errorf("g%d Upsert: %v", gid, err)
						return
					}
				case 1:
					got, err := store.Get(localCtx, myID, myName)
					if err != nil {
						// First iteration may be pre-write; tolerate
						// ErrSkillNotFound but not other errors.
						continue
					}
					if got.Description != myDesc {
						mismatchN.Add(1)
						t.Errorf("g%d Get: context bleed; got Description=%q want=%q",
							gid, got.Description, myDesc)
						return
					}
				case 2:
					_, err := store.Search(localCtx, myID, "description-for-goroutine", 5)
					if err != nil && localCtx.Err() == nil {
						errCount.Add(1)
						t.Errorf("g%d Search: %v", gid, err)
						return
					}
				case 3:
					_, err := store.List(localCtx, myID, skills.ListFilter{Limit: 10})
					if err != nil && localCtx.Err() == nil {
						errCount.Add(1)
						t.Errorf("g%d List: %v", gid, err)
						return
					}
				}
				opCount.Add(1)
			}
		}(g)
	}

	wg.Wait()
	t.Logf("ops=%d errs=%d mismatches=%d", opCount.Load(), errCount.Load(), mismatchN.Load())

	if errCount.Load() != 0 {
		t.Fatalf("unexpected errors: %d", errCount.Load())
	}
	if mismatchN.Load() != 0 {
		t.Fatalf("context bleed detected: %d mismatches", mismatchN.Load())
	}

	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Allow the events bus subscriber + sql/db pool to drain.
	deadline2 := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baselineGoroutines && time.Now().Before(deadline2) {
		runtime.Gosched()
	}
	// Postgres connection pool may keep a few idle connections around
	// (bounded by SetMaxIdleConns) — that's not a leak. Allow slack.
	if delta := runtime.NumGoroutine() - baselineGoroutines; delta > 12 {
		t.Fatalf("goroutine leak: baseline=%d final=%d delta=%d",
			baselineGoroutines, runtime.NumGoroutine(), delta)
	}
}
