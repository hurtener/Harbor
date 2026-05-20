package mcp

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestRegistry_ListServers_ConcurrentReuse discharges the D-025
// concurrent-reuse contract for the Phase 73k MCP Registry read API.
//
// It runs N=128 concurrent invocations against a single shared Registry
// reader under `-race`, asserting:
//
//   - no data races (the race detector is the gate);
//   - no context bleed (each goroutine carries a distinct identity
//     quadruple — tenant t-<i>, user u-<i>, session s-<i> — and a
//     filter-by-name-prefix that must return only that goroutine's
//     server);
//   - no cancellation cross-talk (a subset of goroutines run a
//     pre-cancelled ctx; cancelling them MUST NOT affect the others);
//   - no goroutine leaks (baseline runtime.NumGoroutine restored after
//     every goroutine joins).
func TestRegistry_ListServers_ConcurrentReuse(t *testing.T) {
	const n = 128

	r := NewRegistry(WithRegistryClock(func() time.Time {
		return time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	}))
	// Register N servers so each goroutine has a distinct name-prefix
	// target — a goroutine filtering on "srv-007-" must see only
	// "srv-007".
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("srv-%03d", i)
		if err := r.Register(ServerRegistration{
			Provider:     &stubProvider{id: tools.ToolSourceID(name), toolNames: []string{"t"}},
			Transport:    "http+sse",
			InitialState: ServerStateOnline,
		}); err != nil {
			t.Fatalf("Register %s: %v", name, err)
		}
	}

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, err := identity.With(context.Background(), identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			})
			if err != nil {
				errs <- fmt.Errorf("goroutine %d: identity.With: %w", i, err)
				return
			}
			// Pre-cancel a quarter of the goroutines' ctx to prove
			// cancellation does not cross-talk to the others.
			if i%4 == 0 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				if _, _, lerr := r.ListServers(cctx, ListFilter{}); lerr == nil {
					errs <- fmt.Errorf("goroutine %d: cancelled ctx should have errored", i)
				}
				return
			}
			// The remaining goroutines filter on their own server's
			// name prefix — a context-bleed bug would surface as the
			// wrong server (or extra servers) in the result.
			want := fmt.Sprintf("srv-%03d", i)
			rows, _, lerr := r.ListServers(ctx, ListFilter{NamePrefix: want})
			if lerr != nil {
				errs <- fmt.Errorf("goroutine %d: ListServers: %w", i, lerr)
				return
			}
			if len(rows) != 1 || rows[0].Name != want {
				errs <- fmt.Errorf("goroutine %d: context bleed — want [%s], got %v", i, want, rows)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Goroutine-leak check: allow the scheduler a moment to reap, then
	// assert the count returned to baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, got)
	}
}
