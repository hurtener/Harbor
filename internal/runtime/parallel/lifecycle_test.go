package parallel_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestExecute_ParallelBranches_EmitLifecycle pins the R2 parallel-branch
// coverage: a native CallParallel turn (the default N>1 dispatch since
// the 107d cutover) dispatched through the parallel executor's branches
// emits ≥2 quadruple-stamped `tool.invoked` events — one per branch —
// plus matching terminal events, because the branch descriptors are
// resolved from a bus-carrying catalog whose descriptor-wrap shell emits
// by construction. Guards the phase-107d ≥2-tool.invoked-per-turn
// invariant under the wrap seam.
func TestExecute_ParallelBranches_EmitLifecycle(t *testing.T) {
	t.Parallel()

	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 8,
		SubscriberBufferSize:     64,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tools.EventTypeToolInvoked, tools.EventTypeToolCompleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	// A bus-carrying catalog wraps every registered descriptor's Invoke;
	// the parallel executor resolves branches from it.
	cat := tools.NewCatalog(tools.WithCatalogBus(bus))
	for _, n := range []string{"alpha", "beta"} {
		if regErr := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{Name: n, Transport: tools.TransportInProcess},
			Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{Value: map[string]any{"tool": n, "args": string(args)}}, nil
			},
		}); regErr != nil {
			t.Fatalf("Register(%q): %v", n, regErr)
		}
	}

	exec := parallel.New(cat)
	q := fixedQ(t, "r-parallel-lifecycle")
	call := planner.CallParallel{
		Branches: []planner.CallTool{
			{Tool: "alpha", Args: json.RawMessage(`{"x":1}`)},
			{Tool: "beta", Args: json.RawMessage(`{"x":2}`)},
		},
		Join: &planner.JoinSpec{Kind: planner.JoinAll},
	}
	results, err := exec.Execute(ctxWithQ(t, q), call)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}

	// Collect 4 events (2 invoked + 2 completed) within a bounded window.
	invoked := 0
	completed := 0
	deadline := time.After(2 * time.Second)
loop:
	for invoked+completed < 4 {
		select {
		case ev := <-sub.Events():
			if ev.Identity.RunID != "r-parallel-lifecycle" {
				t.Errorf("event %s not turn-attributable: RunID=%q", ev.Type, ev.Identity.RunID)
			}
			switch ev.Type {
			case tools.EventTypeToolInvoked:
				invoked++
			case tools.EventTypeToolCompleted:
				completed++
			}
		case <-deadline:
			break loop
		}
	}
	if invoked < 2 {
		t.Errorf("observed %d tool.invoked, want ≥2 (one per branch)", invoked)
	}
	if completed < 2 {
		t.Errorf("observed %d tool.completed, want ≥2 (one per branch)", completed)
	}
}
