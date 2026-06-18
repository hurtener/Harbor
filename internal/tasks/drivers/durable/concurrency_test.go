package durable_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDurable_ConcurrentReuse is the D-025 concurrent-reuse gate: N
// concurrent goroutines drive ONE shared durable driver, each in its
// own session, and each asserts it sees only its own task (no context
// bleed). Run under -race to catch data races; the per-session List
// assertion catches cross-session leakage.
func TestDurable_ConcurrentReuse(t *testing.T) {
	const n = 128

	store := newStore(t)
	bus := mkBus(t)
	r := openOver(t, store, bus)
	defer func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Quadruple{Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("sess-%d", i),
			}}
			ctx, err := identity.With(context.Background(), id.Identity)
			if err != nil {
				errCh <- err
				return
			}
			h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "c"})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Spawn: %w", i, err)
				return
			}
			if err := r.MarkRunning(ctx, h.ID); err != nil {
				errCh <- fmt.Errorf("goroutine %d MarkRunning: %w", i, err)
				return
			}
			got, err := r.Get(ctx, h.ID)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d Get: %w", i, err)
				return
			}
			if got.ID != h.ID {
				errCh <- fmt.Errorf("goroutine %d Get returned wrong task %s", i, got.ID)
				return
			}
			// Per-session List must contain exactly this goroutine's task
			// — proof of no cross-session bleed.
			list, err := r.List(ctx, id.Identity, tasks.TaskFilter{})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d List: %w", i, err)
				return
			}
			if len(list) != 1 || list[0].ID != h.ID {
				errCh <- fmt.Errorf("goroutine %d List = %d tasks (want exactly its own)", i, len(list))
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TestDurable_ConcurrentSameTask_ToolCount complements the per-session
// reuse test with SAME-task contention: N goroutines IncrementToolCount
// on one shared task; the final count must be exactly N (no torn writes
// / lost updates under the engine lock).
func TestDurable_ConcurrentSameTask_ToolCount(t *testing.T) {
	const n = 200
	store := newStore(t)
	bus := mkBus(t)
	r := openOver(t, store, bus)
	defer func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}()

	id := quadA()
	ctx := ctxFor(t, id.Identity)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "shared"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.IncrementToolCount(ctx, h.ID); err != nil {
				t.Errorf("IncrementToolCount: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ToolCount != n {
		t.Errorf("ToolCount = %d after %d concurrent increments, want %d (lost update / torn write)", got.ToolCount, n, n)
	}
}

// TestDurable_NoGoroutineLeak asserts the driver starts no goroutine
// that outlives Close — the baseline count is restored after teardown.
func TestDurable_NoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	store := newStore(t)
	bus := mkBus(t)
	r := openOver(t, store, bus)

	id := quadA()
	ctx := ctxFor(t, id.Identity)
	for range 20 {
		h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground, Description: "g"})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		_ = r.MarkRunning(ctx, h.ID)
		_ = r.MarkComplete(ctx, h.ID, tasks.TaskResult{})
	}

	_ = r.Close(context.Background())
	_ = bus.Close(context.Background())
	_ = store.Close(context.Background())

	// Allow any transient goroutines to wind down.
	for range 50 {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
}
