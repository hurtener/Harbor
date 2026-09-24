package parallel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/parallel"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestExecutor_ShortCircuitJoinWaitsForRequiredCleanup(t *testing.T) {
	for _, kind := range []planner.JoinKind{planner.JoinFirstSuccess, planner.JoinN} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(ctxWithQ(t, fixedQ(t, "cleanup")), 3*time.Second)
			defer cancel()
			started := make(chan struct{})
			finished := make(chan struct{})
			cat := tools.NewCatalog()
			for _, name := range []string{"winner", "cleanup"} {
				if err := cat.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: name}, Invoke: func(ctx context.Context, _ json.RawMessage) (tools.ToolResult, error) {
					if name == "winner" {
						select {
						case <-started:
							return tools.ToolResult{Value: "receipt"}, nil
						case <-ctx.Done():
							return tools.ToolResult{}, ctx.Err()
						}
					}
					close(started)
					<-ctx.Done()
					defer close(finished)
					return tools.ToolResult{}, tools.ErrInvocationCleanupFailed
				}}); err != nil {
					t.Fatal(err)
				}
			}
			results, err := parallel.New(cat).Execute(ctx, planner.CallParallel{Branches: []planner.CallTool{{Tool: "winner"}, {Tool: "cleanup"}}, Join: &planner.JoinSpec{Kind: kind, N: 1}})
			if !errors.Is(err, tools.ErrInvocationCleanupFailed) || len(results) != 2 {
				t.Fatalf("cleanup hidden by success: results=%+v err=%v", results, err)
			}
			select {
			case <-finished:
			default:
				t.Fatal("executor returned with unjoined cleanup")
			}
		})
	}
}
