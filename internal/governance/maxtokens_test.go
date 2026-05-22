package governance_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestMaxTokens_LatentPermitsAllValues(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	e := governance.NewMaxTokensEnforcer(bus, governance.Config{})
	ctx := ctxWith(t, "T", "U", "S", "R")
	huge := 1_000_000
	req := llm.CompleteRequest{Model: "m", MaxTokens: &huge}
	if err := e.PreCall(ctx, req); err != nil {
		t.Errorf("PreCall under latent default returned: %v", err)
	}
}

func TestMaxTokens_BlocksOverCap(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	ctx := ctxWith(t, "T", "U", "S", "R")

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
		Types: []events.EventType{governance.EventTypeMaxTokensExceeded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// Under cap: permits.
	under := 50
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &under}); err != nil {
		t.Errorf("under cap PreCall: %v", err)
	}
	// At cap: permits (≤ cap).
	at := 100
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &at}); err != nil {
		t.Errorf("at cap PreCall: %v", err)
	}
	// Over cap: fail loud.
	over := 200
	err = e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &over})
	if !errors.Is(err, governance.ErrMaxTokensExceeded) {
		t.Fatalf("over cap PreCall: want ErrMaxTokensExceeded, got %v", err)
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.MaxTokensExceededPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Requested != 200 || p.Cap != 100 {
			t.Errorf("payload = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not observe maxtokens_exceeded event within 2s")
	}
}

func TestMaxTokens_NilOrZeroRequestedPermits(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	ctx := ctxWith(t, "T", "U", "S", "R")
	// nil MaxTokens — permits.
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
		t.Errorf("nil MaxTokens PreCall: %v", err)
	}
	// Zero MaxTokens — permits.
	zero := 0
	if err := e.PreCall(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &zero}); err != nil {
		t.Errorf("zero MaxTokens PreCall: %v", err)
	}
}

func TestMaxTokens_FailsClosedOnMissingIdentity(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier:   "free",
		IdentityTiers: map[string]governance.TierConfig{"free": {MaxTokens: 100}},
	}
	e := governance.NewMaxTokensEnforcer(bus, cfg)
	over := 200
	err := e.PreCall(context.Background(), llm.CompleteRequest{Model: "m", MaxTokens: &over})
	if !errors.Is(err, governance.ErrIdentityRequired) {
		t.Errorf("missing identity: want ErrIdentityRequired, got %v", err)
	}
}

func TestMaxTokens_PostCallNoop(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()
	e := governance.NewMaxTokensEnforcer(bus, governance.Config{})
	ctx := ctxWith(t, "T", "U", "S", "R")
	if err := e.PostCall(ctx, llm.CompleteRequest{}, llm.CompleteResponse{}, nil); err != nil {
		t.Errorf("PostCall: %v", err)
	}
}

// TestMaxTokens_ConcurrentReuse_D025 — Wave 7b audit FAIL #3 closes:
// N≥100 concurrent goroutines invoke PreCall against a single shared
// `MaxTokensEnforcer` configured with one tier capping at 100. Half the
// goroutines request 50 tokens (under cap → permit) and half request
// 1_000_000 (over cap → reject). The test asserts: (a) every goroutine
// observes the right outcome (no permit/reject crosstalk), (b) no race
// detector hits, (c) baseline goroutine count restored after teardown,
// (d) the bus subscriber observes exactly one `governance.maxtokens_exceeded`
// event per rejecting call (no double-emit, no missed emit).
func TestMaxTokens_ConcurrentReuse_D025(t *testing.T) {
	t.Parallel()
	bus, _, cleanup := busAndState(t)
	defer cleanup()

	const n = 128
	const cap = 100
	cfg := governance.Config{
		DefaultTier: "default",
		IdentityTiers: map[string]governance.TierConfig{
			"default": {MaxTokens: cap},
		},
	}
	enforcer := governance.NewMaxTokensEnforcer(bus, cfg)

	// Subscribe once before the goroutines fire so we count rejection
	// events without losing emissions during channel attach.
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{governance.EventTypeMaxTokensExceeded},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var permits atomic.Int64
	var rejects atomic.Int64
	var errs atomic.Int64

	wg.Add(n)
	for i := range n {

		go func() {
			defer wg.Done()
			// Per-call identity so the per-key state map is exercised.
			ctx := ctxWith(t, "T", "U", fmt.Sprintf("S-%d", i), "R")
			var requested int
			if i%2 == 0 {
				requested = 50 // under cap → permit
			} else {
				requested = 1_000_000 // over cap → reject
			}
			req := llm.CompleteRequest{Model: "m", MaxTokens: &requested}
			err := enforcer.PreCall(ctx, req)
			switch {
			case err == nil && requested <= cap:
				permits.Add(1)
			case errors.Is(err, governance.ErrMaxTokensExceeded) && requested > cap:
				rejects.Add(1)
			default:
				errs.Add(1)
				t.Errorf("goroutine %d: requested=%d got err=%v", i, requested, err)
			}
		}()
	}
	wg.Wait()

	if errs.Load() != 0 {
		t.Fatalf("%d goroutines saw unexpected outcomes", errs.Load())
	}
	if got, want := permits.Load(), int64(n/2); got != want {
		t.Errorf("permits=%d want %d", got, want)
	}
	if got, want := rejects.Load(), int64(n/2); got != want {
		t.Errorf("rejects=%d want %d", got, want)
	}

	// Drain the bus subscription — should see exactly n/2 events.
	gotEvents := 0
	drainDeadline := time.NewTimer(2 * time.Second)
	defer drainDeadline.Stop()
drain:
	for gotEvents < n/2 {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break drain
			}
			if ev.Type != governance.EventTypeMaxTokensExceeded {
				t.Errorf("unexpected event type=%s", ev.Type)
				continue
			}
			gotEvents++
		case <-drainDeadline.C:
			break drain
		}
	}
	if gotEvents != n/2 {
		t.Errorf("observed %d maxtokens_exceeded events, want %d", gotEvents, n/2)
	}

	// Goroutine baseline restored — same tolerance as Phase 36a cost
	// stress (+5 absorbs sub.Cancel teardown + the bus's reader).
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if leak := runtime.NumGoroutine() - baseline; leak > 5 {
		t.Errorf("goroutine leak after N=%d concurrent PreCalls: %d above baseline", n, leak)
	}
}
