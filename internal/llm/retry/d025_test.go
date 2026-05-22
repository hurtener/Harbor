package retry_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/retry"
)

// TestWrap_ConcurrentReuse_D025 — N≥100 concurrent Complete calls
// against ONE shared retry wrapper. Each goroutine carries a unique
// identity; the validator forces a retry on the first attempt for
// every other goroutine. Asserts no races, no leak, no identity
// bleed, no cross-cancellation.
func TestWrap_ConcurrentReuse_D025(t *testing.T) {
	t.Parallel()
	const N = 128

	bus := testBus(t)

	// Inner returns a different content per attempt-of-this-call.
	var (
		// Tracks: each request's first attempt vs. retry uniquely.
		// We can't reliably correlate attempts to goroutines without
		// embedding markers; we use the goroutine identity through
		// the message text.
		seenIDs sync.Map
	)
	rec := newRecorder(func(req llm.CompleteRequest, _ int) (llm.CompleteResponse, error) {
		// Echo the tenant from the first user message as the response,
		// so validator can check identity propagation.
		if len(req.Messages) == 0 {
			return llm.CompleteResponse{}, errors.New("empty messages")
		}
		// Look at the FIRST user message text — that's the original.
		var orig string
		for _, m := range req.Messages {
			if m.Role == llm.RoleUser && m.Content.Text != nil {
				orig = *m.Content.Text
				break
			}
		}
		return llm.CompleteResponse{Content: orig}, nil
	})
	cfg := snapshotWithProfile(llm.ModelProfile{ContextWindowTokens: 1000, MaxRetries: 2})
	client := retry.Wrap(rec, cfg, llm.Deps{Bus: bus})

	baseline := goroutineBaseline()

	var wg sync.WaitGroup
	wg.Add(N)
	var (
		passCount atomic.Int64
		failCount atomic.Int64
	)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			tenantID := fmt.Sprintf("t-%d", i)
			id := identity.Identity{TenantID: tenantID, UserID: "u", SessionID: "s"}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("r-%d", i))
			if err != nil {
				t.Errorf("identity.WithRun: %v", err)
				return
			}
			body := fmt.Sprintf("goroutine-%d-tenant-%s", i, tenantID)
			// Every other goroutine forces ONE retry by asking the
			// validator to reject the first response.
			var attempt atomic.Int64
			validator := func(r llm.CompleteResponse) error {
				a := attempt.Add(1)
				// Must see our own body — context bleed detector.
				if r.Content != body {
					return fmt.Errorf("identity bleed: got %q want %q", r.Content, body)
				}
				if i%2 == 0 && a == 1 {
					return errors.New("reject-first")
				}
				return nil
			}
			req := sampleRequest(validator)
			req.Messages[0].Content.Text = &body
			_, err = client.Complete(ctx, req)
			if err == nil {
				passCount.Add(1)
			} else {
				failCount.Add(1)
			}
			seenIDs.Store(tenantID, struct{}{})
		}(i)
	}
	wg.Wait()

	// Count unique tenants.
	count := 0
	seenIDs.Range(func(_, _ any) bool { count++; return true })
	if count != N {
		t.Errorf("seen %d unique tenants, want %d", count, N)
	}
	// All goroutines should have either passed or retried successfully.
	if failCount.Load() != 0 {
		t.Errorf("%d goroutines failed; expected 0 (all should succeed within MaxRetries)",
			failCount.Load())
	}

	current := goroutineBaseline()
	if current > baseline+5 {
		t.Errorf("goroutine count grew: baseline=%d current=%d", baseline, current)
	}
}
