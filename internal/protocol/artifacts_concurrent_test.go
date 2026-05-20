package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestArtifactsHandler_ConcurrentReuse_NoCrossTalk pins the D-025
// concurrent-reuse contract for the ArtifactsSurface: N=100 concurrent
// `artifacts.list` calls against a SINGLE shared surface + shared
// ArtifactStore, each goroutine using a distinct identity tenant. The
// test asserts:
//
//   - No data race (`-race` clean — the gate).
//   - No cross-tenant bleed — every returned row carries only the
//     caller's tenant.
//   - No cancellation cross-talk — cancelling one goroutine's ctx does
//     not affect any other's result.
//   - No goroutine leak — the baseline runtime.NumGoroutine() count is
//     restored after every goroutine returns.
func TestArtifactsHandler_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const n = 100

	store := newInMemStore(t)
	surface := newArtifactsSurface(t, store, "inmem")

	// Pre-seed one artifact per tenant so each goroutine has a row to
	// find — proves the list is genuinely per-tenant.
	for i := 0; i < n; i++ {
		tenant := fmt.Sprintf("tenant-%03d", i)
		scope := types.ArtifactScope{Tenant: tenant, User: "u1", Session: "s1"}
		putFixture(t, surface, scope, []byte(fmt.Sprintf("payload for %s", tenant)),
			types.ArtifactsPutOpts{MimeType: "text/plain"})
	}

	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%03d", idx)
			ctx := context.Background()
			// Half the goroutines cancel their ctx immediately after the
			// call returns — proves cancellation does not cross-talk.
			if idx%2 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
			}
			resp, err := surface.Dispatch(ctx, methods.MethodArtifactsList, &types.ArtifactsListRequest{
				Scope: types.ArtifactScope{Tenant: tenant, User: "u1", Session: "s1"},
			})
			if err != nil {
				errs[idx] = fmt.Errorf("goroutine %d: dispatch: %w", idx, err)
				return
			}
			lr, ok := resp.(*types.ArtifactsListResponse)
			if !ok {
				errs[idx] = fmt.Errorf("goroutine %d: response type %T", idx, resp)
				return
			}
			// Cross-tenant bleed check: every row MUST carry this
			// goroutine's tenant.
			for _, row := range lr.Rows {
				if row.Ref.Scope.Tenant != tenant {
					errs[idx] = fmt.Errorf("goroutine %d: row tenant %q != caller %q (cross-tenant bleed)",
						idx, row.Ref.Scope.Tenant, tenant)
					return
				}
			}
			if len(lr.Rows) != 1 {
				errs[idx] = fmt.Errorf("goroutine %d: got %d rows, want exactly 1", idx, len(lr.Rows))
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	// Goroutine-leak check: allow a brief settle, then assert the count
	// returned to (near) baseline. A small slack absorbs the test
	// runtime's own scheduler goroutines.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d goroutines above baseline %d after all dispatches returned", leaked, baseline)
	}
}

// TestArtifactsHandler_ConcurrentPutGet_NoCrossTalk additionally
// exercises concurrent put + get_ref against a single shared surface
// (stub presigner) — proving the upload + resolve paths are also
// D-025-safe under contention.
func TestArtifactsHandler_ConcurrentPutGet_NoCrossTalk(t *testing.T) {
	const n = 100

	store := stubPresigner{ArtifactStore: newInMemStore(t)}
	surface := newArtifactsSurface(t, store, "s3-stub")

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			tenant := fmt.Sprintf("tenant-%03d", idx)
			scope := types.ArtifactScope{Tenant: tenant, User: "u1", Session: "s1"}
			ctx := context.Background()

			putResp, err := surface.Dispatch(ctx, methods.MethodArtifactsPut, &types.ArtifactsPutRequest{
				Scope: scope,
				Bytes: []byte(fmt.Sprintf("payload-%03d", idx)),
				Opts:  types.ArtifactsPutOpts{MimeType: "text/plain"},
			})
			if err != nil {
				errs[idx] = fmt.Errorf("goroutine %d: put: %w", idx, err)
				return
			}
			ref := putResp.(*types.ArtifactsPutResponse).Ref

			getResp, err := surface.Dispatch(ctx, methods.MethodArtifactsGetRef, &types.ArtifactsGetRefRequest{
				Scope: scope, ID: ref.ID, Expiry: 5 * time.Minute,
			})
			if err != nil {
				errs[idx] = fmt.Errorf("goroutine %d: get_ref: %w", idx, err)
				return
			}
			gr := getResp.(*types.ArtifactsGetRefResponse)
			if gr.Ref.Scope.Tenant != tenant {
				errs[idx] = fmt.Errorf("goroutine %d: get_ref tenant %q != caller %q", idx, gr.Ref.Scope.Tenant, tenant)
			}
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}
