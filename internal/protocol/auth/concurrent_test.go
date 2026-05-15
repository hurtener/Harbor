package auth_test

// concurrent_test.go — the Phase 61 D-025 concurrent-reuse contract.
//
// CLAUDE.md §5 + §11 + D-025: every reusable artifact MUST ship a
// concurrent-reuse test that runs N≥100 invocations against a single
// shared instance under -race, asserting:
//
//   - no data races (the race detector is the gate);
//   - no context bleed (run A's identity claim never reaches run B);
//   - no cross-cancellation (cancelling run A's ctx does NOT affect
//     run B's verification);
//   - no goroutine leak (baseline runtime.NumGoroutine restored after
//     all invocations return).
//
// The auth.Validator is a compiled artifact (immutable after
// NewValidator returns; reads from ctx + raw token, writes nothing on
// the struct after construction). One Validator MUST serve N≥120
// concurrent goroutines.

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

const (
	// concurrentN is the per-test concurrency floor — well above the
	// D-025 N≥100 minimum (CLAUDE.md §5 + §11).
	concurrentN = 128
)

// TestConcurrentReuse_Validator pins D-025: one Validator serves N≥120
// concurrent Validate goroutines, distinct per-goroutine identity
// claims, with the race detector live and a goroutine-baseline
// assertion at teardown.
func TestConcurrentReuse_Validator(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	// Pre-mint N tokens, each with a distinct (tenant, user, session)
	// triple. Pre-minting (rather than minting in-goroutine) keeps the
	// concurrent path focused on Validate, not on signing — the race
	// detector watches Validate, which is the artifact under test.
	tokens := make([]string, concurrentN)
	for i := range tokens {
		c := jwt.MapClaims{
			"iss":     "https://idp.test",
			"sub":     fmt.Sprintf("user-%04d", i),
			"exp":     fixedNow.Add(15 * time.Minute).Unix(),
			"tenant":  fmt.Sprintf("tenant-%04d", i),
			"user":    fmt.Sprintf("user-%04d", i),
			"session": fmt.Sprintf("sess-%04d", i),
		}
		tokens[i] = signRS256(t, priv, c, "k1")
	}

	// Settle the goroutine baseline. A few iterations of GC + a short
	// pause let any test-runner-managed goroutines settle; we measure
	// AFTER the cooldown so spurious goroutine creation in the
	// runtime/test framework does not contaminate the leak check.
	settleGoroutines()
	baseline := runtime.NumGoroutine()

	// Fan out N concurrent Validate calls. The barrier (the start
	// channel) lines them up so they all enter Validate around the
	// same moment, maximising race-detector signal.
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, concurrentN)
	)
	for i := 0; i < concurrentN; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			verified, err := v.Validate(context.Background(), tokens[idx])
			if err != nil {
				errs[idx] = err
				return
			}
			// Per-goroutine context-bleed assertion: the verified
			// identity MUST be the goroutine's own claim. If it ever
			// returns the WRONG goroutine's identity (a context
			// bleed) the test fails loud.
			wantTenant := fmt.Sprintf("tenant-%04d", idx)
			if verified.Identity.TenantID != wantTenant {
				errs[idx] = fmt.Errorf("context bleed: goroutine %d got tenant %q, want %q",
					idx, verified.Identity.TenantID, wantTenant)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Goroutine-leak check: every spawned goroutine must have
	// returned. Settle the baseline again before the comparison so any
	// runtime-side cleanup goroutines (e.g. GC sweep workers) do not
	// produce a false positive.
	settleGoroutines()
	post := runtime.NumGoroutine()
	if post > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d, post=%d (drift=%d) — Validate spawned goroutines that did not return",
			baseline, post, post-baseline)
	}
}

// TestConcurrentReuse_Validator_CancellationIsolated pins D-025's
// cross-cancellation guarantee: cancelling run A's ctx MUST NOT affect
// run B's Validate call. Validate does not read ctx for any
// long-running blocking operation (it is CPU-bound on the JWT parse +
// verify), so cancellation isolation is structural — but we exercise
// it explicitly in case a future refactor introduces a ctx-blocking
// path.
func TestConcurrentReuse_Validator_CancellationIsolated(t *testing.T) {
	priv, pub := loadTestRS256(t)
	keys := newStaticKeySet()
	keys.add("k1", "RS256", pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return fixedNow }), withTestRedactor())
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	tok := signRS256(t, priv, validClaims(fixedNow), "k1")

	// Two parallel Validate calls: one with a cancelled ctx (A), one
	// with a live ctx (B). B MUST succeed irrespective of A's
	// cancellation.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	liveCtx := context.Background()

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() { defer wg.Done(); _, errA = v.Validate(cancelCtx, tok) }()
	go func() { defer wg.Done(); _, errB = v.Validate(liveCtx, tok) }()
	wg.Wait()

	// errA may be nil OR may carry a ctx-cancel-shaped error — either
	// is acceptable; the load-bearing assertion is errB succeeds.
	_ = errA
	if errB != nil {
		t.Errorf("live-ctx Validate failed despite the parallel cancel: %v", errB)
	}
}

// settleGoroutines runs a few GC + sched-yield iterations so the
// goroutine count quiesces before a leak check. Tests that count
// goroutines must do this — the test runner itself spawns / reaps
// goroutines (timers, GC workers) in the background.
func settleGoroutines() {
	for i := 0; i < 5; i++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}
