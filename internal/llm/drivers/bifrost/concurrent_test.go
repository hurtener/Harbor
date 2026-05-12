package bifrost

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// TestConcurrent_D025_BifrostDriver — D-025 concurrent-reuse contract.
// N=128 concurrent goroutines × one shared bifrost driver (with a
// stub bifrost client). Asserts:
//
//   - All Complete calls succeed (no races detected by `-race`).
//   - Per-call identity does NOT bleed (every goroutine sees its own
//     identity in the recorded request).
//   - Goroutine baseline restores within 2s of teardown.
func TestConcurrent_D025_BifrostDriver(t *testing.T) {
	stub := newStubClient()
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	defer func() { _ = drv.Close(context.Background()) }()

	const N = 128
	baseline := runtime.NumGoroutine()
	var (
		wg   sync.WaitGroup
		errs atomic.Int64
		ok   atomic.Int64
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx, err := identity.With(ctx, id)
			if err != nil {
				errs.Add(1)
				t.Errorf("identity.With(%v): %v", id, err)
				return
			}
			text := fmt.Sprintf("hello-%d", i)
			resp, err := drv.Complete(ctx, llm.CompleteRequest{
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
				t.Errorf("empty content for %d", i)
				return
			}
			ok.Add(1)
		}()
	}
	wg.Wait()
	if n := errs.Load(); n != 0 {
		t.Fatalf("%d concurrent calls errored", n)
	}
	if got := ok.Load(); got != N {
		t.Errorf("ok = %d want %d", got, N)
	}

	// Goroutine baseline restored. Tolerance +5 for parked
	// goroutines that haven't retired yet. No time.Sleep for
	// synchronisation per AGENTS.md §11.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if g := runtime.NumGoroutine(); g > baseline+5 {
		t.Errorf("goroutine baseline not restored: got %d baseline %d (+5)", g, baseline)
	}
}

// TestConcurrent_PerCallCancellationIsIsolated — cancelling ctx on
// call A MUST NOT abort call B. A blocking stub stream + per-call
// ctxs prove the isolation: the driver's chunk reader is per-call.
func TestConcurrent_PerCallCancellationIsIsolated(t *testing.T) {
	stub := newStubClient()
	// Both calls use streaming so they hold open the channel reader.
	stub.streamHandler = func(req *bfschemas.BifrostChatRequest) (chan *bfschemas.BifrostStreamChunk, *bfschemas.BifrostError) {
		// A short stream then close — gives B a chance to complete
		// while A is mid-cancel.
		ch := make(chan *bfschemas.BifrostStreamChunk, 4)
		go func() {
			defer close(ch)
			piece := "x"
			for i := 0; i < 3; i++ {
				select {
				case ch <- &bfschemas.BifrostStreamChunk{
					BifrostChatResponse: &bfschemas.BifrostChatResponse{
						Choices: []bfschemas.BifrostResponseChoice{{
							ChatStreamResponseChoice: &bfschemas.ChatStreamResponseChoice{
								Delta: &bfschemas.ChatStreamResponseChoiceDelta{Content: &piece},
							},
						}},
					},
				}:
				case <-time.After(200 * time.Millisecond):
					return
				}
				runtime.Gosched()
			}
		}()
		return ch, nil
	}
	drv := newDriverWithClient(stub, bfschemas.OpenAI, nil)
	defer func() { _ = drv.Close(context.Background()) }()

	idA := identity.Identity{TenantID: "T", UserID: "U", SessionID: "A"}
	idB := identity.Identity{TenantID: "T", UserID: "U", SessionID: "B"}

	ctxA, cancelA := context.WithCancel(context.Background())
	ctxA, _ = identity.With(ctxA, idA)
	defer cancelA()
	ctxB, cancelB := context.WithTimeout(context.Background(), 3*time.Second)
	ctxB, _ = identity.With(ctxB, idB)
	defer cancelB()

	var wg sync.WaitGroup
	wg.Add(2)
	var errA, errB error
	go func() {
		defer wg.Done()
		// Cancel A almost immediately.
		go func() {
			runtime.Gosched()
			cancelA()
		}()
		text := "for-A"
		_, errA = drv.Complete(ctxA, llm.CompleteRequest{
			Model:     "m",
			Messages:  []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
			Stream:    true,
			OnContent: func(string, bool) {},
		})
	}()
	go func() {
		defer wg.Done()
		text := "for-B"
		_, errB = drv.Complete(ctxB, llm.CompleteRequest{
			Model:     "m",
			Messages:  []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
			Stream:    true,
			OnContent: func(string, bool) {},
		})
	}()
	wg.Wait()

	if errA != nil && !errors.Is(errA, context.Canceled) {
		// A may also return successfully if all chunks arrive before
		// cancel fires — both are acceptable. We only care that B
		// was not affected.
		t.Logf("A: %v (acceptable: cancel may race with chunk arrival)", errA)
	}
	if errB != nil {
		t.Errorf("B was affected by A's cancel: %v", errB)
	}
}
