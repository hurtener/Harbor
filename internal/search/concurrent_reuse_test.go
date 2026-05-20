package search_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

// TestSearchers_ConcurrentReuse_AllFourIndexes_NoCrossTalk is the
// D-025 witness for the entire search subsystem: ONE shared registry,
// ONE shared aggregate dispatcher, N≥100 concurrent invocations with
// distinct per-goroutine identity quadruples, under -race.
//
// Asserts:
//   - No data races (the race detector is the gate).
//   - No context bleed (every result row carries the requesting
//     goroutine's tenant, never a peer's).
//   - No cross-cancellation (cancelling goroutine A's ctx never
//     short-circuits goroutine B's result).
//   - Baseline goroutine count restored after all calls return.
func TestSearchers_ConcurrentReuse_AllFourIndexes_NoCrossTalk(t *testing.T) {
	const N = 128

	// Use the echoTenantSearcher from aggregate_test.go — already
	// asserts ctx-derived tenant identity comes back unchanged.
	sessionsS := &echoTenantSearcher{idx: types.SearchIndexSessions}
	tasksS := &echoTenantSearcher{idx: types.SearchIndexTasks}
	eventsS := &echoTenantSearcher{idx: types.SearchIndexEvents}
	artifactsS := &echoTenantSearcher{idx: types.SearchIndexArtifacts}

	reg, err := search.NewRegistry(sessionsS, tasksS, eventsS, artifactsS)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N*5)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ident := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    "u",
				SessionID: "s",
			}
			ctx, _ := identity.With(context.Background(), ident)
			// Mix of methods — `search.query` aggregates across all
			// four, the per-index Searchers go directly.
			switch i % 5 {
			case 0:
				resp, qerr := search.Query(ctx, reg, ident, func(context.Context) bool { return true }, types.SearchRequest{})
				if qerr != nil {
					failures <- fmt.Sprintf("g%d/Query: %v", i, qerr)
					return
				}
				for _, r := range resp.Rows {
					if r.TenantID != ident.TenantID {
						failures <- fmt.Sprintf("g%d/Query: LEAK %s vs %s", i, r.TenantID, ident.TenantID)
					}
				}
			default:
				idxs := []types.SearchIndex{
					types.SearchIndexSessions,
					types.SearchIndexTasks,
					types.SearchIndexEvents,
					types.SearchIndexArtifacts,
				}
				s, _ := reg.Get(idxs[i%4])
				resp, qerr := s.Search(ctx, types.SearchRequest{})
				if qerr != nil {
					failures <- fmt.Sprintf("g%d/%s: %v", i, s.Index(), qerr)
					return
				}
				for _, r := range resp.Rows {
					if r.TenantID != ident.TenantID {
						failures <- fmt.Sprintf("g%d/%s: LEAK %s vs %s", i, s.Index(), r.TenantID, ident.TenantID)
					}
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	var msgs []string
	for f := range failures {
		msgs = append(msgs, f)
	}
	if len(msgs) > 0 {
		t.Fatalf("concurrent-reuse failures (%d):\n  %v", len(msgs), msgs)
	}

	// Allow the Query dispatcher's per-index timeout goroutines to
	// wind down before snapshotting.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

// TestSearchers_CrossCancellation_DoesNotAffectPeers — D-025: a
// cancelled ctx in goroutine A must not affect goroutine B's result.
func TestSearchers_CrossCancellation_DoesNotAffectPeers(t *testing.T) {
	t.Parallel()
	s := &echoTenantSearcher{idx: types.SearchIndexSessions}
	reg, _ := search.NewRegistry(s)

	idA := identity.Identity{TenantID: "ta", UserID: "u", SessionID: "s"}
	idB := identity.Identity{TenantID: "tb", UserID: "u", SessionID: "s"}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxA, _ = identity.With(ctxA, idA)
	ctxB, _ := identity.With(context.Background(), idB)

	var wg sync.WaitGroup
	var errA, errB error
	var respB types.SearchResponse

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, errA = search.Query(ctxA, reg, idA, func(context.Context) bool { return true }, types.SearchRequest{})
	}()
	cancelA()

	wg.Add(1)
	go func() {
		defer wg.Done()
		respB, errB = search.Query(ctxB, reg, idB, func(context.Context) bool { return true }, types.SearchRequest{})
	}()

	wg.Wait()

	// goroutine A may or may not have observed the cancellation —
	// that's fine. The contract is that B is unaffected.
	if errB != nil {
		t.Fatalf("goroutine B: %v", errB)
	}
	for _, r := range respB.Rows {
		if r.TenantID != idB.TenantID {
			t.Errorf("LEAK: B got tenant %s, want %s", r.TenantID, idB.TenantID)
		}
	}
	_ = errA // silence unused
}
