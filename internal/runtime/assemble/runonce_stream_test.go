// runonce_stream_test.go — coverage for the WithStream streaming sink
// on RunOnce (D-266): the primitive-with-consumer end-to-end test
// (ordered token/step chunks arrive BEFORE the final envelope,
// deterministic because the seam fires synchronously on the run
// goroutine), the StreamEvent kind mapping, and a nil-sink no-op guard.
// The no-cross-run chunk-bleed guarantee is pinned by the extended
// N>=100 concurrent-reuse stress in runonce_test.go.
package assemble_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	_ "github.com/hurtener/Harbor/internal/llm/mock"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

// TestRunOnce_WithStream_ChunksArriveBeforeEnvelope is the §13
// primitive-with-consumer end-to-end test: a WithStream sink on the SAME
// blocking RunOnce observes token deltas + step boundaries, and EVERY
// event is delivered before RunOnce returns the envelope — deterministic
// because the OnChunk seam fires synchronously on the run goroutine.
func TestRunOnce_WithStream_ChunksArriveBeforeEnvelope(t *testing.T) {
	ctx := context.Background()
	stack := runnableStack(t)
	defer func() { _ = stack.Close(ctx) }()

	var (
		mu        sync.Mutex
		events    []assemble.StreamEvent
		returned  atomic.Bool
		lateEvent atomic.Bool
	)
	sink := func(e assemble.StreamEvent) {
		// The load-bearing ordering assertion: no event may arrive after
		// RunOnce has returned the envelope. Synchronous-seam delivery
		// makes this hold deterministically.
		if returned.Load() {
			lateEvent.Store(true)
		}
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	const goal = "summarise the streaming deployment status"
	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, goal, id, assemble.WithStream(sink))
	returned.Store(true)
	if err != nil {
		t.Fatalf("RunOnce(WithStream): %v", err)
	}
	if lateEvent.Load() {
		t.Fatal("a StreamEvent arrived AFTER RunOnce returned the envelope — the seam is not synchronous")
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Fatalf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}

	mu.Lock()
	captured := append([]assemble.StreamEvent(nil), events...)
	mu.Unlock()

	if len(captured) == 0 {
		t.Fatal("WithStream sink observed no events — streaming did not fire")
	}

	var (
		tokenCount, stepCount int
		contentTokens         strings.Builder
	)
	for _, e := range captured {
		switch e.Kind {
		case assemble.StreamToken:
			tokenCount++
			if !e.Reasoning {
				contentTokens.WriteString(e.Text)
			}
		case assemble.StreamStep:
			stepCount++
		case assemble.StreamToolDispatched:
			// The mock LLM does not dispatch tools; this kind is exercised
			// by TestRunOnce_StreamEventKinds_Mapping + the wiring.
		}
	}
	if tokenCount == 0 {
		t.Errorf("expected >=1 StreamToken event, got %d", tokenCount)
	}
	if stepCount == 0 {
		t.Errorf("expected >=1 StreamStep (planner-step boundary) event, got %d", stepCount)
	}
	// Ordering within the run: the streamed content tokens reconstruct the
	// run's own answer, so the assembled token text carries the goal the
	// mock echoes back — proving the sink saw THIS run's chunks.
	if got := contentTokens.String(); !strings.Contains(got, goal) {
		t.Errorf("assembled content tokens %q do not contain the run's goal %q", got, goal)
	}
}

// TestRunOnce_WithStream_NilSink_IsNoOp — WithStream(nil) is a no-op:
// RunOnce still returns the terminal answer with no panic.
func TestRunOnce_WithStream_NilSink_IsNoOp(t *testing.T) {
	ctx := context.Background()
	stack := runnableStack(t)
	defer func() { _ = stack.Close(ctx) }()

	id := identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"}
	env, err := stack.RunOnce(ctx, "goal", id, assemble.WithStream(nil))
	if err != nil {
		t.Fatalf("RunOnce(WithStream(nil)): %v", err)
	}
	if env.FinishReason != string(planner.FinishGoal) {
		t.Errorf("FinishReason = %q, want %q", env.FinishReason, planner.FinishGoal)
	}
}

// TestRunOnce_StreamEventKinds_Mapping locks the three sealed-enum kinds
// to distinct values so a future rename can't silently collapse the
// streaming contract (the tool-dispatched kind has no mock-driven
// end-to-end path; this pins its identity).
func TestRunOnce_StreamEventKinds_Mapping(t *testing.T) {
	kinds := map[assemble.StreamEventKind]struct{}{
		assemble.StreamToken:          {},
		assemble.StreamToolDispatched: {},
		assemble.StreamStep:           {},
	}
	if len(kinds) != 3 {
		t.Fatalf("StreamEvent kinds must be 3 distinct values, got %d", len(kinds))
	}
	if assemble.StreamToken == assemble.StreamStep ||
		assemble.StreamToken == assemble.StreamToolDispatched ||
		assemble.StreamStep == assemble.StreamToolDispatched {
		t.Fatal("StreamEvent kinds collided")
	}
}
