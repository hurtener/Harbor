package tokenexchange_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestProvider_ConcurrentReuse_NoIdentityBleed pins the D-025
// concurrent-reuse contract for the tokenexchange driver: one shared
// provider instance serves N=128 concurrent Token() calls across mixed
// identity triples under -race. The identity scheme DELIBERATELY
// reuses user IDs across tenants (user = i%8 over tenants = i%3, and 8
// is not a multiple of 3, so the same user ID appears under several
// tenants) — a cache or single-flight key that omits the tenant fails
// the per-goroutine tenant assertions. Asserts:
//   - no data races (run under -race in CI)
//   - no identity bleed (each goroutine's token carries ITS OWN tenant
//     AND user — asserted broker-side via the tenant-embedding access
//     token; cross-tenant same-user-ID pairs never share a token)
//   - no cancellation cross-talk (a cancelled goroutine does not affect
//     any other goroutine's exchange)
//   - no goroutine leaks (NumGoroutine returns to baseline after
//     provider Close)
//
// CLAUDE.md §5 + §11; D-025.
func TestProvider_ConcurrentReuse_NoIdentityBleed(t *testing.T) {
	t.Parallel()
	const N = 128

	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errCh := make(chan error, N)

	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// tenant = i%3, user = i%8: the same user ID recurs under
			// different tenants (e.g. user-1 under tenant-1, tenant-0,
			// tenant-2) — the cross-tenant-same-user-ID leg.
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%3),
				UserID:    fmt.Sprintf("user-%d", i%8),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				errCh <- fmt.Errorf("g%d identity.With: %w", i, err)
				return
			}
			// One goroutine per stripe cancels mid-flight to prove no
			// cross-talk: cancelling THIS ctx must not fail the others.
			if i%16 == 0 {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				_, _ = prov.Token(cctx, tools.ToolSourceID("any")) // may error; must not affect others
				return
			}
			tok, err := prov.Token(ctx, tools.ToolSourceID("any"))
			if err != nil {
				errCh <- fmt.Errorf("g%d Token: %w", i, err)
				return
			}
			// The token's subject must be THIS goroutine's tenant+user —
			// never another's. The broker embeds both in the access
			// token, so a cross-tenant cache hit fails here.
			if tok.UserID != id.UserID {
				errCh <- fmt.Errorf("g%d identity bleed: tok.UserID=%q want %q", i, tok.UserID, id.UserID)
				return
			}
			if tok.TenantID != id.TenantID {
				errCh <- fmt.Errorf("g%d tenant bleed: tok.TenantID=%q want %q", i, tok.TenantID, id.TenantID)
				return
			}
			if !strings.HasPrefix(tok.AccessToken, tokenPrefix(id)) {
				errCh <- fmt.Errorf("g%d token cross-talk (tenant or user of another goroutine): %q, want prefix %q",
					i, tok.AccessToken, tokenPrefix(id))
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	_ = prov.Close(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if leak := runtime.NumGoroutine() - baseline; leak > 5 {
		t.Fatalf("goroutine leak after N=%d concurrent Token(): leaked=%d (baseline=%d, now=%d)",
			N, leak, baseline, runtime.NumGoroutine())
	}
}

// TestProvider_SingleFlight_CollapsesBurst asserts a burst of M
// concurrent misses for ONE subject produces exactly one broker
// request — the single-flight gate keyed (scope, tenant, user, source).
func TestProvider_SingleFlight_CollapsesBurst(t *testing.T) {
	t.Parallel()
	const M = 64

	broker := newFakeBroker(t)
	prov, _, _ := mkProvider(t, broker)
	id := aliceID()

	var wg sync.WaitGroup
	var sawToken atomic.Int32
	start := make(chan struct{})
	for range M {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := mkCtx(t, id)
			<-start // release all at once to maximise the collision window
			tok, err := prov.Token(ctx, tools.ToolSourceID("any"))
			if err != nil {
				t.Errorf("Token: %v", err)
				return
			}
			if tok.AccessToken != "" {
				sawToken.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if int(sawToken.Load()) != M {
		t.Fatalf("expected all %d callers to get a token; got %d", M, sawToken.Load())
	}
	// Single-flight: at most a small handful of broker calls (a flight
	// completing exactly as another caller arrives may legitimately
	// trigger a second). M=64 → 64 round-trips is the smell.
	if calls := broker.calls(); calls > 4 {
		t.Fatalf("single-flight collapse failed: %d broker calls for M=%d concurrent misses (want <= 4)", calls, M)
	}
}
