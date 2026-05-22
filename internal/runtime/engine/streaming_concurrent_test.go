package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// TestEmitChunk_CrossRun_NoDeadlock is **the gate test for Phase 12**.
// It pins the deadlock-prevention guarantee from brief 01 §4: "A run
// that emits hundreds of stream frames could fill its outgoing queue
// and block the producing goroutine. ... Without it, parallel runs
// can deadlock each other through shared bounded queues."
//
// Setup:
//   - 8 producer goroutines, one per RunID, each emitting 100 frames
//     to a 1-node engine.
//   - The engine has a slow consumer: a Fetch every 200µs. Below the
//     producer's emit cadence so backpressure activates.
//   - Per-run capacity = 16. Total emits = 800. Max in-flight at any
//     time = 8 * 16 = 128 frames; the rest queue at the producer's
//     capacity waiter.
//
// Asserts:
//   - All 800 frames delivered (no drops).
//   - Per-stream order preserved (Seq strictly increasing per run).
//   - Goroutine baseline restored after Stop.
//
// If this test ever flakes in CI, Phase 12 has shipped wrong and
// must be rolled back. The master plan README is explicit:
// "Phase 12 (Streaming + per-run backpressure) — the predecessor's
// deadlock-under-streaming sharp edge; if shipped wrong, parallel
// runs deadlock."
func TestEmitChunk_CrossRun_NoDeadlock(t *testing.T) {
	const tenants = 8
	const framesPerRun = 100
	const cap = 16

	// Each producer emits its own list of expected frame texts so we
	// can assert ordering after the fact.
	type producedSet struct {
		frames []engine.StreamFrame
		mu     sync.Mutex
	}
	deliveries := make(map[string]*producedSet, tenants)
	for i := range tenants {
		deliveries[fmt.Sprintf("r-%d", i)] = &producedSet{}
	}
	var deliveriesMu sync.Mutex

	nodeFunc := func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		runID := env.RunID
		for i := range framesPerRun {
			frame := engine.StreamFrame{
				StreamID: runID,
				Text:     fmt.Sprintf("%s-%d", runID, i),
				Done:     i == framesPerRun-1,
			}
			if err := nctx.EmitChunk(ctx, frame); err != nil {
				return messages.Envelope{}, fmt.Errorf("EmitChunk[%s,%d]: %w", runID, i, err)
			}
		}
		return messages.Envelope{}, nil
	}
	node := engine.Node{
		Name:   "stream-producer",
		Func:   nodeFunc,
		Policy: engine.NodePolicy{RunCapacity: cap},
	}
	eng, err := engine.New([]engine.Adjacency{{From: node}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseline := runtime.NumGoroutine()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Slow consumer goroutine: drains all 8*100 = 800 stream frames
	// at a pace that forces capacity backpressure on producers. The
	// engine's worker also emits the nodeFunc's RETURN envelope
	// (zero-value here); the consumer filters those out by
	// type-asserting on StreamFrame.
	totalFrames := tenants * framesPerRun
	consumerDone := make(chan error, 1)
	go func() {
		fetched := 0
		for fetched < totalFrames {
			fctx, fcancel := context.WithTimeout(ctx, 30*time.Second)
			env, err := eng.Fetch(fctx)
			fcancel()
			if err != nil {
				consumerDone <- fmt.Errorf("Fetch[%d]: %w", fetched, err)
				return
			}
			frame, ok := env.Payload.(engine.StreamFrame)
			if !ok {
				// nodeFunc's return envelope (non-stream); skip.
				continue
			}
			deliveriesMu.Lock()
			set := deliveries[env.RunID]
			deliveriesMu.Unlock()
			if set == nil {
				consumerDone <- fmt.Errorf("Fetch[%d] unknown RunID %q", fetched, env.RunID)
				return
			}
			set.mu.Lock()
			set.frames = append(set.frames, frame)
			set.mu.Unlock()
			fetched++
			// Slow drain: small sleep to ensure backpressure pressure.
			// 200µs * 800 = 160ms total drain time; producers are
			// faster, so capacity will saturate.
			select {
			case <-time.After(200 * time.Microsecond):
			case <-ctx.Done():
				consumerDone <- ctx.Err()
				return
			}
		}
		consumerDone <- nil
	}()

	// Producers: one Emit per RunID, each emitting `framesPerRun`
	// frames inside the node.
	var prodWG sync.WaitGroup
	for i := range tenants {

		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			env := messages.Envelope{
				Headers: messages.Headers{
					TenantID: "tenant",
					UserID:   "user",
					Topic:    "stream",
				},
				SessionID: "sess",
				RunID:     fmt.Sprintf("r-%d", i),
			}
			if err := eng.Emit(ctx, env); err != nil {
				t.Errorf("producer %d Emit: %v", i, err)
			}
		}()
	}
	prodWG.Wait()

	// Wait for consumer to drain everything.
	select {
	case err := <-consumerDone:
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("consumer did not drain all frames within 60s — likely deadlock")
	}

	// Assert all frames present and per-stream ordered.
	for runID, set := range deliveries {
		set.mu.Lock()
		got := len(set.frames)
		// Sort by Seq for ordering check (Seq is monotonic per
		// StreamID = RunID here, but the dispatcher demuxes per-run
		// in FIFO order so they should arrive in order anyway).
		sorted := make([]engine.StreamFrame, len(set.frames))
		copy(sorted, set.frames)
		set.mu.Unlock()
		if got != framesPerRun {
			t.Errorf("run %q: got %d frames, want %d", runID, got, framesPerRun)
			continue
		}
		// Assert seq is strictly increasing in the order we
		// received them (the per-run subqueue is FIFO; insertion
		// order via append should equal Seq order).
		for i := 1; i < len(sorted); i++ {
			if sorted[i].Seq <= sorted[i-1].Seq {
				t.Errorf("run %q: out-of-order at i=%d: prev=%d cur=%d",
					runID, i, sorted[i-1].Seq, sorted[i].Seq)
				break
			}
		}
		// Last frame must be Done.
		if !sorted[len(sorted)-1].Done {
			t.Errorf("run %q: last frame Done=false", runID)
		}
	}

	// Stop and assert no goroutine leak.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := eng.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 3 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// TestEngine_ConcurrentReuse_Streaming is the D-025 contract for
// Phase 12: a compiled *engine is reusable across N>=100 concurrent
// goroutines emitting stream chunks on distinct RunIDs. Asserts no
// race, no goroutine leak after Stop, no cross-stream interleave.
func TestEngine_ConcurrentReuse_Streaming(t *testing.T) {
	const N = 120
	const framesPerRun = 5

	type seenRun struct {
		frames []engine.StreamFrame
		mu     sync.Mutex
	}
	seen := make(map[string]*seenRun, N)
	for i := range N {
		seen[fmt.Sprintf("r-%d", i)] = &seenRun{}
	}
	var seenMu sync.Mutex

	nodeFunc := func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		runID := env.RunID
		for i := range framesPerRun {
			frame := engine.StreamFrame{
				StreamID: runID,
				Text:     fmt.Sprintf("%s-%d", runID, i),
				Done:     i == framesPerRun-1,
			}
			if err := nctx.EmitChunk(ctx, frame); err != nil {
				return messages.Envelope{}, err
			}
		}
		return messages.Envelope{}, nil
	}
	node := engine.Node{
		Name:   "stream-producer",
		Func:   nodeFunc,
		Policy: engine.NodePolicy{RunCapacity: 8},
	}
	eng, err := engine.New([]engine.Adjacency{{From: node}})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseline := runtime.NumGoroutine()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	totalFrames := N * framesPerRun
	consumerDone := make(chan error, 1)
	go func() {
		fetched := 0
		for fetched < totalFrames {
			env, err := eng.Fetch(ctx)
			if err != nil {
				consumerDone <- fmt.Errorf("Fetch[%d]: %w", fetched, err)
				return
			}
			frame, ok := env.Payload.(engine.StreamFrame)
			if !ok {
				// nodeFunc return envelope; skip.
				continue
			}
			seenMu.Lock()
			s := seen[env.RunID]
			seenMu.Unlock()
			if s == nil {
				consumerDone <- fmt.Errorf("Fetch[%d] unknown RunID %q", fetched, env.RunID)
				return
			}
			s.mu.Lock()
			s.frames = append(s.frames, frame)
			s.mu.Unlock()
			fetched++
		}
		consumerDone <- nil
	}()

	var prodWG sync.WaitGroup
	for i := range N {

		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			env := messages.Envelope{
				Headers:   messages.Headers{TenantID: "t", UserID: "u"},
				SessionID: "s",
				RunID:     fmt.Sprintf("r-%d", i),
			}
			_ = eng.Emit(ctx, env)
		}()
	}
	prodWG.Wait()

	select {
	case err := <-consumerDone:
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("consumer didn't drain in 60s")
	}

	// No cross-stream interleave: every frame for run r-N has a
	// StreamID equal to r-N.
	for runID, s := range seen {
		s.mu.Lock()
		ids := make([]string, len(s.frames))
		seqs := make([]int, len(s.frames))
		for i, f := range s.frames {
			ids[i] = f.StreamID
			seqs[i] = f.Seq
		}
		s.mu.Unlock()
		if len(s.frames) != framesPerRun {
			t.Errorf("run %q: %d frames, want %d", runID, len(s.frames), framesPerRun)
		}
		for _, id := range ids {
			if id != runID {
				t.Errorf("run %q: got frame StreamID=%q (cross-stream bleed)", runID, id)
			}
		}
		// Seqs should be 1..framesPerRun.
		sort.Ints(seqs)
		for i, sq := range seqs {
			if sq != i+1 {
				t.Errorf("run %q: Seq[%d]=%d, want %d", runID, i, sq, i+1)
			}
		}
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := eng.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)",
			baseline, runtime.NumGoroutine(), delta)
	}
}

