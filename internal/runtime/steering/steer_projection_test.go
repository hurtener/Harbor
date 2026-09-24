package steering

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

type steeringRequestClient struct{ requests []llm.CompleteRequest }

type steerModelFunc func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error)

func (f steerModelFunc) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	return f(ctx, req)
}

func (steerModelFunc) Close(context.Context) error { return nil }

func TestRun_SteerInvalidatesUndispatchedSerialTail(t *testing.T) {
	loop, registry, _ := newTestRunLoop(t)
	requests, executions := 0, 0
	client := steerModelFunc(func(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		requests++
		if requests == 1 {
			return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{
				{ID: "first", Name: "inspect", Args: json.RawMessage(`{}`)},
				{ID: "obsolete-tail", Name: "inspect", Args: json.RawMessage(`{}`)},
			}}, nil
		}
		calls, results, correction := 0, 0, false
		for _, msg := range req.Messages {
			calls += len(msg.ToolCalls)
			if msg.Role == llm.RoleTool {
				results++
			}
			if msg.Content.Text != nil && *msg.Content.Text == "Stop the old plan" {
				correction = msg.Role == llm.RoleUser
			}
			for _, call := range msg.ToolCalls {
				if call.ID == "obsolete-tail" {
					t.Error("unexecuted tail entered the request as execution evidence")
				}
			}
		}
		if calls != 1 || results != 1 || !correction {
			t.Errorf("calls=%d results=%d correction=%v", calls, results, correction)
		}
		return llm.CompleteResponse{Content: "corrected", FinishReason: "stop"}, nil
	})
	spec := runSpecFor(runA, react.New(client, react.WithParallelToolCalls(false)))
	spec.Base.Catalog = steeringRequestCatalog{}
	spec.Base.Trajectory = &planner.Trajectory{}
	spec.ToolExecutor = checkpointExecutor(func(ctx context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
		executions++
		if err := enqueueCorrection(registry, "Stop the old plan"); err != nil {
			return nil, nil, err
		}
		if err := ctx.Err(); err != nil {
			t.Error("steering cancelled already-admitted tool execution")
		}
		return "inspected", "inspected", nil
	})
	if _, err := loop.Run(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || executions != 1 {
		t.Fatalf("model=%d dispatch=%d", requests, executions)
	}
}

func (c *steeringRequestClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.requests = append(c.requests, req)
	if len(c.requests) <= 2 {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: fmt.Sprintf("inspect-%d", len(c.requests)), Name: "inspect", Args: json.RawMessage(`{}`)}}}, nil
	}
	return llm.CompleteResponse{Content: "Done.", FinishReason: "stop"}, nil
}

func (*steeringRequestClient) Close(context.Context) error { return nil }

type steeringRequestCatalog struct{}

func (steeringRequestCatalog) Resolve(name string) (tools.Tool, bool) {
	return tools.Tool{Name: "inspect"}, name == "inspect"
}
func (steeringRequestCatalog) List() []tools.Tool { return []tools.Tool{{Name: "inspect"}} }

func TestRun_UserMessageReachesModelAndSurvivesLaterSteps(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained-%v", retained), func(t *testing.T) {
			loop, registry, _ := newTestRunLoop(t)
			client := &steeringRequestClient{}
			spec := runSpecFor(runA, react.New(client))
			spec.Base.Catalog = steeringRequestCatalog{}
			if retained {
				spec.Base.Trajectory = &planner.Trajectory{}
				spec.DispatchCheckpoint = &contextCheckpointRecorder{checkpointRecorder: checkpointRecorder{t: t}}
			}
			const correction = "Use amber accents.\n  Preserve NORTH-STAR-47."
			calls := 0
			spec.ToolExecutor = checkpointExecutor(func(_ context.Context, _ planner.RunContext, _ planner.Decision) (any, any, error) {
				calls++
				if calls == 1 {
					inbox, err := registry.Lookup(runA)
					if err != nil {
						return nil, nil, err
					}
					if err := inbox.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: runA, CallerTenant: runA.TenantID, CallerScope: ScopeSessionUser, Payload: map[string]any{"message": correction}}); err != nil {
						return nil, nil, err
					}
				}
				return "inspected", "inspected", nil
			})
			if _, err := loop.Run(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 3 || calls != 2 {
				t.Fatalf("model=%d tools=%d", len(client.requests), calls)
			}
			for i, req := range client.requests[1:] {
				found := false
				for _, msg := range req.Messages {
					if msg.Role == llm.RoleUser && msg.Content.Text != nil && strings.Contains(*msg.Content.Text, "NORTH-STAR-47") {
						found = true
					}
				}
				if !found {
					t.Errorf("subsequent model request %d lost steering", i+2)
				}
			}
			exact := false
			for _, msg := range client.requests[1].Messages {
				if msg.Role == llm.RoleUser && msg.Content.Text != nil && *msg.Content.Text == correction {
					exact = true
				}
			}
			if !exact {
				t.Error("new steering was not projected as an exact user message")
			}
			if got := len(loop.ControlHistory(runA.SessionID)); got != 1 {
				t.Errorf("applied controls=%d, want 1", got)
			}
		})
	}
}
