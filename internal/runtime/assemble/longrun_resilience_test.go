package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

// bulkResultBytes reproduces the incident shape: each tool result sits just
// below the heavy-output offload threshold, so it stays inline in the
// trajectory, yet its rendered exchange is too large for one summary part.
const bulkResultBytes = 126 * 1024

// longRunDriver scripts a run of bulk reads followed by a final answer, and a
// summariser that can be made to fail.
type longRunDriver struct {
	mu            sync.Mutex
	reads         map[string]int
	decisions     map[string]int
	failSummaries bool
	summaryReqs   []llm.CompleteRequest
	decisionReqs  map[string][]llm.CompleteRequest
}

func newLongRunDriver(reads map[string]int) *longRunDriver {
	return &longRunDriver{reads: reads, decisions: map[string]int{}, decisionReqs: map[string][]llm.CompleteRequest{}}
}

func (d *longRunDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if system := req.Messages[0].Content.Text; system != nil && strings.Contains(*system, "You summarize historical agent execution") {
		d.summaryReqs = append(d.summaryReqs, req)
		if d.failSummaries {
			return llm.CompleteResponse{}, errors.New("fixture summariser unavailable")
		}
		return llm.CompleteResponse{Content: `{"goals":["collect the reports"],"facts":["bulk reports were read"],"pending":["answer"],"last_output_digest":"reads complete","note":""}`, FinishReason: "stop"}, nil
	}
	d.decisionReqs[q.RunID] = append(d.decisionReqs[q.RunID], req)
	d.decisions[q.RunID]++
	if n := d.decisions[q.RunID]; n <= d.reads[q.RunID] {
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: fmt.Sprint("read-", n), Name: "bulk_read",
			Args: json.RawMessage(fmt.Sprintf(`{"ordinal":%d}`, n))}}, FinishReason: "tool_calls"}, nil
	}
	return llm.CompleteResponse{Content: "Done.", FinishReason: "stop"}, nil
}

func (*longRunDriver) Close(context.Context) error { return nil }

func longRunStack(t *testing.T, driver *longRunDriver, window int, dsn string) *assemble.Stack {
	t.Helper()
	cfg := minimalCfg(t)
	cfg.State.Driver, cfg.State.DSN = "sqlite", dsn
	cfg.Artifacts.HeavyOutputThresholdBytes = 128 * 1024
	cfg.Memory.Strategy, cfg.Memory.RecentTurns, cfg.Memory.BudgetTokens = "rolling_summary", 1, 0
	name := "long-run-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	snapshot := llm.ConfigSnapshot{Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
		ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: window}},
		DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
	if err != nil {
		t.Fatal(err)
	}
	if err := stack.Catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "bulk_read", Description: "Read one bulk report",
		Transport: tools.TransportInProcess, Source: "long-run-fixture", Loading: tools.LoadingAlways,
		ArgsSchema: json.RawMessage(`{"type":"object","properties":{"ordinal":{"type":"integer"}},"required":["ordinal"]}`)},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			var input struct {
				Ordinal int `json:"ordinal"`
			}
			if err := json.Unmarshal(args, &input); err != nil {
				return tools.ToolResult{}, err
			}
			return tools.ToolResult{Value: map[string]any{"report": fmt.Sprintf("REPORT-%d-", input.Ordinal) + strings.Repeat("r", bulkResultBytes)}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	return stack
}

