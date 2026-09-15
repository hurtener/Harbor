package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

type settingsCapture struct{ calls chan llm.CompleteRequest }

func (c *settingsCapture) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.calls <- req
	return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "done", Name: "_finish", Args: json.RawMessage(`{"answer":"ok"}`)}}}, nil
}
func (*settingsCapture) Close(context.Context) error { return nil }

func TestRunLLMSettingsReachProviderWithoutConsumingPendingSlot(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	rl, err := steering.NewRunLoop(steering.NewRegistry(), pauseresume.New(pauseresume.WithBus(bus)), steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatal(err)
	}
	cap := &settingsCapture{calls: make(chan llm.CompleteRequest, 128)}
	pending := runsprotocol.NewStore()
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{Bus: bus, RunLoop: rl, Planner: react.New(cap), Tasks: reg, SessionOverrides: pending})
	if err != nil {
		t.Fatal(err)
	}
	// One shared driver executes independently accepted choices.
	if err := driver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = driver.Close(context.Background()) }()

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	legacy := "legacy-model"
	pending.Set(id, runsprotocol.PendingOverride{Model: &legacy})
	for i := range 128 {
		model, effort, max := fmt.Sprint("model-", i), []string{"low", "medium", "high"}[i%3], 100+i
		if _, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "test", LLMSettings: &llm.RunSettings{Model: &model, ReasoningEffort: &effort, MaxTokens: &max}}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 128 {
		select {
		case req := <-cap.calls:
			var i int
			if _, err := fmt.Sscanf(req.Model, "model-%d", &i); err != nil || i < 0 || i >= 128 {
				t.Fatalf("unexpected model %q", req.Model)
			}
			if seen[req.Model] {
				t.Fatalf("duplicate model %q", req.Model)
			}
			seen[req.Model] = true
			if req.ReasoningEffort != llm.ReasoningEffort([]string{"low", "medium", "high"}[i%3]) || req.MaxTokens == nil || *req.MaxTokens != 100+i {
				t.Fatalf("settings mixed for %q", req.Model)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("timed out awaiting provider request")
		}
	}
	if got, ok := pending.Consume(id); !ok || got.Model == nil || *got.Model != legacy {
		t.Fatal("explicit tasks consumed the legacy slot")
	}
}
