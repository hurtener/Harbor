package bifrost

// Phase 84d (D-191) — the D-025 concurrent-reuse gate for the
// embeddings driver: N≥100 concurrent Embed calls against ONE shared
// instance under -race, asserting no data races, no context bleed,
// no cross-cancellation, and goroutine-baseline restoration.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	bfschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestConcurrentReuse_Embed_NoRaceNoBleedNoLeak(t *testing.T) {
	d, _ := newStubDriver(func(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
		// Echo the input length into the vector so each caller can
		// assert ITS request produced ITS response (context bleed).
		data := make([]bfschemas.EmbeddingData, len(req.Input.Texts))
		for i, txt := range req.Input.Texts {
			data[i] = bfschemas.EmbeddingData{
				Index:     i,
				Embedding: bfschemas.EmbeddingStruct{EmbeddingArray: []float64{float64(len(txt))}},
			}
		}
		return &bfschemas.BifrostEmbeddingResponse{Data: data}, nil
	})

	baseline := runtime.NumGoroutine()
	const goroutines = 128

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			ctx, err := identity.With(context.Background(), identity.Identity{
				TenantID:  fmt.Sprintf("t-%d", i%7),
				UserID:    fmt.Sprintf("u-%d", i%13),
				SessionID: fmt.Sprintf("s-%d", i),
			})
			if err != nil {
				errCh <- err
				return
			}
			// Per-goroutine distinct payload length: i+1 bytes.
			text := make([]byte, i+1)
			for j := range text {
				text[j] = 'x'
			}
			vecs, err := d.Embed(ctx, []string{string(text)})
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d: %w", i, err)
				return
			}
			if len(vecs) != 1 || float64(vecs[0][0]) != float64(i+1) {
				errCh <- fmt.Errorf("goroutine %d: response bleed — got %v, want leading %d", i, vecs, i+1)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// TestConcurrentReuse_CancellationIsPerCall asserts cancelling run
// A's ctx never affects run B (cross-cancellation).
func TestConcurrentReuse_CancellationIsPerCall(t *testing.T) {
	release := make(chan struct{})
	d, _ := newStubDriver(func(req *bfschemas.BifrostEmbeddingRequest) (*bfschemas.BifrostEmbeddingResponse, *bfschemas.BifrostError) {
		<-release
		return okResponse(req)
	})

	idCtx := func(session string) context.Context {
		ctx, err := identity.With(context.Background(), identity.Identity{
			TenantID: "t", UserID: "u", SessionID: session,
		})
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		return ctx
	}

	ctxA, cancelA := context.WithCancel(idCtx("a"))
	cancelA() // run A is cancelled before the call
	if _, err := d.Embed(ctxA, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run A: err=%v, want context.Canceled", err)
	}

	// Run B proceeds normally on the same shared driver.
	done := make(chan error, 1)
	go func() {
		_, err := d.Embed(idCtx("b"), []string{"b"})
		done <- err
	}()
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run B affected by run A's cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run B did not complete")
	}
}
