package benchmarks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

// A deterministic provider measures framework work, not network/model latency.
// Compaction still uses the production governed client and trajectory summarizer.
type memoryBenchDriver struct{ summaries atomic.Int64 }

func (d *memoryBenchDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	for _, msg := range req.Messages {
		if msg.Content.Text != nil && strings.Contains(*msg.Content.Text, "You summarize historical agent execution") {
			d.summaries.Add(1)
			return llm.CompleteResponse{Content: `{"goals":["Iterate layout"],"facts":["Use the existing layout"],"pending":["Next edit"],"last_output_digest":"Done","note":""}`, FinishReason: "stop"}, nil
		}
	}
	return llm.CompleteResponse{Content: "Layout updated.", FinishReason: "stop"}, nil
}
func (*memoryBenchDriver) Close(context.Context) error { return nil }

func memoryBenchStack(b *testing.B) (*assemble.Stack, *memoryBenchDriver) {
	b.Helper()
	driver := &memoryBenchDriver{}
	name := "cumulative-benchmark-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := config.Defaults()
	cfg.LLM.Driver = name
	cfg.LLM.Model = "fixture"
	cfg.Memory.RecentTurns = 4
	cfg.Memory.BudgetTokens = 10000
	snapshot := llm.ConfigSnapshot{
		Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
		ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
		DisableCorrections: true, DisableRetry: true, DisableDowngrade: true,
	}
	stack, err := assemble.Assemble(b.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = stack.Close(context.Background()) })
	return stack, driver
}

func memoryBenchID() identity.Identity {
	return identity.Identity{TenantID: "bench-tenant", UserID: "bench-user", SessionID: "bench-session"}
}

// BenchmarkCumulativeSessionRun replaces the retired pair-executor comparison.
// It times real repeated runs, including cumulative rollover. Values are not
// comparable to the old AddTurn-only benchmark and need their own baseline.
func BenchmarkCumulativeSessionRun(b *testing.B) {
	stack, driver := memoryBenchStack(b)
	id := memoryBenchID()
	query := "Edit the existing layout. " + strings.Repeat("Keep the established typography. ", 16)
	for i := range 12 {
		if _, err := stack.RunOnce(b.Context(), query, id, assemble.WithRunID(fmt.Sprintf("warmup-%d", i))); err != nil {
			b.Fatal(err)
		}
	}
	if driver.summaries.Load() == 0 {
		b.Fatal("warmup did not exercise cumulative rollover")
	}
	before := driver.summaries.Load()
	b.ResetTimer()
	for i := range b.N {
		if _, err := stack.RunOnce(b.Context(), query, id, assemble.WithRunID(fmt.Sprintf("bench-%d", i))); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(driver.summaries.Load()-before)/float64(b.N), "compactions/op")
}

// BenchmarkMemoryInspection times the real administrative projection of a
// populated cumulative checkpoint and its recent tail, without an alternate
// transcript or a synthetic legacy context patch.
func BenchmarkMemoryInspection(b *testing.B) {
	stack, driver := memoryBenchStack(b)
	id := memoryBenchID()
	for i := range 12 {
		if _, err := stack.RunOnce(b.Context(), "Continue editing the layout", id, assemble.WithRunID(fmt.Sprintf("seed-%d", i))); err != nil {
			b.Fatal(err)
		}
	}
	if driver.summaries.Load() == 0 {
		b.Fatal("fixture has no cumulative checkpoint")
	}
	q := identity.Quadruple{Identity: id}
	view, err := stack.Memory.Inspect(b.Context(), q)
	if err != nil || view.Summary == "" {
		b.Fatalf("inspection lacks checkpoint: %+v, %v", view, err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(view.Summary), &summary); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		if _, err := stack.Memory.Inspect(b.Context(), q); err != nil {
			b.Fatal(err)
		}
	}
}