func requestText(req llm.CompleteRequest) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		if msg.Content.Text != nil {
			b.WriteString(*msg.Content.Text)
		}
		for _, part := range msg.Content.Parts {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Incident regression, part one: during a long run whose exchanges each sit
// just under the offload threshold, maintenance clips oversized exchanges to
// previews plus references (instead of failing on "exchange N requires a
// bounded result reference"), and every decision request stays in the window.
func TestLongRun_OversizedExchangesCompactWithClippedReferences(t *testing.T) {
	driver := newLongRunDriver(map[string]int{"long": 8})
	stack := longRunStack(t, driver, 200_000, filepath.Join(t.TempDir(), "long.db"))
	defer func() { _ = stack.Close(context.Background()) }()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "long"}
	if _, err := stack.RunOnce(t.Context(), "Read the eight reports", id, assemble.WithRunID("long")); err != nil {
		t.Fatalf("long run failed: %v", err)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.summaryReqs) == 0 {
		t.Fatal("the run never needed maintenance; the fixture does not reproduce the incident")
	}
	clipped := false
	for _, req := range driver.summaryReqs {
		for _, part := range req.Messages[1].Content.Parts {
			if len(part.Text) >= 128*1024 {
				t.Fatalf("summary part of %d bytes reached the heavy threshold", len(part.Text))
			}
		}
		clipped = clipped || strings.Contains(requestText(req), "clipped for maintenance")
	}
	if !clipped {
		t.Fatal("no exchange was clipped to a preview plus reference")
	}
	profile := llm.ModelProfile{ContextWindowTokens: 200_000}
	for i, req := range driver.decisionReqs["long"] {
		if est := llm.EstimateRequestTokens(req, profile); est >= 200_000 {
			t.Fatalf("decision %d estimated %d tokens, over the window", i+1, est)
		}
	}
}

// Incident regression, part two: a session whose retained history no longer
// fits the model (the "every later turn fails" state) recovers on its next
// turn, whether maintenance works again or keeps failing.
func TestLongRun_StuckSessionRecoversOnNextTurn(t *testing.T) {
	for _, mode := range []string{"summariser-recovers", "summariser-fails"} {
		t.Run(mode, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "stuck.db")
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "stuck-" + mode}
			// A large window lets the seed turn retain ~330k tokens of exact
			// history without any maintenance.
			seedDriver := newLongRunDriver(map[string]int{"seed": 10})
			seed := longRunStack(t, seedDriver, 2_000_000, dsn)
			if _, err := seed.RunOnce(t.Context(), "Read ten reports", id, assemble.WithRunID("seed")); err != nil {
				t.Fatalf("seed turn: %v", err)
			}
			if err := seed.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			// The next turn runs against a model whose window the retained
			// history exceeds several times over.
			driver := newLongRunDriver(map[string]int{})
			driver.failSummaries = mode == "summariser-fails"
			stack := longRunStack(t, driver, 120_000, dsn)
			defer func() { _ = stack.Close(context.Background()) }()
			if _, err := stack.RunOnce(t.Context(), "Summarise what you found", id, assemble.WithRunID("next")); err != nil {
				t.Fatalf("stuck session did not recover: %v", err)
			}
			driver.mu.Lock()
			reqs := append([]llm.CompleteRequest(nil), driver.decisionReqs["next"]...)
			summaries := len(driver.summaryReqs)
			clipped := false
			for _, req := range driver.summaryReqs {
				clipped = clipped || strings.Contains(requestText(req), "clipped for maintenance")
			}
			driver.mu.Unlock()
			if mode == "summariser-recovers" && !clipped {
				t.Fatal("historical exchanges were not clipped to previews plus references")
			}
			if len(reqs) == 0 || summaries == 0 {
				t.Fatalf("decisions=%d summaries=%d; maintenance was never attempted", len(reqs), summaries)
			}
			profile := llm.ModelProfile{ContextWindowTokens: 120_000}
			for i, req := range reqs {
				if est := llm.EstimateRequestTokens(req, profile); est >= 120_000 {
					t.Fatalf("decision %d estimated %d tokens, over the window", i+1, est)
				}
			}
			text := requestText(reqs[0])
			if mode == "summariser-fails" && !strings.Contains(text, "Full payload available") {
				t.Fatal("reduced history did not keep a reference to the exact evidence")
			}
			// A further turn proceeds normally on the recovered session.
			driver.mu.Lock()
			driver.failSummaries = false
			driver.mu.Unlock()
			if _, err := stack.RunOnce(t.Context(), "And one more question", id, assemble.WithRunID("after")); err != nil {
				t.Fatalf("turn after recovery: %v", err)
			}
		})
	}
}
