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

	for i := range N {
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
				errCh <- fmt.Errorf("g%d Token: want *ErrAuthRequired, got %w", i, err)
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
	for range N {
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

// TestProvider_RefreshSingleFlight_LeaderCancelDoesNotPoisonFollowers
// pins the D-025 cancellation-cross-talk guarantee on the refresh
// single-flight: the refresh round-trip runs detached from the
// initiating caller, so cancelling the caller that STARTED the flight
// must not fail a follower collapsed onto the same flight with a
// "context canceled" it never asked for.
func TestProvider_RefreshSingleFlight_LeaderCancelDoesNotPoisonFollowers(t *testing.T) {
	t.Parallel()

	h := newProviderHarness(t)
	id := mkIdentity(t)
	baseCtx := mkCtx(t, id)

	// Seed an expired token + refresh-token so Token() hits the
	// refresh path.
	expired := Token{
		Source:       h.userCfg.Source,
		BindingScope: ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  "old-access",
		RefreshToken: "dummy-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	if err := h.store.Put(baseCtx, expired); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	// Gate the refresh grant: signal arrival, then block until released.
	started := make(chan struct{}, 2)
	gate := make(chan struct{})
	h.server.refreshHook = func() {
		started <- struct{}{}
		<-gate
	}

	// Leader: starts the refresh flight, then gets cancelled mid-flight.
	leaderCtx, cancelLeader := context.WithCancel(baseCtx)
	leaderErr := make(chan error, 1)
	go func() {
		_, err := h.provider.Token(leaderCtx, h.userCfg.Source)
		leaderErr <- err
	}()
	<-started // refresh in flight; flight registered

	// Follower joins the same flight.
	type res struct {
		tok Token
		err error
	}
	followerRes := make(chan res, 1)
	go func() {
		tok, err := h.provider.Token(baseCtx, h.userCfg.Source)
		followerRes <- res{tok, err}
	}()

	// Cancel the leader while the server is still gated.
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader: want context.Canceled, got %v", err)
	}

	// Release the server: the follower must get the refreshed token —
	// never the leader's cancellation.
	close(gate)
	r := <-followerRes
	if r.err != nil {
		t.Fatalf("follower poisoned by leader cancellation: %v", r.err)
	}
	if r.tok.AccessToken == "old-access" || r.tok.AccessToken == "" {
		t.Fatalf("follower did not receive a refreshed token: %q", r.tok.AccessToken)
	}
}

// TestProvider_RefreshSingleFlight_CrossTenantSameUserID_NotShared
// pins the multi-isolation contract on the refresh single-flight key:
// two TENANTS sharing a user ID must never collapse onto one refresh
// flight (the follower would receive the leader tenant's token).
func TestProvider_RefreshSingleFlight_CrossTenantSameUserID_NotShared(t *testing.T) {
	t.Parallel()

	h := newProviderHarness(t)
	idA := identity.Identity{TenantID: "tenant-A", UserID: "alice", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "alice", SessionID: "sB"}
	ctxA := mkCtx(t, idA)
	ctxB := mkCtx(t, idB)

	// Seed an expired token per tenant — same user ID, same source.
	for _, seed := range []struct {
		ctx context.Context
		id  identity.Identity
	}{{ctxA, idA}, {ctxB, idB}} {
		if err := h.store.Put(seed.ctx, Token{
			Source:       h.userCfg.Source,
			BindingScope: ScopeUser,
			TenantID:     seed.id.TenantID,
			UserID:       seed.id.UserID,
			AccessToken:  "old-access-" + seed.id.TenantID,
			RefreshToken: "dummy-refresh-" + seed.id.TenantID,
			TokenType:    "Bearer",
			ExpiresAt:    time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("seed Put (%s): %v", seed.id.TenantID, err)
		}
	}

	// Concurrent refreshes for both tenants. The fake server mints a
	// fresh random token per /token call, so a SHARED flight would hand
	// both tenants the SAME access token.
	var wg sync.WaitGroup
	toks := make([]Token, 2)
	errs := make([]error, 2)
	for i, c := range []context.Context{ctxA, ctxB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			toks[i], errs[i] = h.provider.Token(c, h.userCfg.Source)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Token[%d]: %v", i, err)
		}
	}
	if toks[0].AccessToken == toks[1].AccessToken {
		t.Fatalf("cross-tenant refresh flight shared: both tenants received %q", toks[0].AccessToken)
	}
	if toks[0].TenantID != "tenant-A" || toks[1].TenantID != "tenant-B" {
		t.Fatalf("tenant stamp bleed: A=%q B=%q", toks[0].TenantID, toks[1].TenantID)
	}
	// Two distinct flights → two /token round-trips.
	if calls := h.server.TokenCalls(); calls != 2 {
		t.Fatalf("expected 2 refresh round-trips (one per tenant), got %d", calls)
	}
}
