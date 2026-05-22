package governance_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// fakeClock is a controllable Clock for time-based bucket-refill tests.
// Operators advance the clock with `Advance`; concurrent reads return
// the current value atomically.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0).UTC()}
}

func TestRateLimiter_LatentPermitsAllCalls(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	rl, err := governance.NewRateLimiter(st, bus, governance.Config{})
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	for i := range 50 {
		if err := rl.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Errorf("PreCall[%d] under latent default: %v", i, err)
		}
	}
}

func TestRateLimiter_DrainsAndRefills(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	clk := newFakeClock()
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{
				Capacity:       3,
				RefillTokens:   3,
				RefillInterval: time.Minute,
			}},
		},
		Clock: clk,
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	one := 1
	req := llm.CompleteRequest{Model: "m", MaxTokens: &one}

	// Drain 3 calls (Capacity).
	for i := range 3 {
		if err := rl.PreCall(ctx, req); err != nil {
			t.Fatalf("drain #%d: %v", i, err)
		}
	}
	// 4th call → ErrRateLimited.
	if err := rl.PreCall(ctx, req); !errors.Is(err, governance.ErrRateLimited) {
		t.Errorf("expected ErrRateLimited, got %v", err)
	}
	// Advance one refill interval — bucket fills back to Capacity.
	clk.Advance(time.Minute)
	if err := rl.PreCall(ctx, req); err != nil {
		t.Errorf("post-refill PreCall: %v", err)
	}
}

func TestRateLimiter_EmitsEventOnBlock(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{Capacity: 1}},
		},
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	one := 1
	req := llm.CompleteRequest{Model: "m", MaxTokens: &one}

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "T", User: "U", Session: "S",
		Types: []events.EventType{governance.EventTypeRateLimited},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	if err := rl.PreCall(ctx, req); err != nil {
		t.Fatalf("first PreCall: %v", err)
	}
	if err := rl.PreCall(ctx, req); !errors.Is(err, governance.ErrRateLimited) {
		t.Fatalf("second PreCall: want ErrRateLimited, got %v", err)
	}
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.RateLimitedPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Capacity != 1 || p.Requested != 1 {
			t.Errorf("payload = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not observe rate_limited event within 2s")
	}
}

func TestRateLimiter_IdentityIsolation(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{Capacity: 2}},
		},
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	one := 1
	req := llm.CompleteRequest{Model: "m", MaxTokens: &one}

	ctxA := ctxWith(t, "A", "uA", "sA", "rA")
	ctxB := ctxWith(t, "B", "uB", "sB", "rB")

	for i := range 2 {
		if err := rl.PreCall(ctxA, req); err != nil {
			t.Fatalf("A drain[%d]: %v", i, err)
		}
	}
	// A is empty; B should still be at full capacity.
	if err := rl.PreCall(ctxB, req); err != nil {
		t.Errorf("B blocked by A's drain: %v", err)
	}
	if err := rl.PreCall(ctxA, req); !errors.Is(err, governance.ErrRateLimited) {
		t.Errorf("A: want ErrRateLimited, got %v", err)
	}
}

func TestRateLimiter_ConcurrentDrainNeverNegative(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	const N = 128
	const capacity = 16
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{Capacity: capacity}},
		},
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	q := identity.MustQuadrupleFrom(ctx)
	one := 1
	req := llm.CompleteRequest{Model: "m", MaxTokens: &one}

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var permitted, blocked atomic.Int32
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rl.PreCall(ctx, req); err != nil {
				if errors.Is(err, governance.ErrRateLimited) {
					blocked.Add(1)
					return
				}
				t.Errorf("unexpected err: %v", err)
				return
			}
			permitted.Add(1)
		}()
	}
	wg.Wait()
	snap, err := rl.Snapshot(ctx, q)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	level := snap["m"]
	if level < 0 {
		t.Errorf("bucket went negative: %d", level)
	}
	if permitted.Load() > capacity {
		t.Errorf("permitted=%d exceeded capacity=%d", permitted.Load(), capacity)
	}
	if int(permitted.Load())+level != capacity {
		t.Errorf("permitted(%d) + level(%d) != capacity(%d)", permitted.Load(), level, capacity)
	}
	if blocked.Load() == 0 {
		t.Errorf("no calls blocked across %d concurrent drains", N)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

func TestRateLimiter_RequestedTokensDefault(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{Capacity: 5}},
		},
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	// No MaxTokens — default drain = 1 each → 5 calls succeed, 6th blocks.
	for i := range 5 {
		if err := rl.PreCall(ctx, llm.CompleteRequest{Model: "m"}); err != nil {
			t.Fatalf("drain[%d]: %v", i, err)
		}
	}
	if err := rl.PreCall(ctx, llm.CompleteRequest{Model: "m"}); !errors.Is(err, governance.ErrRateLimited) {
		t.Errorf("want ErrRateLimited, got %v", err)
	}
}

func TestRateLimiter_OneShotBucket_NoRefill(t *testing.T) {
	t.Parallel()
	bus, st, cleanup := busAndState(t)
	defer cleanup()
	clk := newFakeClock()
	cfg := governance.Config{
		DefaultTier: "free",
		IdentityTiers: map[string]governance.TierConfig{
			"free": {RateLimit: governance.RateLimitConfig{Capacity: 2}}, // no refill knobs
		},
		Clock: clk,
	}
	rl, err := governance.NewRateLimiter(st, bus, cfg)
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	defer rl.Close(context.Background())
	ctx := ctxWith(t, "T", "U", "S", "R")
	one := 1
	req := llm.CompleteRequest{Model: "m", MaxTokens: &one}
	// Drain to zero.
	for i := range 2 {
		if err := rl.PreCall(ctx, req); err != nil {
			t.Fatalf("drain[%d]: %v", i, err)
		}
	}
	clk.Advance(24 * time.Hour) // would refill if interval > 0
	if err := rl.PreCall(ctx, req); !errors.Is(err, governance.ErrRateLimited) {
		t.Errorf("one-shot bucket refilled despite zero RefillInterval: err=%v", err)
	}
}

func TestNewRateLimiter_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := governance.NewRateLimiter(nil, nil, governance.Config{}); err == nil {
		t.Errorf("nil state.StateStore: want error, got nil")
	}
	_, st, cleanup := busAndState(t)
	defer cleanup()
	if _, err := governance.NewRateLimiter(st, nil, governance.Config{}); err == nil {
		t.Errorf("nil events.EventBus: want error, got nil")
	}
}
