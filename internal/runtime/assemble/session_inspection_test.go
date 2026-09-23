package assemble_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	"github.com/hurtener/Harbor/internal/planner/react"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
)

func TestRunOnce_MemoryInspectionUsesExecutionOwner(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.LLM.Model = "mock/echo"
	cfg.LLM.ModelProfiles = map[string]config.LLMModelProfileConfig{"mock/echo": {ContextWindowTokens: 100000, TokenEstimator: "chars_div_4"}}
	cfg.Memory.Strategy = "rolling_summary"
	cfg.Memory.RecentTurns = 4
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	client := &retainedRecordingClient{calls: map[string]int{}, requests: map[string][]llm.CompleteRequest{}}
	stack.Planner = react.New(client)
	id := identity.Identity{TenantID: "inspection-tenant", UserID: "inspection-user", SessionID: "inspection-session"}
	q := identity.Quadruple{Identity: id, RunID: "viewer-one"}
	const constraint = "FIRST-ONLY-NORTH-STAR-47"
	if _, err := stack.RunOnce(t.Context(), constraint, id, assemble.WithRunID("agent-first")); err != nil {
		t.Fatal(err)
	}
	list, err := memprotocol.List(t.Context(), memprotocol.ListDeps{Store: stack.Memory, DriverName: cfg.State.Driver, HeavyThreshold: 1 << 20}, prototypes.MemoryListRequest{}, q)
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("real execution invisible: items=%v err=%v", list.Items, err)
	}
	key := list.Items[0].Key
	q.RunID = "different-viewer"
	get, err := memprotocol.Get(t.Context(), memprotocol.GetDeps{Store: stack.Memory, Artifacts: stack.Artifacts, DriverName: cfg.State.Driver, HeavyThreshold: 1 << 20}, prototypes.MemoryGetRequest{Key: key}, q)
	if err != nil || !strings.Contains(string(get.Detail.Value), constraint) || get.Detail.Item.ExpiresAt.IsZero() {
		t.Fatalf("get=%+v err=%v", get, err)
	}
	// A memory read must not create an independently retained copy of an
	// expiring private source. Source-bound heavy retrieval remains required.
	if _, err := memprotocol.Get(t.Context(), memprotocol.GetDeps{Store: stack.Memory, Artifacts: stack.Artifacts, DriverName: cfg.State.Driver, HeavyThreshold: 1}, prototypes.MemoryGetRequest{Key: key}, q); !errors.Is(err, memprotocol.ErrContextLeak) {
		t.Fatalf("heavy expiry boundary=%v", err)
	}
	put, err := memprotocol.Put(t.Context(), memprotocol.PutDeps{Store: stack.Memory, Bus: stack.Bus}, prototypes.MemoryPutRequest{Turn: prototypes.MemoryTurnInput{UserMessage: "SECOND-NOTE-ONLY", AssistantResponse: "operator note"}}, q)
	if err != nil || put.Key == "" {
		t.Fatalf("put=%+v err=%v", put, err)
	}
	if _, err := stack.RunOnce(t.Context(), "continue", id, assemble.WithRunID("agent-next")); err != nil {
		t.Fatal(err)
	}
	request := client.body(t, id.SessionID+"/agent-next")
	if !strings.Contains(request, constraint) || !strings.Contains(request, "SECOND-NOTE-ONLY") {
		t.Fatal("inspection writes and execution read different owners")
	}
	if _, err := memprotocol.Delete(t.Context(), memprotocol.DeleteDeps{Store: stack.Memory, Bus: stack.Bus}, prototypes.MemoryDeleteRequest{Key: key}, q); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.RunOnce(t.Context(), "continue again", id, assemble.WithRunID("agent-after-delete")); err != nil {
		t.Fatal(err)
	}
	request = client.body(t, id.SessionID+"/agent-after-delete")
	if strings.Contains(request, constraint) || !strings.Contains(request, "SECOND-NOTE-ONLY") {
		t.Fatal("deletion lost an unrelated source or retained the deleted one")
	}
	if _, err := memprotocol.Get(t.Context(), memprotocol.GetDeps{Store: stack.Memory, Artifacts: stack.Artifacts, DriverName: cfg.State.Driver, HeavyThreshold: 1 << 20}, prototypes.MemoryGetRequest{Key: put.Key}, q); err != nil {
		t.Fatalf("unrelated key moved after deletion: %v", err)
	}
}
