package llm_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/mock"
)

// TestConcurrent_D025 — D-025 concurrent-reuse contract for LLMClient.
//
// N=128 concurrent goroutines × one shared LLMClient. Each goroutine
// constructs its own ctx with a per-goroutine identity, fires Complete,
// asserts:
//
//   - The driver observed THIS goroutine's identity (via SeenIdentity
//     channel — captures one-at-a-time but the test correlates by
//     count, not by per-call match).
//   - No error from Complete (the safety pass clears, the mock
//     synthesises a response).
//   - Per-call ctx cancellation does NOT affect other goroutines
//     (each goroutine cancels its own ctx after returning).
//
// After all goroutines complete, the runtime.NumGoroutine() must
// return to the baseline (within a small tolerance) — no leak.
func TestConcurrent_D025_LLMClient(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()

	// We need direct access to the mock's SeenIdentity sink — wire
	// a custom driver and register a one-shot factory. The
	// production "mock" driver registered via init() doesn't have
	// this hook by design (operators do not configure SeenIdentity).
	const factoryName = "mock-concurrent-test"
	seen := make(chan identity.Quadruple, 128*2)
	llm.Register(factoryName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return mock.New(mock.Options{SeenIdentity: seen}), nil
	})

	snap := makeSnapshot("m", 1_000_000)
	snap.Driver = factoryName
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	const N = 128
	baseline := runtime.NumGoroutine()
	var (
		wg     sync.WaitGroup
		errs   atomic.Int64
		issued atomic.Int64
	)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i),
				UserID:    fmt.Sprintf("u-%d", i),
				SessionID: fmt.Sprintf("s-%d", i),
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx, err := identity.With(ctx, id)
			if err != nil {
				errs.Add(1)
				t.Errorf("identity.With(%v): %v", id, err)
				return
			}
			text := fmt.Sprintf("hello-%d", i)
			resp, err := client.Complete(ctx, llm.CompleteRequest{
				Model:    "m",
				Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
			})
			if err != nil {
				errs.Add(1)
				t.Errorf("Complete[%d]: %v", i, err)
				return
			}
			if resp.Content == "" {
				errs.Add(1)
				t.Errorf("resp.Content[%d] empty", i)
				return
			}
			issued.Add(1)
		}()
	}
	wg.Wait()
	if n := errs.Load(); n > 0 {
		t.Fatalf("%d concurrent Complete calls errored", n)
	}
	if got := issued.Load(); got != N {
		t.Errorf("issued=%d, want %d", got, N)
	}

	// Drain SeenIdentity — confirms every Complete reached the
	// driver and the driver observed an identity per call.
	identitiesSeen := 0
drain:
	for {
		select {
		case <-seen:
			identitiesSeen++
			if identitiesSeen >= N {
				break drain
			}
		case <-time.After(500 * time.Millisecond):
			break drain
		}
	}
	if identitiesSeen != N {
		t.Errorf("driver observed %d identities, want %d", identitiesSeen, N)
	}

	// Goroutine baseline restored. Tolerance +5 for parked
	// goroutines that haven't retired yet. No time.Sleep for
	// synchronisation per AGENTS.md §11.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if g := runtime.NumGoroutine(); g > baseline+5 {
		t.Errorf("goroutine baseline not restored: got %d, baseline %d (+5 tolerance)", g, baseline)
	}
}

// TestConcurrent_PerCallCancellationIsIsolated — cancelling ctx on
// goroutine A MUST NOT abort Complete on goroutine B. The mock's
// PreStreamDelay creates an observable window where A's cancel
// arrives mid-stream; B's Complete (without delay) should still
// succeed.
func TestConcurrent_PerCallCancellationIsIsolated(t *testing.T) {
	deps, cleanup := makeDeps(t)
	defer cleanup()

	const factoryName = "mock-cancel-test"
	llm.Register(factoryName, func(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error) {
		return mock.New(mock.Options{
			StreamChunks:   8,
			PreStreamDelay: 50 * time.Millisecond,
		}), nil
	})
	snap := makeSnapshot("m", 1_000_000)
	snap.Driver = factoryName
	client, err := llm.Open(context.Background(), snap, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	textA := "this-A-will-cancel"
	textB := "this-B-completes"

	idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "A"}
	idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "B"}

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxA, _ = identity.With(ctxA, idA)
	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()
	ctxB, _ = identity.With(ctxB, idB)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var cancelOnce sync.Once
		_, _ = client.Complete(ctxA, llm.CompleteRequest{
			Model:    "m",
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &textA}}},
			Stream:   true,
			// First observed chunk triggers cancel — synchronous on
			// the streaming path, so cancel arrives mid-stream
			// deterministically. AGENTS.md §11: no time.Sleep for
			// synchronisation.
			OnContent: func(_ string, _ bool) {
				cancelOnce.Do(cancelA)
			},
		})
		// We don't assert on A's err — both Canceled and a successful
		// short-completion are acceptable; what matters is B is
		// unaffected.
	}()
	go func() {
		defer wg.Done()
		_, err := client.Complete(ctxB, llm.CompleteRequest{
			Model:    "m",
			Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &textB}}},
		})
		if err != nil {
			t.Errorf("Complete(B): %v (B should not be affected by A's cancel)", err)
		}
	}()
	wg.Wait()
}
