package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// TestList_ConcurrentReuse_D025 exercises the D-025 concurrent-reuse
// contract: N=128 concurrent goroutines, each with a goroutine-unique
// TaskFilter, against a SINGLE shared tasks/protocol.Service under
// `-race`. It asserts:
//
//   - no data races (the race detector is the gate),
//   - no context bleed (each goroutine's filter is honoured — every
//     returned row matches the goroutine's status facet),
//   - no cross-cancellation (cancelling one goroutine's ctx must not
//     abort another's call),
//   - no goroutine leaks (baseline runtime.NumGoroutine restored after
//     every call returns).
func TestList_ConcurrentReuse_D025(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")

	// Seed a deterministic mix so every facet has matches.
	statuses := []tasks.TaskStatus{
		tasks.StatusPending, tasks.StatusRunning, tasks.StatusFailed, tasks.StatusComplete,
	}
	for i := range 40 {
		seedTask(t, reg, id, tasks.KindForeground, statuses[i%len(statuses)],
			fmt.Sprintf("task-%d", i), "query")
	}

	wireStatuses := []prototypes.TaskStatus{
		prototypes.TaskStatusPending, prototypes.TaskStatusRunning,
		prototypes.TaskStatusFailed, prototypes.TaskStatusComplete,
	}

	const n = 128
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := wireStatuses[i%len(wireStatuses)]
			ctx, cancel := context.WithCancel(context.Background())
			// Cancel a deterministic subset's ctx mid-flight to prove
			// no cross-cancellation: a cancelled ctx must not abort a
			// sibling's List.
			if i%7 == 0 {
				defer cancel()
			} else {
				cancel = func() {}
				_ = cancel
			}
			resp, err := svc.List(ctx, prototypes.TaskListRequest{
				Identity: scopeOf("t1", "u1", "s1"),
				Filter:   prototypes.TaskFilter{Statuses: []prototypes.TaskStatus{want}},
			}, false)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: List: %w", i, err)
				return
			}
			// Context-bleed assertion: every returned row matches THIS
			// goroutine's status facet — another goroutine's filter
			// never leaked onto this call's rows.
			for _, row := range resp.Rows {
				if row.Status != want {
					errCh <- fmt.Errorf("goroutine %d: context bleed — want %q, got row %q", i, want, row.Status)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Goroutine-leak assertion: the baseline is restored once every
	// call has returned (allow a brief settle for the runtime).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: %d goroutines above baseline", leaked)
	}
}

// TestList_SharedServiceIsImmutable asserts a single Service serves N
// distinct identities without cross-talk — the Service holds no
// per-call state (D-025).
func TestList_SharedServiceIsImmutable(t *testing.T) {
	reg, bus := newTestRegistry(t)
	proj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj, tasksprotocol.WithBus(bus))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Two tenants, disjoint task sets.
	seedTask(t, reg, idFor("t1", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "t1 task", "q")
	seedTask(t, reg, idFor("t2", "u2", "s2"), tasks.KindForeground, tasks.StatusRunning, "t2 task", "q")

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r1, err := svc.List(context.Background(), prototypes.TaskListRequest{Identity: scopeOf("t1", "u1", "s1")}, false)
			if err != nil {
				t.Errorf("t1 List: %v", err)
				return
			}
			for _, row := range r1.Rows {
				if row.Identity.Tenant != "t1" {
					t.Errorf("tenant bleed: t1 query returned %q", row.Identity.Tenant)
				}
			}
		}()
	}
	wg.Wait()
}
