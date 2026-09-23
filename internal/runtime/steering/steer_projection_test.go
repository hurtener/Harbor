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
