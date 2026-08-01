package benchmarks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
)

// benchScriptedClient is a minimal, allocation-free llm.LLMClient for
// the react-step benchmark: every Complete returns the same scripted
// tool-call response, so each Planner.Next projects exactly one ReAct
// step (an LLM call → a CallTool decision) without a network round-trip.
// It is the benchmark fixture; the production planner runs against the
// real bifrost-backed client.
type benchScriptedClient struct {
	resp llm.CompleteResponse
}

func (c *benchScriptedClient) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	return c.resp, nil
}

func (c *benchScriptedClient) Close(_ context.Context) error { return nil }

// BenchmarkReActPlanner_NextStep_DeclaredTool measures one ReAct planner step
// against the REAL *react.ReActPlanner (no mock planner — only the LLM edge is
// scripted, as a benchmark cannot call a live model). The catalog fixture is
// load-bearing: each iteration must build a native declaration and resolve the
// model-authored name only through that exact per-turn projection.
func BenchmarkReActPlanner_NextStep_DeclaredTool(b *testing.B) {
	client := &benchScriptedClient{
		resp: llm.CompleteResponse{
			ToolCalls: []llm.ToolCallStructured{{
				ID:   "call_1",
				Name: "search_docs",
				Args: json.RawMessage(`{"query":"harbor"}`),
			}},
		},
	}
	p := react.New(client)
	// Build the same catalog/view seam a run receives. The model can only
	// return a declared provider-visible name; keeping this setup outside the
	// timed loop preserves the benchmark's focus on Planner.Next.
	catalog := tools.NewCatalog()
	if err := catalog.Register(tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        "search_docs",
			Description: "benchmark search fixture",
			Loading:     tools.LoadingAlways,
		},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{}, nil
		},
	}); err != nil {
		b.Fatalf("register benchmark tool: %v", err)
	}

	// Documented dummy identity quadruple — no secrets (CLAUDE.md §13).
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "bench-tenant", UserID: "bench-user", SessionID: "bench-session"},
		RunID:    "bench-run",
	}
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		b.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{
		Quadruple: q,
		Goal:      "answer the benchmark query",
		Catalog:   tools.NewPlannerView(catalog, tools.CatalogFilter{}),
		Emit:      func(events.Event) {},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := p.Next(ctx, rc); err != nil {
			b.Fatalf("Next: %v", err)
		}
	}
}

// BenchmarkReActPlanner_NextStep_ToolFree is the stable no-catalog terminal
// path. It keeps prompt assembly and response projection measurable without
// conflating their baseline with declared-tool snapshot construction.
func BenchmarkReActPlanner_NextStep_ToolFree(b *testing.B) {
	client := &benchScriptedClient{resp: llm.CompleteResponse{Content: "benchmark answer"}}
	p := react.New(client)
	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "bench-tenant", UserID: "bench-user", SessionID: "bench-session"},
		RunID:    "bench-run",
	}
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		b.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{
		Quadruple: q,
		Goal:      "answer the benchmark query",
		Emit:      func(events.Event) {},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := p.Next(ctx, rc); err != nil {
			b.Fatalf("Next: %v", err)
		}
	}
}
