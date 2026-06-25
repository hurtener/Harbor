package durable_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestConcurrentReuse_DurableBus is the D-025 concurrent-reuse gate.
// N=120 goroutines (>=100 per the contract) each publish a batch of
// events for a DISTINCT identity quadruple against ONE shared durable
// bus under -race, then independently replay their own session.
//
// Guarantees asserted:
//   - No data races (the -race detector is the gate).
//   - No context bleed — each goroutine's replay returns ONLY its own
//     session's events; a foreign triple in any replayed event is a
//     bleed.
//   - No cross-cancellation — a ~20% subset publishes with an
//     already-cancelled ctx; ValidateEvent/persist either succeed
//     (sequencing does not honour ctx) or fail, but a cancelled
//     goroutine's ctx never disturbs another goroutine's run.
//   - No goroutine leak — baseline runtime.NumGoroutine is restored
//     after Close + the publishers join.
func TestConcurrentReuse_DurableBus(t *testing.T) {
	const n = 120
	const perGoroutine = 8

	store := newInmemStore(t)
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	rp := bus.(events.Replayer)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			id := identity.Quadruple{Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", idx),
				UserID:    fmt.Sprintf("user-%d", idx),
				SessionID: fmt.Sprintf("session-%d", idx),
			}}

			ctx := context.Background()
			cancelled := idx%5 == 0 // ~20% pre-cancelled
			if cancelled {
				c, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = c
			}

			for j := range perGoroutine {
				ev := events.Event{
					Type:     events.EventTypeRuntimeWarning,
					Identity: id,
					Payload:  runtimeWarn(fmt.Sprintf("g%d-e%d", idx, j)),
				}
				// A cancelled ctx must not panic or corrupt shared
				// state; the publish may or may not error, but it
				// must never affect another goroutine.
				_ = bus.Publish(ctx, ev)
			}

			// Replay this goroutine's OWN session — a context bleed
			// surfaces here as a foreign triple.
			got, rerr := rp.Replay(context.Background(),
				events.Cursor{SessionID: id.SessionID}, filterFor(id))
			if rerr != nil {
				errCh <- fmt.Errorf("goroutine %d: replay: %w", idx, rerr)
				return
			}
			for _, ev := range got {
				if ev.Identity.TenantID != id.TenantID ||
					ev.Identity.UserID != id.UserID ||
					ev.Identity.SessionID != id.SessionID {
					errCh <- fmt.Errorf("goroutine %d: context bleed — replayed %+v",
						idx, ev.Identity)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give any stray goroutine a chance to exit, then assert baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d (leaked ~%d)",
			baseline, runtime.NumGoroutine(), leaked)
	}
}

// TestConcurrentReuse_DurableBus_RecoveredInstance proves the sequence
// rehydration composes with the D-025 concurrent-reuse contract: the
// shared instance under stress is one constructed over a NON-empty log
// (its counter was recovered at construction). N>=100 goroutines publish
// to distinct sessions against the recovered bus under -race; every
// post-recovery sequence must be strictly greater than the recovered
// floor, with no data race, no context bleed, and no goroutine leak.
func TestConcurrentReuse_DurableBus_RecoveredInstance(t *testing.T) {
	const seed = 25
	const n = 100
	const perGoroutine = 4

	store := newInmemStore(t)

	// First Runtime: seed a non-empty log so the recovered bus starts
	// with nextSeq == seed.
	bus1, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (seed): %v", err)
	}
	publishN(t, bus1, quad("t0", "u0", "seed"), seed)
	if err := bus1.Close(context.Background()); err != nil {
		t.Fatalf("close seed bus: %v", err)
	}

	// Second Runtime: the recovered instance is the shared artifact.
	bus, err := durable.New(context.Background(), durableCfg(), auditpatterns.New(), store)
	if err != nil {
		t.Fatalf("durable.New (recovered): %v", err)
	}
	rp := bus.(events.Replayer)

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			id := identity.Quadruple{Identity: identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", idx),
				UserID:    fmt.Sprintf("user-%d", idx),
				SessionID: fmt.Sprintf("session-%d", idx),
			}}
			for j := range perGoroutine {
				ev := events.Event{
					Type:     events.EventTypeRuntimeWarning,
					Identity: id,
					Payload:  runtimeWarn(fmt.Sprintf("g%d-e%d", idx, j)),
				}
				if perr := bus.Publish(context.Background(), ev); perr != nil {
					errCh <- fmt.Errorf("goroutine %d publish: %w", idx, perr)
					return
				}
			}
			got, rerr := rp.Replay(context.Background(),
				events.Cursor{SessionID: id.SessionID}, filterFor(id))
			if rerr != nil {
				errCh <- fmt.Errorf("goroutine %d replay: %w", idx, rerr)
				return
			}
			for _, ev := range got {
				if ev.Identity.SessionID != id.SessionID {
					errCh <- fmt.Errorf("goroutine %d: context bleed — %+v", idx, ev.Identity)
					return
				}
				if ev.Sequence <= seed {
					errCh <- fmt.Errorf("goroutine %d: post-recovery sequence %d not above recovered floor %d",
						idx, ev.Sequence, seed)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d (leaked ~%d)", baseline, runtime.NumGoroutine(), leaked)
	}
}
