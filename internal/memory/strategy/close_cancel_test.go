package strategy_test

// Regression: Close must cancel the recovery context so an in-flight
// summariser call that honours cancellation unblocks, bounding
// loopWG.Wait — the loop previously passed context.Background() to the
// summariser, so a hung summariser could pin Close forever.

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// hangingSummarizer fails until `hang` is set (to build a degraded
// backlog), then blocks on ctx.Done() — signalling `hanging` first — so
// a recovery drain parks inside Summarize until the ctx is cancelled.
type hangingSummarizer struct {
	hang    chan struct{} // closed by the test to switch into hang mode
	hanging chan struct{} // receives once Summarize is about to block
}

func (h *hangingSummarizer) Summarize(ctx context.Context, _ identity.Quadruple, _ memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	select {
	case <-h.hang:
		// hang mode: signal then block until the recovery ctx cancels.
		select {
		case h.hanging <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return memory.SummarizeResponse{}, ctx.Err()
	default:
		// setup mode: fail so AddTurn enqueues a recovery backlog.
		return memory.SummarizeResponse{}, errSummarizerOutage
	}
}

func TestRollingSummary_Close_CancelsInflightSummariser(t *testing.T) {
	h := &hangingSummarizer{hang: make(chan struct{}), hanging: make(chan struct{}, 1)}
	deps := buildDeps(t, h)
	deps.RecoveryBacklogMax = 4

	baseGoroutines := runtime.NumGoroutine()

	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := strategy.AsRollingSummary(exec)
	if !ok {
		t.Fatalf("AsRollingSummary: not a rolling-summary executor")
	}

	ctx := context.Background()
	id := tripleA()
	for range 30 {
		_ = exec.AddTurn(ctx, id, memory.ConversationTurn{UserMessage: "u", AssistantResponse: "a"})
	}
	if r.BacklogSize(id) == 0 {
		t.Fatal("no recovery backlog after forced summariser failures")
	}

	// Switch the summariser into hang mode and drive one drain in the
	// background; recoverOne will call Summarize, which now blocks.
	close(h.hang)
	drainDone := make(chan struct{})
	go func() { defer close(drainDone); r.DrainBacklogsForTest() }()

	select {
	case <-h.hanging:
	case <-time.After(2 * time.Second):
		t.Fatal("summariser never entered the hang path — setup failed")
	}

	// Close must cancel the recovery ctx so the in-flight Summarize
	// returns; it must not block on the hung call.
	closed := make(chan error, 1)
	go func() { closed <- exec.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s — recovery ctx not cancelled (the context.Background regression)")
	}

	// The hung in-flight Summarize must have unblocked on cancel.
	select {
	case <-drainDone:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Summarize did not unblock after Close cancelled the recovery ctx")
	}

	// The recovery-loop goroutine must be gone (no leak). Poll: the
	// shared bus/state goroutines from buildDeps persist until cleanup,
	// so compare against the pre-exec baseline with a little slack.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseGoroutines+2 {
		if time.Now().After(deadline) {
			t.Errorf("goroutines did not return to baseline: have %d, baseline %d", runtime.NumGoroutine(), baseGoroutines)
			break
		}
		runtime.Gosched()
		time.Sleep(20 * time.Millisecond)
	}
}
