package durable_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestDurable_ConcurrentReuse_Isolation is the D-025 concurrent-reuse +
// cross-session isolation gate: many goroutines across several sessions
// drive ONE shared durable bus; each session's subscriber must receive
// exactly its own envelopes (no context bleed). Run under -race.
func TestDurable_ConcurrentReuse_Isolation(t *testing.T) {
	const sessions = 3
	const perSession = 40 // 3*40 = 120 concurrent publishers (>= 100)

	store := newStore(t)
	eb := newEventBus(t)
	b := openOver(t, store, eb)
	defer func() {
		ctx := context.Background()
		_ = b.Close(ctx)
		_ = eb.Close(ctx)
		_ = store.Close(ctx)
	}()

	ids := make([]identity.Quadruple, sessions)
	subs := make([]events.Subscription, sessions)
	for s := range sessions {
		ids[s] = identity.Quadruple{Identity: identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%d", s),
			UserID:    fmt.Sprintf("user-%d", s),
			SessionID: fmt.Sprintf("sess-%d", s),
		}}
		subs[s] = subscribe(t, eb, ids[s].Identity)
		defer subs[s].Cancel()
	}

	var wg sync.WaitGroup
	for s := range sessions {
		for i := range perSession {
			wg.Add(1)
			go func(s, i int) {
				defer wg.Done()
				env := envelope("c", ids[s], fmt.Sprintf("evt-%d-%d", s, i))
				if err := b.Publish(ctxFor(t, ids[s].Identity), env); err != nil {
					t.Errorf("publish s=%d i=%d: %v", s, i, err)
				}
			}(s, i)
		}
	}
	wg.Wait()

	// Each session's subscriber must receive exactly its own perSession
	// envelopes — no cross-session bleed.
	for s := range sessions {
		got := collectEdges(subs[s], perSession, 8*time.Second)
		if len(got) != perSession {
			t.Errorf("session %d: received %d envelopes, want %d (bleed or loss)", s, len(got), perSession)
		}
		// Any extra (cross-bleed from another session, or a poller
		// double-project) is a failure.
		if extra := collectEdges(subs[s], 1, 200*time.Millisecond); len(extra) != 0 {
			t.Errorf("session %d: received MORE than its own %d envelopes (bleed/double-project)", s, perSession)
		}
	}
}

// TestDurable_NoGoroutineLeak asserts the poller goroutine is joined on
// Close — the baseline goroutine count is restored after teardown.
func TestDurable_NoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	store := newStore(t)
	eb := newEventBus(t)
	b := openOver(t, store, eb)

	id := triple()
	for i := range 10 {
		if err := b.Publish(ctxFor(t, id.Identity), envelope("g", id, fmt.Sprintf("evt-%d", i))); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	_ = b.Close(context.Background())
	_ = eb.Close(context.Background())
	_ = store.Close(context.Background())

	for range 50 {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
}
