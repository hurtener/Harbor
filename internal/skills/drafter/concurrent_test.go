package drafter

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
)

// concurrent_test.go — the concurrent-reuse contract for the shared
// compiled artifacts of this lane (the Adapter and the narrow writer
// seam): N>=128 concurrent invocations against ONE shared adapter
// across mixed triples under `-race`, with no data race, no context
// bleed, no cancellation cross-talk, no goroutine leak, and no
// cross-scope ref.

func TestCreateDraft_ConcurrentReuse_NoRaceNoBleedNoLeak(t *testing.T) {
	const n = 128
	const cancelled = 8 // a subset whose run ctx is pre-cancelled

	// One shared in-memory store; one shared adapter (the compiled
	// artifact under test); the stub sinks every observed identity so
	// context bleed is asserted with real observations.
	store := newStore(t)
	observed := make(chan identity.Quadruple, n)
	client := stubClient{
		content: validDraftContent(),
		sink:    observed,
	}
	a, err := New(client, Options{})
	if err != nil {
		t.Fatal(err)
	}

	baseline := runtime.NumGoroutine()

	type outcome struct {
		idx    int
		q      identity.Quadruple
		result Result
		err    error
	}
	outcomes := make(chan outcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			q := testQuad(
				fmt.Sprintf("tenant-%d", idx%3),
				fmt.Sprintf("user-%d", idx),
				fmt.Sprintf("session-%d", idx%5),
				fmt.Sprintf("run-%d", idx),
			)
			ctx := runCtx(t, q)
			if idx%(n/cancelled) == 0 && idx < n-1 {
				// A cancelled run must fail loud and write nothing,
				// and must not disturb its siblings.
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			writer, err := NewScopedWriter(store, artifacts.ArtifactScope{
				TenantID:  q.TenantID,
				UserID:    q.UserID,
				SessionID: q.SessionID,
				TaskID:    q.RunID,
			})
			if err != nil {
				outcomes <- outcome{idx: idx, q: q, err: err}
				return
			}
			res, err := CreateDraft(ctx, a, writer, Args{Intent: "intent"})
			outcomes <- outcome{idx: idx, q: q, result: res, err: err}
		}(i)
	}
	wg.Wait()
	close(outcomes)

	// Every identity the client observed must be one of the successful
	// runs' identities, each exactly once — a bleed would show up as a
	// foreign or duplicated identity in the sink.
	expected := make(map[identity.Quadruple]bool, n)
	var successes []outcome
	var cancelFails int
	for o := range outcomes {
		switch {
		case errors.Is(o.err, context.Canceled):
			cancelFails++
			if o.result.ArtifactRef != "" {
				t.Fatalf("cancelled run %d returned a ref: %+v", o.idx, o.result)
			}
		case o.err != nil:
			t.Fatalf("run %d failed unexpectedly: %v", o.idx, o.err)
		default:
			successes = append(successes, o)
			expected[o.q] = true
			if o.result.Installed {
				t.Fatalf("run %d returned installed=true", o.idx)
			}
		}
	}
	if cancelFails != cancelled {
		t.Fatalf("cancelled runs that failed = %d, want %d", cancelFails, cancelled)
	}
	if len(successes) != n-cancelled {
		t.Fatalf("successes = %d, want %d", len(successes), n-cancelled)
	}

	// Drain the identity sink and compare the observed multiset to the
	// expected set. Every successful run's identity must be observed
	// EXACTLY once; any extra or foreign observation is context bleed.
	close(observed)
	seen := make(map[identity.Quadruple]int, len(expected))
	for q := range observed {
		seen[q]++
		if !expected[q] {
			t.Fatalf("client observed an identity that never ran successfully: %+v", q)
		}
	}
	for q := range expected {
		if seen[q] != 1 {
			t.Fatalf("client observed identity %+v %d times, want exactly 1", q, seen[q])
		}
	}

	// No cross-scope ref: every successful draft is readable under its
	// own triple and absent from every OTHER user's triple.
	for _, o := range successes {
		own := artifacts.ArtifactScope{TenantID: o.q.TenantID, UserID: o.q.UserID, SessionID: o.q.SessionID}
		if _, found, err := store.Get(context.Background(), own, o.result.ArtifactRef); err != nil || !found {
			t.Fatalf("run %d draft not readable under its own scope: found=%v err=%v", o.idx, found, err)
		}
		foreign := artifacts.ArtifactScope{TenantID: o.q.TenantID, UserID: "other-user", SessionID: o.q.SessionID}
		if _, found, err := store.Get(context.Background(), foreign, o.result.ArtifactRef); err != nil || found {
			t.Fatalf("run %d draft readable under a foreign user: found=%v err=%v", o.idx, found, err)
		}
	}

	// Goroutine baseline: every invocation's goroutines joined before
	// the invocation returned; nothing leaked after the batch.
	after := runtime.NumGoroutine()
	if after > baseline+2 {
		t.Fatalf("goroutine leak: baseline=%d after=%d", baseline, after)
	}
}
