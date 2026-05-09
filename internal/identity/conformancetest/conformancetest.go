// Package conformancetest exposes the canonical identity-correctness
// suite that every identity-aware Harbor subsystem (StateStore drivers,
// MemoryStore drivers, Governance, Audit, Memory) must run.
//
// It lives in a subpackage so that the production-code path
// `internal/identity` does not import the standard library `testing`
// package. Downstream tests import it as:
//
//	import "github.com/hurtener/Harbor/internal/identity/conformancetest"
//
//	func TestMyDriver_IdentityConformance(t *testing.T) {
//	    conformancetest.Run(t, func() context.Context { return context.Background() })
//	}
//
// The factory must return a fresh context.Background() (or the caller's
// equivalent root) per call; the suite injects identities and asserts
// isolation behavior.
package conformancetest

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

// Run executes the canonical identity-correctness suite against the
// context produced by factory. The suite proves that the identity
// invariants (D-001 fail-closed validation, ctx round-trip, key
// independence, D-025 race-free concurrent reuse) hold in the caller's
// environment.
func Run(t *testing.T, factory func() context.Context) {
	t.Helper()

	t.Run("Validate_FailsClosed", func(t *testing.T) {
		cases := []struct {
			name string
			id   identity.Identity
			want bool
		}{
			{"all-empty", identity.Identity{}, false},
			{"empty-tenant", identity.Identity{UserID: "u", SessionID: "s"}, false},
			{"empty-user", identity.Identity{TenantID: "t", SessionID: "s"}, false},
			{"empty-session", identity.Identity{TenantID: "t", UserID: "u"}, false},
			{"populated", identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, true},
		}
		for _, tc := range cases {
			err := identity.Validate(tc.id)
			if tc.want && err != nil {
				t.Errorf("%s: Validate returned %v, want nil", tc.name, err)
			}
			if !tc.want {
				if err == nil {
					t.Errorf("%s: Validate returned nil, want ErrIdentityIncomplete", tc.name)
				} else if !errors.Is(err, identity.ErrIdentityIncomplete) {
					t.Errorf("%s: Validate err=%v, not ErrIdentityIncomplete", tc.name, err)
				}
			}
		}
	})

	t.Run("With_RoundTrip", func(t *testing.T) {
		want := identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-x"}
		ctx, err := identity.With(factory(), want)
		if err != nil {
			t.Fatalf("With returned %v, want nil", err)
		}
		got, ok := identity.From(ctx)
		if !ok {
			t.Fatalf("From after With returned ok=false")
		}
		if got != want {
			t.Errorf("From returned %+v, want %+v", got, want)
		}
	})

	t.Run("WithRun_RoundTrip", func(t *testing.T) {
		id := identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-x"}
		ctx, err := identity.WithRun(factory(), id, "run-42")
		if err != nil {
			t.Fatalf("WithRun returned %v, want nil", err)
		}
		got, ok := identity.QuadrupleFrom(ctx)
		if !ok {
			t.Fatalf("QuadrupleFrom after WithRun returned ok=false")
		}
		want := identity.Quadruple{Identity: id, RunID: "run-42"}
		if got != want {
			t.Errorf("QuadrupleFrom returned %+v, want %+v", got, want)
		}
	})

	t.Run("Identity_Quadruple_NonAliasing", func(t *testing.T) {
		id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
		ctxIdentityOnly, err := identity.With(factory(), id)
		if err != nil {
			t.Fatalf("With: %v", err)
		}
		if _, ok := identity.QuadrupleFrom(ctxIdentityOnly); ok {
			t.Errorf("With-derived ctx satisfied QuadrupleFrom; keys are not independent")
		}
		ctxRunOnly, err := identity.WithRun(factory(), id, "run-1")
		if err != nil {
			t.Fatalf("WithRun: %v", err)
		}
		if _, ok := identity.From(ctxRunOnly); ok {
			t.Errorf("WithRun-derived ctx satisfied From; keys are not independent")
		}
	})

	t.Run("Concurrent_DerivedCtx_Isolation", func(t *testing.T) {
		const goroutines = 1024
		baseline := runtime.NumGoroutine()

		var wg sync.WaitGroup
		var mismatches atomic.Int64
		root := factory()
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			i := i
			go func() {
				defer wg.Done()
				want := identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", i%17),
					UserID:    fmt.Sprintf("u-%d", i%41),
					SessionID: fmt.Sprintf("s-%d", i),
				}
				ctx, err := identity.With(root, want)
				if err != nil {
					mismatches.Add(1)
					return
				}
				got, ok := identity.From(ctx)
				if !ok || got != want {
					mismatches.Add(1)
				}
			}()
		}
		wg.Wait()

		if n := mismatches.Load(); n != 0 {
			t.Errorf("%d goroutines observed cross-talk", n)
		}

		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
		}
	})
}