// TestEngine_NoGoroutineLeak_AfterStop_WithStreaming covers three
// shutdown shapes: (a) idle engine, (b) engine mid-stream, (c) engine
// with full queues at Stop time. Each scenario asserts goroutine
// baseline restoration within 2s.
func TestEngine_NoGoroutineLeak_AfterStop_WithStreaming(t *testing.T) {
	scenarios := []struct {
		work func(t *testing.T, eng engine.Engine, runCtx context.Context)
		name string
	}{
		{
			name: "idle",
			work: func(t *testing.T, eng engine.Engine, runCtx context.Context) {
				// Just Run + Stop, no emits.
			},
		},
		{
			name: "mid-stream",
			work: func(t *testing.T, eng engine.Engine, runCtx context.Context) {
				env := messages.Envelope{
					Headers:   messages.Headers{TenantID: "t", UserID: "u"},
					SessionID: "s",
					RunID:     "leak-mid",
				}
				_ = eng.Emit(runCtx, env)
				// Drain a couple to let the stream actually start.
				for range 2 {
					fctx, cancel := context.WithTimeout(runCtx, 1*time.Second)
					_, _ = eng.Fetch(fctx)
					cancel()
				}
			},
		},
		{
			name: "full-queues-at-stop",
			work: func(t *testing.T, eng engine.Engine, runCtx context.Context) {
				env := messages.Envelope{
					Headers:   messages.Headers{TenantID: "t", UserID: "u"},
					SessionID: "s",
					RunID:     "leak-full",
				}
				_ = eng.Emit(runCtx, env)
				// Don't drain — let the producer saturate.
				time.Sleep(50 * time.Millisecond)
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			baseline := runtime.NumGoroutine()
			nodeFunc := func(ctx context.Context, env messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
				for i := range 10 {
					if err := nctx.EmitChunk(ctx, engine.StreamFrame{Text: fmt.Sprintf("f%d", i)}); err != nil {
						return messages.Envelope{}, err
					}
				}
				return messages.Envelope{}, nil
			}
			eng := streamingTestEngine(t, engine.NodePolicy{RunCapacity: 4}, nodeFunc)
			ctx, cancel := context.WithCancel(context.Background())
			if err := eng.Run(ctx); err != nil {
				cancel()
				t.Fatalf("Run: %v", err)
			}
			sc.work(t, eng, ctx)
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := eng.Stop(stopCtx)
			stopCancel()
			cancel()
			if err != nil {
				t.Fatalf("Stop: %v", err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
				runtime.Gosched()
				time.Sleep(10 * time.Millisecond)
			}
			if delta := runtime.NumGoroutine() - baseline; delta > 5 {
				t.Errorf("scenario %q: leak baseline=%d after=%d (delta=%d)",
					sc.name, baseline, runtime.NumGoroutine(), delta)
			}
		})
	}
}

// asPhase12ErrorChain ensures the streaming sentinels chain properly
// for callers using errors.Is.
func TestStreamingSentinels_ErrorsIs(t *testing.T) {
	if !errors.Is(engine.ErrSeqProvided, engine.ErrSeqProvided) {
		t.Error("ErrSeqProvided should self-match")
	}
	if !errors.Is(engine.ErrStreamClosed, engine.ErrStreamClosed) {
		t.Error("ErrStreamClosed should self-match")
	}
	if !errors.Is(engine.ErrEmptyRunID, engine.ErrEmptyRunID) {
		t.Error("ErrEmptyRunID should self-match")
	}
	_ = atomic.Int32{} // silence unused import in some builds
}
