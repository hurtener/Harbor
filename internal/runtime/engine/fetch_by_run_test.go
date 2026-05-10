package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// TestFetchByRun_HappyPath — Emit → FetchByRun returns the envelope
// with its identity intact.
func TestFetchByRun_HappyPath(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	in := envFor(id, "R-fetch")
	in.Payload = "payload"
	if err := e.Emit(context.Background(), in); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := e.FetchByRun(ctx, "R-fetch")
	if err != nil {
		t.Fatalf("FetchByRun: %v", err)
	}
	if got.RunID != "R-fetch" {
		t.Errorf("RunID=%q, want R-fetch", got.RunID)
	}
	if got.Payload != "payload" {
		t.Errorf("Payload=%v, want payload", got.Payload)
	}
}

// TestFetchByRun_ConcurrentSameRun_ReturnsErrConcurrentFetchByRun —
// two goroutines call FetchByRun for the same runID; exactly one
// blocks (the consumer), the other returns ErrConcurrentFetchByRun
// immediately.
func TestFetchByRun_ConcurrentSameRun_ReturnsErrConcurrentFetchByRun(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	// No Emit yet — both fetchers will block on an empty subqueue.
	// One claims the fetcher flag; the other returns immediately.
	const N = 8
	var wg sync.WaitGroup
	var concurrentCount atomic.Int32
	var blockedCount atomic.Int32
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := e.FetchByRun(ctx, "R-conc")
			switch {
			case errors.Is(err, engine.ErrConcurrentFetchByRun):
				concurrentCount.Add(1)
			case errors.Is(err, context.DeadlineExceeded):
				blockedCount.Add(1)
			default:
				t.Errorf("unexpected err=%v", err)
			}
		}()
	}
	wg.Wait()
	// Exactly one goroutine claimed the fetcher flag and blocked
	// until the deadline; the rest returned ErrConcurrentFetchByRun.
	if blockedCount.Load() != 1 {
		t.Errorf("blockedCount=%d, want 1 (single fetcher contract)", blockedCount.Load())
	}
	if concurrentCount.Load() != N-1 {
		t.Errorf("concurrentCount=%d, want %d", concurrentCount.Load(), N-1)
	}
}

// TestFetchByRun_AfterCancel_ReturnsErrRunCancelled — Cancel(runID)
// then FetchByRun(runID) returns ErrRunCancelled.
func TestFetchByRun_AfterCancel_ReturnsErrRunCancelled(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	if _, err := e.Cancel(context.Background(), "R-cancelled"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := e.FetchByRun(ctx, "R-cancelled")
	if !errors.Is(err, engine.ErrRunCancelled) {
		t.Errorf("err=%v, want ErrRunCancelled", err)
	}
}

// TestFetchByRun_CancelMidFetch_WakesWithErrRunCancelled — a blocked
// FetchByRun caller is woken by Cancel(runID) and returns
// ErrRunCancelled.
func TestFetchByRun_CancelMidFetch_WakesWithErrRunCancelled(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	// Trigger lazy subqueue creation by emitting once and draining;
	// this primes the dispatcher so the second FetchByRun blocks on
	// an existing-but-empty subqueue (the path Cancel must wake).
	id := ident("T", "U", "S")
	if err := e.Emit(context.Background(), envFor(id, "R-mid")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if _, err := e.FetchByRun(ctx, "R-mid"); err != nil {
		t.Fatalf("priming fetch: %v", err)
	}

	gotErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := e.FetchByRun(ctx, "R-mid")
		gotErr <- err
	}()

	// Give the fetcher a moment to register, then Cancel.
	time.Sleep(20 * time.Millisecond)
	if _, err := e.Cancel(context.Background(), "R-mid"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case err := <-gotErr:
		if !errors.Is(err, engine.ErrRunCancelled) {
			t.Errorf("err=%v, want ErrRunCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked FetchByRun never woke from Cancel")
	}
}

// TestFetchByRun_CtxCancelled_ReturnsCtxErr — caller's ctx cancels
// while FetchByRun is blocked; returns ctx.Err().
func TestFetchByRun_CtxCancelled_ReturnsCtxErr(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	ctx, cancel := context.WithCancel(context.Background())
	gotErr := make(chan error, 1)
	go func() {
		_, err := e.FetchByRun(ctx, "R-ctx")
		gotErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-gotErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchByRun did not honor ctx cancel")
	}
}

// TestFetchByRun_RejectsEmptyRunID — empty runID returns an error.
func TestFetchByRun_RejectsEmptyRunID(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	_, err := e.FetchByRun(context.Background(), "")
	if err == nil {
		t.Fatalf("FetchByRun(empty) returned nil, want error")
	}
}

// TestFetchByRun_AfterStop_ReturnsErrEngineStopped — Stop then
// FetchByRun fails closed.
func TestFetchByRun_AfterStop_ReturnsErrEngineStopped(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_, err := e.FetchByRun(context.Background(), "R")
	if !errors.Is(err, engine.ErrEngineStopped) {
		t.Errorf("err=%v, want ErrEngineStopped", err)
	}
}

// TestFetchByRun_SerialReuseAfterCompletion — after a single
// FetchByRun completes successfully, a second FetchByRun for the
// same runID can run again (the fetcher flag is released).
func TestFetchByRun_SerialReuseAfterCompletion(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	for i := 0; i < 2; i++ {
		env := envFor(id, "R-reuse")
		env.Payload = i
		if err := e.Emit(context.Background(), env); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		got, err := e.FetchByRun(ctx, "R-reuse")
		if err != nil {
			t.Fatalf("FetchByRun %d: %v", i, err)
		}
		if got.Payload != i {
			t.Errorf("Payload=%v, want %d", got.Payload, i)
		}
	}
}

// passthroughEnv is a no-op helper; included for API symmetry with
// envFor + Engine.Emit when callers want a plain envelope.
var _ = func() messages.Envelope {
	id := ident("T", "U", "S")
	return envFor(id, "R")
}
