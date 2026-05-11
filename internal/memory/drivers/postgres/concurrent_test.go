package postgres_test

// Driver-local concurrency stress for the Postgres MemoryStore
// driver. The conformance suite's `Concurrent_AllMethods_NoRace`
// already runs N=128 goroutines against a single shared driver
// instance (the primary D-025 gate); this file adds a supplemental
// cohort that hammers the Snapshot/Restore + Flush paths
// specifically.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memorydriverpostgres "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
)

// TestPostgres_Memory_Concurrent runs an N=100 cohort exercising
// every method against a single shared driver instance. We assert no
// caller-visible errors, no data races (via -race), and no goroutine
// leak after the cohort returns.
func TestPostgres_Memory_Concurrent(t *testing.T) {
	baseDSN := requireDSN(t)
	dsn := freshSchema(t, baseDSN)
	bus, store := buildDeps(t)

	s, err := memorydriverpostgres.New(memory.ConfigSnapshot{
		Driver: "postgres", DSN: dsn, Strategy: memory.StrategyNone,
	}, memory.Deps{State: store, Bus: bus})
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	const goroutines = 100
	const opsPerGo = 8

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := context.Background()
			ident := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%17),
					UserID:    fmt.Sprintf("u-%d", i%41),
					SessionID: fmt.Sprintf("s-%d", i),
				},
				RunID: fmt.Sprintf("run-%d", i),
			}
			for j := 0; j < opsPerGo; j++ {
				switch j % 7 {
				case 0:
					if err := s.AddTurn(ctx, ident, memory.ConversationTurn{}); err != nil {
						errCount.Add(1)
					}
				case 1:
					if _, err := s.GetLLMContext(ctx, ident); err != nil {
						errCount.Add(1)
					}
				case 2:
					if _, err := s.EstimateTokens(ctx, ident); err != nil {
						errCount.Add(1)
					}
				case 3:
					if err := s.Flush(ctx, ident); err != nil {
						errCount.Add(1)
					}
				case 4:
					if _, err := s.Health(ctx, ident); err != nil {
						errCount.Add(1)
					}
				case 5:
					if _, err := s.Snapshot(ctx, ident); err != nil {
						errCount.Add(1)
					}
				case 6:
					if err := s.Restore(ctx, ident, memory.Snapshot{}); err != nil {
						errCount.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := errCount.Load(); n != 0 {
		t.Fatalf("%d concurrent operations errored", n)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
