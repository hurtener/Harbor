package sqlite_test

// Driver-local concurrency stress for the SQLite MemoryStore driver.
// The conformance suite's `Concurrent_AllMethods_NoRace` already runs
// N=128 goroutines against a single shared driver instance and is
// the primary D-025 gate; this file adds a supplemental disk-backed
// cohort that hammers the Snapshot/Restore (the only contention-
// generating paths under Strategy=none — Flush also DELETEs, which
// shares a writer lock with persistRecord).

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
)

// TestSQLite_Memory_Concurrent_BusyTimeoutAbsorbsContention runs an
// N=64 cohort exercising the write-heavy paths (Restore + Flush) +
// the read paths (Snapshot + GetLLMContext + EstimateTokens). The
// `busy_timeout=5000` + single-conn-pool combo MUST absorb
// SQLITE_BUSY transparently — no caller-visible errors, no goroutine
// leak after the cohort returns.
func TestSQLite_Memory_Concurrent_BusyTimeoutAbsorbsContention(t *testing.T) {
	bus, store := buildDeps(t)
	dsn := filepath.Join(t.TempDir(), "concurrent.sqlite")

	s, err := sqlite.New(memory.ConfigSnapshot{
		Driver: "sqlite", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	const goroutines = 64
	const opsPerGo = 12

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)
	for i := range goroutines {

		go func() {
			defer wg.Done()
			ctx := context.Background()
			ident := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%7),
					UserID:    fmt.Sprintf("u-%d", i%11),
					SessionID: fmt.Sprintf("s-%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			for j := range opsPerGo {
				switch j % 5 {
				case 0:
					if err := s.Restore(ctx, ident, memory.Snapshot{}); err != nil {
						errCount.Add(1)
						t.Errorf("Restore (g=%d j=%d): %v", i, j, err)
					}
				case 1:
					if _, err := s.Snapshot(ctx, ident); err != nil {
						errCount.Add(1)
						t.Errorf("Snapshot (g=%d j=%d): %v", i, j, err)
					}
				case 2:
					if err := s.Flush(ctx, ident); err != nil {
						errCount.Add(1)
						t.Errorf("Flush (g=%d j=%d): %v", i, j, err)
					}
				case 3:
					if _, err := s.GetLLMContext(ctx, ident); err != nil {
						errCount.Add(1)
						t.Errorf("GetLLMContext (g=%d j=%d): %v", i, j, err)
					}
				case 4:
					if _, err := s.EstimateTokens(ctx, ident); err != nil {
						errCount.Add(1)
						t.Errorf("EstimateTokens (g=%d j=%d): %v", i, j, err)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errCount.Load(); n != 0 {
		t.Fatalf("%d concurrent operations errored — busy_timeout did not absorb contention", n)
	}

	// Goroutine-leak gate: bounded real-time deadline + Gosched, no
	// time.Sleep for synchronisation (AGENTS.md §11).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
