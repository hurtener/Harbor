package inmem_test

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestListWindow_ConcurrentReuse_ReuseContract pins the D-025 concurrent-
// reuse contract for the events.list windowed read: N≥100 concurrent
// ListWindow readers against ONE shared bus, under concurrent publishing,
// with -race. It asserts no torn pages (each page is internally
// oldest-first, strictly increasing), no cross-caller row bleed (a
// non-admin reader scoped to tenant T never sees another tenant's row),
// and a goroutine baseline restored after Close.
func TestListWindow_ConcurrentReuse_ReuseContract(t *testing.T) {
	bus, _ := newReplayBus(t)
	hr, ok := bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("bus does not implement events.HistoryReplayer")
	}

	const tenants = 16
	const publishers = 32
	const readers = 128 // ≥100 concurrent readers (D-025 / §11)
	const eventsPerPublisher = 24
	const readsPerReader = 8

	ids := make([]identity.Quadruple, tenants)
	for i := range ids {
		ids[i] = mkID(i)
	}

	var wg sync.WaitGroup

	// Publishers stream events for their tenant concurrently with the reads.
	for p := range publishers {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			id := ids[p%tenants]
			for j := range eventsPerPublisher {
				ev := events.Event{
					Type:     events.EventTypeRuntimeError,
					Identity: id,
					Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(p*1000 + j)},
				}
				_ = bus.Publish(context.Background(), ev)
			}
		}(p)
	}

	tornPages := atomic.Int64{}
	rowBleed := atomic.Int64{}

	for r := range readers {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := ids[r%tenants]
			wire := prototypes.EventFilter{
				TenantIDs:  []string{id.TenantID},
				UserIDs:    []string{id.UserID},
				SessionIDs: []string{id.SessionID},
			}
			for range readsPerReader {
				page, err := hr.ListWindow(context.Background(), events.EventListQuery{
					Filter: wire, Limit: 10,
				})
				if err != nil {
					continue
				}
				var prev uint64
				for i, ev := range page.Events {
					// No cross-caller row bleed: a non-admin read scoped to
					// this triple must never surface another identity's row.
					if ev.Identity.TenantID != id.TenantID ||
						ev.Identity.SessionID != id.SessionID {
						rowBleed.Add(1)
					}
					// Torn-page guard: strictly increasing (oldest-first).
					if i > 0 && ev.Sequence <= prev {
						tornPages.Add(1)
					}
					prev = ev.Sequence
				}
			}
		}(r)
	}

	wg.Wait()

	if n := rowBleed.Load(); n != 0 {
		t.Fatalf("cross-caller row bleed: %d rows from a foreign identity leaked into a scoped ListWindow", n)
	}
	if n := tornPages.Load(); n != 0 {
		t.Fatalf("torn pages: %d ListWindow pages were not strictly oldest-first", n)
	}

	// Goroutine baseline restored after Close.
	baseline := runtime.NumGoroutine()
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak after ListWindow stress: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
