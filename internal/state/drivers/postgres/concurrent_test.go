package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestPostgres_Concurrent_SingleSchema — supplemental concurrent
// reuse test (D-025) that complements the conformance suite's
// `Concurrent_SaveLoad_NoRace` (N=128 across many identities). This
// test stresses the Postgres-specific contention path: N goroutines
// hammering Save/Load/Delete on a SINGLE shared driver against a
// SINGLE schema. The point is to flush out:
//
//   - Lock-ordering bugs in the UPSERT path.
//   - Connection-pool exhaustion under hot contention.
//   - Goroutine leaks from prepared-statement caching, etc.
//
// The conformance suite's stress test exercises N=128 across a wide
// identity surface; here N=64 against a tighter identity surface so
// the per-row contention is higher.
func TestPostgres_Concurrent_SingleSchema(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)

	s, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	baseline := runtime.NumGoroutine()

	const goroutines = 64
	const opsPerGo = 8

	var wg sync.WaitGroup
	var errs atomic.Int64
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			ident := identity.Quadruple{
				Identity: identity.Identity{
					// Tighter identity surface than the conformance test
					// — i%4 buckets so contention is higher per-slot.
					TenantID:  fmt.Sprintf("t-%d", i%4),
					UserID:    fmt.Sprintf("u-%d", i%4),
					SessionID: fmt.Sprintf("s-%d", i%8),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			for j := range opsPerGo {
				eventID := state.EventID(fmt.Sprintf("ev-%d-%d", i, j))
				rec := state.StateRecord{
					ID:       eventID,
					Identity: ident,
					Kind:     "task.checkpoint",
					Bytes:    []byte(fmt.Sprintf("payload-%d-%d", i, j)),
					Version:  j,
				}
				if err := s.Save(ctx, rec); err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Save: %v", i, j, err)
					return
				}
				if _, err := s.Load(ctx, ident, "task.checkpoint"); err != nil {
					errs.Add(1)
					t.Errorf("goroutine %d op %d Load: %v", i, j, err)
					return
				}
				// LoadByEventID may miss if a later UPSERT has evicted
				// this EventID — both outcomes are valid (matches the
				// conformance suite's tolerance).
				if _, err := s.LoadByEventID(ctx, eventID); err != nil && !errors.Is(err, state.ErrNotFound) {
					errs.Add(1)
					t.Errorf("goroutine %d op %d LoadByEventID: %v", i, j, err)
					return
				}
				if j%4 == 0 {
					if err := s.Delete(ctx, ident, "task.checkpoint"); err != nil {
						errs.Add(1)
						t.Errorf("goroutine %d op %d Delete: %v", i, j, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errs.Load(); n != 0 {
		t.Fatalf("%d concurrent operations errored", n)
	}

	// Goroutine-leak check: stage any background pool tear-down.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		// Postgres connection pool may keep a few idle connections
		// around — that's bounded by SetMaxIdleConns(5) and is not a
		// leak. Allow a small slack window.
		const slack = 12
		if delta > slack {
			t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
		}
	}
}
