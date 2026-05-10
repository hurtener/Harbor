package engine_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// recordingCancelHandler records every RunCancelledNotice the engine
// fires. Goroutine-safe so tests can install one shared handler across
// multiple Cancel calls.
type recordingCancelHandler struct {
	mu      sync.Mutex
	notices []engine.RunCancelledNotice
}

func (r *recordingCancelHandler) handle(_ context.Context, n engine.RunCancelledNotice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, n)
}

func (r *recordingCancelHandler) snapshot() []engine.RunCancelledNotice {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]engine.RunCancelledNotice, len(r.notices))
	copy(out, r.notices)
	return out
}

// blockingNode parks until the test signals release, then returns the
// envelope. Used to keep a worker mid-invocation while the test calls
// Cancel.
func blockingNode(release <-chan struct{}, started chan<- struct{}) engine.NodeFunc {
	return func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return in, nil
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		}
	}
}

// TestCancel_HappyPath_ReturnsTrue — Cancel observed at least one
// in-flight worker, returns (true, nil).
func TestCancel_HappyPath_ReturnsTrue(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	defer close(release)

	a := engine.Node{Name: "A", Func: blockingNode(release, started)}
	e, err := engine.New([]engine.Adjacency{{From: a}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	if err := e.Emit(context.Background(), envFor(id, "R-active")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never started")
	}

	ok, err := e.Cancel(context.Background(), "R-active")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if !ok {
		t.Errorf("ok=false, want true (worker mid-invocation)")
	}
}

// TestCancel_Idempotent_DoubleCancel — second Cancel for the same
// runID returns (false, nil) because the run had no fresh observable
// presence (the flag was already set).
func TestCancel_Idempotent_DoubleCancel(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	if _, err := e.Cancel(context.Background(), "R-idem"); err != nil {
		t.Fatalf("Cancel 1: %v", err)
	}
	ok, err := e.Cancel(context.Background(), "R-idem")
	if err != nil {
		t.Fatalf("Cancel 2: %v", err)
	}
	if ok {
		t.Errorf("second Cancel returned ok=true, want false (idempotent)")
	}
}

// TestCancel_BeforeEmit_RememberedForTTL — Cancel for a runID before
// Emit is recorded, and a subsequent Emit lands as ErrRunCancelled.
func TestCancel_BeforeEmit_RememberedForTTL(t *testing.T) {
	t.Parallel()
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	if _, err := e.Cancel(context.Background(), "R-pre"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	id := ident("T", "U", "S")
	err := e.Emit(context.Background(), envFor(id, "R-pre"))
	if !errors.Is(err, engine.ErrRunCancelled) {
		t.Errorf("Emit err=%v, want ErrRunCancelled", err)
	}
}

// TestCancel_EmitsBusEvent — the configured RunCancelledHandler fires
// with the right RunID + droppedCount.
func TestCancel_EmitsBusEvent(t *testing.T) {
	t.Parallel()
	rec := &recordingCancelHandler{}
	a := engine.Node{Name: "A", Func: passthrough}
	e, err := engine.New(
		[]engine.Adjacency{{From: a}},
		engine.WithRunCancelledHandler(rec.handle),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	if _, err := e.Cancel(context.Background(), "R-bus"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	notices := rec.snapshot()
	if len(notices) != 1 {
		t.Fatalf("notices=%d, want 1", len(notices))
	}
	if notices[0].RunID != "R-bus" {
		t.Errorf("RunID=%q, want R-bus", notices[0].RunID)
	}
	if notices[0].CancelledAt.IsZero() {
		t.Errorf("CancelledAt is zero — handler should receive wall time")
	}
}

// TestCancel_OneRun_LeavesOtherCompletes — concurrent runs A and B
// in a fan-in graph (two inlets, one outlet). Each inlet has its own
// worker goroutine, so Cancel(R-A) can run while R-B's worker is
// still mid-invocation. Verifies the cancel scope is per-run, not
// engine-wide.
func TestCancel_OneRun_LeavesOtherCompletes(t *testing.T) {
	t.Parallel()
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	startedA := make(chan struct{}, 1)
	startedB := make(chan struct{}, 1)
	defer close(releaseA)
	defer close(releaseB)

	a := engine.Node{
		Name: "A",
		Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			startedA <- struct{}{}
			select {
			case <-releaseA:
				return in, nil
			case <-ctx.Done():
				return messages.Envelope{}, ctx.Err()
			}
		},
	}
	b := engine.Node{
		Name: "B",
		Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
			startedB <- struct{}{}
			select {
			case <-releaseB:
				return in, nil
			case <-ctx.Done():
				return messages.Envelope{}, ctx.Err()
			}
		},
	}
	e, err := engine.New([]engine.Adjacency{
		{From: a},
		{From: b},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	if err := e.EmitTo(context.Background(), envFor(id, "R-A"), engine.NodeRef{Name: "A"}); err != nil {
		t.Fatalf("EmitTo A: %v", err)
	}
	if err := e.EmitTo(context.Background(), envFor(id, "R-B"), engine.NodeRef{Name: "B"}); err != nil {
		t.Fatalf("EmitTo B: %v", err)
	}
	<-startedA
	<-startedB

	if _, err := e.Cancel(context.Background(), "R-A"); err != nil {
		t.Fatalf("Cancel A: %v", err)
	}
	// Release B; verify it reaches egress unaffected by Cancel(R-A).
	releaseB <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := e.FetchByRun(ctx, "R-B")
	if err != nil {
		t.Fatalf("FetchByRun(R-B): %v", err)
	}
	if got.RunID != "R-B" {
		t.Errorf("RunID=%q, want R-B", got.RunID)
	}
}

// TestCancel_CrossTenant_NoBleed — Cancel(runID-tenantA) must not
// touch runs of tenantB. Emits N runs across N tenants; cancels one;
// verifies the others reach egress with their identities intact.
func TestCancel_CrossTenant_NoBleed(t *testing.T) {
	t.Parallel()
	const N = 8
	a := engine.Node{Name: "A", Func: passthrough}
	e, _ := engine.New(
		[]engine.Adjacency{{From: a}},
		engine.WithQueueSize(N*2+4),
	)
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	// Emit one envelope per tenant. Passthrough → each lands in the
	// dispatcher's per-run subqueue + anyRun.
	for i := 0; i < N; i++ {
		idTri := ident(fmt.Sprintf("T%d", i), "U", "S")
		if err := e.Emit(context.Background(), envFor(idTri, fmt.Sprintf("R-%d", i))); err != nil {
			t.Fatalf("Emit %d: %v", i, err)
		}
	}

	// Drain the first one (R-0) on the per-run subqueue so we know
	// every run has reached the dispatcher (the worker processes
	// envelopes serially → if R-0 made it, all earlier siblings did
	// too; the last few may still be in flight, so we drain by-run
	// for each in turn below).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Cancel R-0. The cancel happens after the worker has processed
	// it (because the channel write is fast for passthrough), so the
	// per-run subqueue holds R-0's envelope when Cancel drains it.
	if _, err := e.Cancel(context.Background(), "R-0"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// FetchByRun(R-0) must report ErrRunCancelled.
	if _, err := e.FetchByRun(ctx, "R-0"); !errors.Is(err, engine.ErrRunCancelled) {
		t.Errorf("FetchByRun(R-0) err=%v, want ErrRunCancelled", err)
	}

	// All other tenants' runs reach FetchByRun cleanly with the right
	// identity.
	for i := 1; i < N; i++ {
		runID := fmt.Sprintf("R-%d", i)
		got, err := e.FetchByRun(ctx, runID)
		if err != nil {
			t.Fatalf("FetchByRun(%s): %v", runID, err)
		}
		wantTenant := fmt.Sprintf("T%d", i)
		if got.Identity().TenantID != wantTenant {
			t.Errorf("RunID=%s tenant=%q, want %q", runID, got.Identity().TenantID, wantTenant)
		}
	}
}

// TestCancel_DuringStreaming_NoDeadlock — a producer mid-stream is
// cancelled. The blocked EmitChunk wakes with ErrRunCancelled; the
// engine doesn't deadlock; goroutines clean up.
func TestCancel_DuringStreaming_NoDeadlock(t *testing.T) {
	t.Parallel()
	gotErr := make(chan error, 1)
	emittedFirst := make(chan struct{}, 1)
	nodeFunc := engine.NodeFunc(func(ctx context.Context, in messages.Envelope, nctx *engine.NodeContext) (messages.Envelope, error) {
		// Emit one frame, signal the test, then keep emitting until
		// EmitChunk wakes with ErrRunCancelled.
		if err := nctx.EmitChunk(ctx, engine.StreamFrame{Text: "first"}); err != nil {
			gotErr <- err
			return messages.Envelope{}, err
		}
		emittedFirst <- struct{}{}
		// Burst more frames — RunCapacity=2 so we'll block.
		for i := 0; i < 100; i++ {
			if err := nctx.EmitChunk(ctx, engine.StreamFrame{Text: fmt.Sprintf("f%d", i)}); err != nil {
				gotErr <- err
				return messages.Envelope{}, err
			}
		}
		return messages.Envelope{}, nil
	})
	eng := streamingTestEngine(t, engine.NodePolicy{RunCapacity: 2}, nodeFunc)
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = eng.Stop(context.Background()) }()

	if err := eng.Emit(context.Background(), streamEnv("R-stream")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	<-emittedFirst

	// Cancel mid-stream. The blocked producer's EmitChunk wakes with
	// ErrRunCancelled.
	if _, err := eng.Cancel(context.Background(), "R-stream"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case err := <-gotErr:
		if !errors.Is(err, engine.ErrRunCancelled) {
			t.Fatalf("EmitChunk err=%v, want ErrRunCancelled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked producer never woke from cancel — deadlock")
	}
}

// TestCancel_Idempotent_Property — N goroutines call Cancel(runID)
// concurrently; exactly one returns (true, nil) when the run is
// active. The rest return (false, nil) and no panics / races.
func TestCancel_Idempotent_Property(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	defer close(release)

	a := engine.Node{Name: "A", Func: blockingNode(release, started)}
	e, _ := engine.New([]engine.Adjacency{{From: a}})
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	id := ident("T", "U", "S")
	if err := e.Emit(context.Background(), envFor(id, "R-prop")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	<-started

	const N = 32
	var trueCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ok, err := e.Cancel(context.Background(), "R-prop")
			if err != nil {
				t.Errorf("Cancel: %v", err)
			}
			if ok {
				trueCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := trueCount.Load(); got != 1 {
		t.Errorf("Cancel returned ok=true %d times, want exactly 1", got)
	}
}

// TestCancel_RejectsEmptyRunID — Cancel with empty runID returns an
// error. Identity is mandatory.
func TestCancel_RejectsEmptyRunID(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = e.Stop(context.Background()) }()

	_, err := e.Cancel(context.Background(), "")
	if err == nil {
		t.Fatalf("Cancel(empty) returned nil, want error")
	}
}

// TestCancel_AfterStop_ReturnsErrEngineStopped — Cancel on a stopped
// engine fails closed.
func TestCancel_AfterStop_ReturnsErrEngineStopped(t *testing.T) {
	t.Parallel()
	e, _ := engine.New(linearGraph())
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_, err := e.Cancel(context.Background(), "R")
	if !errors.Is(err, engine.ErrEngineStopped) {
		t.Fatalf("err=%v, want ErrEngineStopped", err)
	}
}
