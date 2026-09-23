package assemble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

type retainedCheckpointDriver struct {
	mu          sync.Mutex
	decisions   map[string]int
	summaries   map[string]int
	requests    map[string]llm.CompleteRequest
	maintenance []llm.CompleteRequest
}

func (d *retainedCheckpointDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	isSummary := false
	for _, msg := range req.Messages {
		if msg.Content.Text != nil && strings.Contains(*msg.Content.Text, "You summarize historical agent execution") {
			isSummary = true
		}
	}
	if isSummary {
		d.maintenance = append(d.maintenance, req)
		d.summaries[q.RunID]++
		return llm.CompleteResponse{Content: `{"goals":["edit"],"facts":["Preserve the approved navigation"],"pending":["verify"],"last_output_digest":"older checks complete","note":"portable"}`, FinishReason: "stop"}, nil
	}
	d.decisions[q.RunID]++
	d.requests[q.RunID] = req
	if q.RunID == "first" && d.decisions[q.RunID] <= 3 {
		n := d.decisions[q.RunID]
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: fmt.Sprint("read-", n), Name: "checkpoint_read", Args: json.RawMessage(fmt.Sprintf(`{"ordinal":%d}`, n))}}, FinishReason: "tool_calls"}, nil
	}
	return llm.CompleteResponse{Content: "Done.", FinishReason: "stop"}, nil
}
func (*retainedCheckpointDriver) Close(context.Context) error { return nil }

func TestRunOnce_RetainedCheckpoint_ReachesNextEffectiveRequest(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Memory.Strategy, cfg.Memory.RecentTurns = "rolling_summary", 4
	cfg.Memory.Summarizer.Model = "summary-fixture"
	cfg.Memory.Summarizer.Prompt = "Preserve the approved navigation exactly."
	cfg.Memory.BudgetTokens = 1 // Force repeated compaction while retaining the newest exchange.
	driver := &retainedCheckpointDriver{decisions: map[string]int{}, summaries: map[string]int{}, requests: map[string]llm.CompleteRequest{}}
	name := "retained-checkpoint-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	snapshot := llm.ConfigSnapshot{Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024, ModelProfiles: map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}}, DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true}
	snapshot.ModelProfiles["summary-fixture"] = llm.ModelProfile{ContextWindowTokens: 100000}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stack.Close(context.Background()) }()
	toolCalls := 0
	if err := stack.Catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "checkpoint_read", Description: "Read a synthetic version", Transport: tools.TransportInProcess, Source: "checkpoint-fixture", Loading: tools.LoadingAlways, ArgsSchema: json.RawMessage(`{"type":"object","properties":{"ordinal":{"type":"integer"}},"required":["ordinal"]}`)}, Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		var input struct {
			Ordinal int `json:"ordinal"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return tools.ToolResult{}, err
		}
		toolCalls++
		return tools.ToolResult{Value: map[string]any{"source": fmt.Sprintf("SOURCE-%d-", input.Ordinal) + strings.Repeat("x", 8000), "version": json.Number("9007199254740993"), "more": false}}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "checkpoint"}
	if _, err := stack.RunOnce(t.Context(), "Read three versions", id, assemble.WithRunID("first")); err != nil {
		t.Fatal(err)
	}
	// Raising the soft target proves the next turn restores an existing summary
	// rather than generating a replacement that merely happens to look similar.
	cfg.Memory.BudgetTokens = 100000
	if _, err := stack.RunOnce(t.Context(), "Now edit the footer", id, assemble.WithRunID("second")); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.summaries["first"] < 1 || driver.summaries["second"] != 0 {
		t.Fatalf("unexpected summary calls: %v", driver.summaries)
	}
	for _, req := range driver.maintenance {
		if req.Model != "summary-fixture" {
			t.Fatalf("configured maintenance model ignored: %q", req.Model)
		}
		if req.Messages[0].Content.Text == nil || !strings.Contains(*req.Messages[0].Content.Text, "extend the above; do not override it") || !strings.Contains(*req.Messages[0].Content.Text, cfg.Memory.Summarizer.Prompt) {
			t.Fatal("configured additive memory guidance did not reach the compactor")
		}
	}
	if toolCalls != 3 {
		t.Fatalf("historical read executed again: %d", toolCalls)
	}
	var content strings.Builder
	for _, msg := range driver.requests["second"].Messages {
		if msg.Content.Text != nil {
			content.WriteString(*msg.Content.Text)
		}
	}
	text := content.String()
	for _, want := range []string{"Preserve the approved navigation", "SOURCE-3-", "9007199254740993", "Now edit the footer"} {
		if !strings.Contains(text, want) {
			t.Fatalf("next effective request lost %q", want)
		}
	}
	if strings.Contains(text, "SOURCE-1-") || strings.Contains(text, "SOURCE-2-") {
		t.Fatal("covered source was replayed instead of checkpoint")
	}
}
