package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestCatalog_ConcurrentReuse_D025 pins the concurrent-reuse
// contract for the catalog itself: N concurrent Register +
// Resolve + List calls under -race must not race.
func TestCatalog_ConcurrentReuse_D025(t *testing.T) {
	const concurrentReaders = 100
	const concurrentWriters = 20
	const namesPerWriter = 5

	cat := tools.NewCatalog()
	baseline := runtime.NumGoroutine()

	var wgWrite sync.WaitGroup
	for w := range concurrentWriters {

		wgWrite.Add(1)
		go func() {
			defer wgWrite.Done()
			for i := range namesPerWriter {
				name := fmt.Sprintf("tool-w%d-n%d", w, i)
				err := cat.Register(tools.ToolDescriptor{
					Tool: tools.Tool{Name: name, Loading: tools.LoadingAlways},
					Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
						return tools.ToolResult{Value: name}, nil
					},
				})
				if err != nil {
					t.Errorf("register %q: %v", name, err)
				}
			}
		}()
	}

	var wgRead sync.WaitGroup
	stop := make(chan struct{})
	for range concurrentReaders {
		wgRead.Add(1)
		go func() {
			defer wgRead.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cat.List(tools.CatalogFilter{
					TenantID: "t", UserID: "u", SessionID: "s",
				})
				cat.Resolve("nonexistent")
			}
		}()
	}

	wgWrite.Wait()
	close(stop)
	wgRead.Wait()

	for w := range concurrentWriters {
		for i := range namesPerWriter {
			name := fmt.Sprintf("tool-w%d-n%d", w, i)
			d, ok := cat.Resolve(name)
			if !ok {
				t.Errorf("expected %q present", name)
				continue
			}
			res, err := d.Invoke(context.Background(), nil)
			if err != nil {
				t.Errorf("invoke %q: %v", name, err)
			}
			if res.Value != name {
				t.Errorf("expected value=%q, got %v", name, res.Value)
			}
		}
	}

	list := cat.List(tools.CatalogFilter{})
	if len(list) != concurrentWriters*namesPerWriter {
		t.Errorf("expected %d tools, got %d", concurrentWriters*namesPerWriter, len(list))
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

// TestRunWithPolicy_ConcurrentReuse_D025 pins the contract for the
// policy executor: a shared `policy` value and a shared `invoke`
// closure can run N concurrent invocations under -race without
// state bleed.
func TestRunWithPolicy_ConcurrentReuse_D025(t *testing.T) {
	const n = 100

	var counter atomic.Int64
	invoke := func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		c := counter.Add(1)
		if c%5 == 0 {
			return tools.ToolResult{}, fmt.Errorf("transient: counter=%d", c)
		}
		var got map[string]int
		_ = json.Unmarshal(args, &got)
		return tools.ToolResult{Value: got["i"]}, nil
	}
	policy := tools.ToolPolicy{
		MaxRetries:  5,
		BackoffBase: 1 * time.Millisecond,
		BackoffMult: 2,
		BackoffMax:  5 * time.Millisecond,
		TimeoutMS:   1000,
		RetryOn:     []tools.ErrorClass{tools.ErrClassTransient},
		Validate:    tools.ValidateBoth,
	}

	baseline := runtime.NumGoroutine()
	results := make([]int, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := range n {

		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := fmt.Sprintf("t-%d", i%8)
			id := identity.Identity{TenantID: tenant, UserID: fmt.Sprintf("u-%d", i%8), SessionID: fmt.Sprintf("s-%d", i%8)}
			ctx, _ := identity.With(context.Background(), id)
			args, _ := json.Marshal(map[string]int{"i": i})
			res, err := tools.RunWithPolicy(ctx, args, invoke, nil, nil, policy)
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = res.Value.(int)
		}()
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("invocation %d failed: %v", i, e)
			continue
		}
		if results[i] != i {
			t.Errorf("invocation %d: context bleed; expected %d, got %d", i, i, results[i])
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
