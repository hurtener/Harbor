package harbortest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestRunOnce_ConcurrentReuse_NoCrossTalk — the D-025
// concurrent-reuse stress for the kit. N=100 concurrent RunOnce
// invocations, each with its own (tenant, user, session) triple
// and its own bus (the default zero-Deps shape). Asserts:
//
//  1. No race-detector hits (CI gate; we run under -race).
//  2. Per-goroutine captured logs contain ONLY events tagged with
//     that goroutine's identity triple (no cross-talk).
//  3. The goroutine baseline is restored after every RunOnce
//     returns (no subscription-drain goroutine leak).
//
// We use per-goroutine buses (the zero-Deps shape) because that's
// the natural test-author pattern when running parallel agents in
// isolation. Sharing a bus across concurrent Admin subscribers
// causes every subscriber to see every other run's events — that's
// the Admin filter's documented behaviour, not a leak; AssertNoLeaks
// is the kit's tool for analysing that union log.
//
// A second test below covers the shared-bus shape via the
// FaultInjector concurrent reuse — the bus's D-025 contract is
// already pinned by internal/events tests.
func TestRunOnce_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const n = 100

	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    "u",
				SessionID: fmt.Sprintf("session-%d", i),
			}
			runID := fmt.Sprintf("run-%d", i)

			agent := harbortest.AgentFunc(func(ctx context.Context, _ any) (any, error) {
				b := events.MustFrom(ctx)
				return nil, b.Publish(ctx, events.Event{
					Type:     events.EventTypeRuntimeWarning,
					Identity: identity.Quadruple{Identity: id, RunID: runID},
					Payload:  &events.RedactedMap{Data: map[string]any{"i": i}},
				})
			})

			// Zero-deps RunOnce: each goroutine opens + closes its own
			// in-mem bus. The kit's contract is that this is safe and
			// leak-free under concurrent use.
			_, log, err := harbortest.RunOnce(context.Background(), agent, nil, harbortest.Deps{
				Identity: &id,
				RunID:    runID,
			})
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}

			// Per-goroutine isolation: the only non-empty triple in the
			// captured log MUST be this goroutine's triple. Bus-internal
			// events (audit.admin_scope_used) carry the subscriber's
			// empty pre-publish identity — that's fine.
			for _, ev := range log.All() {
				// Bus-internal events from the subscribe path carry
				// empty identity; those are not cross-talk, they're
				// the subscription-side bookkeeping.
				if ev.Identity.TenantID == "" && ev.Identity.UserID == "" && ev.Identity.SessionID == "" {
					continue
				}
				if ev.Identity.TenantID != id.TenantID || ev.Identity.SessionID != id.SessionID {
					errs <- fmt.Errorf("run %d: cross-talk: event type=%q identity=%+v want triple %+v",
						i, ev.Type, ev.Identity, id)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// Baseline restoration with slack for scheduler noise.
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > baseline+12 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, after)
	}
}

// TestFaultInjector_ConcurrentReuse — N concurrent invocations of a
// wrapped tool with N injected failures. Asserts the counter pops
// exactly N times under -race.
//
// (D-025 concurrent reuse: the FaultInjector IS a compiled artifact
// shared across goroutines.)
func TestFaultInjector_ConcurrentReuse(t *testing.T) {
	const n = 100
	inner := tools.NewCatalog()
	registerAdd(t, inner)
	inj := harbortest.NewFaultInjector(inner)

	// Inject exactly n transient failures so the count is exact.
	harbortest.SimulateFailure(inj, "add", tools.ErrClassTransient, n)

	cat := inj.Catalog()
	d, _ := cat.Resolve("add")
	args, _ := json.Marshal(addArgs{A: 1, B: 1})

	var wg sync.WaitGroup
	var failures, successes int64
	for i := 0; i < n*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.Invoke(context.Background(), args)
			if err != nil {
				atomic.AddInt64(&failures, 1)
			} else {
				atomic.AddInt64(&successes, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&failures); got != int64(n) {
		t.Errorf("failures = %d, want %d", got, n)
	}
	if got := atomic.LoadInt64(&successes); got != int64(n) {
		t.Errorf("successes = %d, want %d", got, n)
	}
}
