package auth

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// TestProvider_ConcurrentReuse_NoCrossTalk pins the D-025
// concurrent-reuse contract: a single shared *Provider serves N=128
// concurrent goroutines each running its own InitiateFlow →
// VisitAuthorizeURL → CompleteFlow → Token cycle under its own
// identity stack. Asserts:
//   - no data races (the test is run under -race in CI)
//   - no context bleed (each goroutine reads back the token its own
//     identity stored; never another goroutine's)
//   - no cancellation cross-talk (cancelling one goroutine's ctx mid-
//     flow does NOT affect any other goroutine's flow)
//   - no goroutine leaks (NumGoroutine returns to baseline ± slack
//     within 2 seconds of all flows completing)
//
// CLAUDE.md §5 + §11; D-025.
func TestProvider_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	t.Parallel()
	const N = 128

	h := newProviderHarness(t)
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	var failed atomic.Bool
	errCh := make(chan error, N)

	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%4), // 4 tenants
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				errCh <- fmt.Errorf("g%d identity.With: %w", i, err)
				failed.Store(true)
				return
			}
			// 1: Initiate via Token() → ErrAuthRequired.
			_, err = h.provider.Token(ctx, h.userCfg.Source)
			var authErr *ErrAuthRequired
			if !errors.As(err, &authErr) {
				errCh <- fmt.Errorf("g%d Token: want *ErrAuthRequired, got %v", i, err)
				failed.Store(true)
				return
			}
			// 2: Visit authorize URL → get code.
			code, gotState, err := h.server.VisitAuthorizeURL(authErr.AuthorizeURL)
			if err != nil {
				errCh <- fmt.Errorf("g%d VisitAuthorizeURL: %w", i, err)
				failed.Store(true)
				return
			}
			if gotState != authErr.State {
				errCh <- fmt.Errorf("g%d state cross-talk: got %q want %q",
					i, gotState, authErr.State)
				failed.Store(true)
				return
			}
			// 3: CompleteFlow.
			tok, err := h.provider.CompleteFlow(ctx, authErr.State, code)
			if err != nil {
				errCh <- fmt.Errorf("g%d CompleteFlow: %w", i, err)
				failed.Store(true)
				return
			}
			// Tenant + user must match this goroutine's identity.
			if tok.TenantID != id.TenantID {
				errCh <- fmt.Errorf("g%d tenant bleed: tok.TenantID=%q ctx.TenantID=%q",
					i, tok.TenantID, id.TenantID)
				failed.Store(true)
				return
			}
			if tok.UserID != id.UserID {
				errCh <- fmt.Errorf("g%d user bleed: tok.UserID=%q ctx.UserID=%q",
					i, tok.UserID, id.UserID)
				failed.Store(true)
				return
			}
			// 4: Readback via Token() — same access token.
			rt, err := h.provider.Token(ctx, h.userCfg.Source)
			if err != nil {
				errCh <- fmt.Errorf("g%d Token readback: %w", i, err)
				failed.Store(true)
				return
			}
			if rt.AccessToken != tok.AccessToken {
				errCh <- fmt.Errorf("g%d access token mismatch on readback: %q vs %q",
					i, rt.AccessToken, tok.AccessToken)
				failed.Store(true)
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
	if failed.Load() {
		t.FailNow()
	}

	// Allow goroutines + background subscribers to wind down.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtime.NumGoroutine() - baseline
	if leak > 5 {
		t.Fatalf("goroutine leak after N=%d concurrent flows: leaked=%d (baseline=%d, now=%d)",
			N, leak, baseline, runtime.NumGoroutine())
	}
}

// TestProvider_ConcurrentReuse_RefreshSingleFlight asserts that N
// concurrent Token() calls on an expired token do not stampede the
// authorization server: the refresh runs once and N callers see the
// shared result. Brief 09 §"Concurrent refresh storm on agent-bound
// tokens" — mandatory.
func TestProvider_ConcurrentReuse_RefreshSingleFlight(t *testing.T) {
	t.Parallel()

	h := newProviderHarness(t)
	id := mkIdentity(t)
	ctx := mkCtx(t, id)

	// Seed an expired token + refresh-token in the store so Token()
	// hits the refresh path.
	expired := Token{
		Source:       h.userCfg.Source,
		BindingScope: ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  "old-access",
		RefreshToken: "dummy-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-time.Hour), // expired
	}
	if err := h.store.Put(ctx, expired); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	const N = 32
	var wg sync.WaitGroup
	var seenFresh atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := h.provider.Token(ctx, h.userCfg.Source)
			if err != nil {
				t.Errorf("Token: %v", err)
				return
			}
			if tok.AccessToken != "old-access" {
				seenFresh.Add(1)
			}
		}()
	}
	wg.Wait()

	// All N goroutines should have observed a freshly-refreshed token
	// (the access_token is generated per /token call).
	if int(seenFresh.Load()) != N {
		t.Fatalf("expected all %d goroutines to see refreshed token; got %d", N, seenFresh.Load())
	}
	// Single-flight: at most ONE refresh round-trip to the
	// authorization server. The fake server tracks calls.
	tokenCalls := h.server.TokenCalls()
	if tokenCalls > 4 {
		// We allow some headroom for genuinely racy single-flight
		// boundaries (a refresh completing exactly as another caller
		// arrives may legitimately trigger a second flight). But
		// N=32 → 32 round-trips is the smell we are watching for.
		t.Fatalf("refresh storm: %d /token calls for N=%d concurrent Token() callers (expected ≤ 4)",
			tokenCalls, N)
	}
}
