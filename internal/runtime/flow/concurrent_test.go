package flow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestFlow_ConcurrentReuse_NoBudgetBleed pins the D-025 contract.
func TestFlow_ConcurrentReuse_NoBudgetBleed(t *testing.T) {
	const n = 100

	var perFlowCounter atomic.Int64

	tagging := func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		c := perFlowCounter.Add(1)
		out := in
		var got map[string]int
		if bs, ok := in.Payload.(json.RawMessage); ok {
			_ = json.Unmarshal(bs, &got)
		}
		if got == nil {
			got = map[string]int{}
		}
		got["counter"] = int(c)
		raw, _ := json.Marshal(got)
		out.Payload = json.RawMessage(raw)
		return out, nil
	}

	def := flow.Definition{
		Name:  "tagged_flow",
		Entry: "tag",
		Exit:  "tag",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"tag": {Func: tagging},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("tagged_flow")

	baseline := runtime.NumGoroutine()

	type record struct {
		i       int
		counter int
		err     error
	}
	results := make([]record, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i%8)
			id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("u-%d", i%8), SessionID: fmt.Sprintf("s-%d", i%8)}
			ctx, err := identity.With(context.Background(), id)
			if err != nil {
				results[i] = record{i: i, err: err}
				return
			}
			args, _ := json.Marshal(map[string]int{"i": i})
			res, err := d.Invoke(ctx, args)
			if err != nil {
				results[i] = record{i: i, err: err}
				return
			}
			var got map[string]int
			if rb, ok := res.Value.(json.RawMessage); ok {
				_ = json.Unmarshal(rb, &got)
			}
			if got["i"] != i {
				results[i] = record{i: i, err: fmt.Errorf("context bleed: expected i=%d, got %d", i, got["i"])}
				return
			}
			results[i] = record{i: i, counter: got["counter"]}
		}()
	}
	wg.Wait()

	failures := 0
	for _, r := range results {
		if r.err != nil {
			failures++
			t.Logf("invocation %d: %v", r.i, r.err)
		}
	}
	if failures > 0 {
		t.Errorf("%d invocations failed", failures)
	}

	if got := perFlowCounter.Load(); got != int64(n) {
		t.Errorf("expected counter=%d after %d invocations, got %d", n, n, got)
	}

	seen := make(map[int]int)
	for _, r := range results {
		if r.err != nil {
			continue
		}
		seen[r.counter]++
	}
	if len(seen) != n {
		t.Errorf("expected %d distinct counter values, got %d", n, len(seen))
	}
	for k, count := range seen {
		if count > 1 {
			t.Errorf("counter value %d observed %d times (expected once)", k, count)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}

// TestFlow_Concurrent_DeadlineFiresIndependently verifies that
// each invocation gets its own deadline accumulator.
func TestFlow_Concurrent_DeadlineFiresIndependently(t *testing.T) {
	slow := func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		select {
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return in, nil
		}
	}
	def := flow.Definition{
		Name:  "slow_flow",
		Entry: "a",
		Exit:  "a",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: slow},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("slow_flow")

	const concurrent = 20
	var wg sync.WaitGroup
	errs := make([]error, concurrent)
	for i := 0; i < concurrent; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("s-%d", i)}
			ctx, _ := identity.With(context.Background(), id)
			ctx = flow.WithBudget(ctx, flow.Budget{Deadline: 30 * time.Millisecond})
			_, err := d.Invoke(ctx, []byte(`{}`))
			errs[i] = err
		}()
	}
	wg.Wait()

	budgetErrCount := 0
	for _, e := range errs {
		if e == nil {
			t.Errorf("expected error per invocation, got nil")
			continue
		}
		if errors.Is(e, flow.ErrFlowBudgetExceeded) || errors.Is(e, context.DeadlineExceeded) {
			budgetErrCount++
		}
	}
	if budgetErrCount != concurrent {
		t.Errorf("expected %d budget/timeout errors, got %d", concurrent, budgetErrCount)
	}
}
