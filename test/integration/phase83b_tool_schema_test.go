// Package integration — Phase 83b wave-15 integration test.
//
// Phase 83b enriches the ReAct system prompt's `<available_tools>`
// section with per-tool `args_schema`, `side_effects`, and curated
// examples. Phase 107c (D-167) NARROWS the prompt-side rendering to
// name+description only — schemas now live in the provider's native
// `Tools[]` declaration. The integration tests below were broadened
// to cover both shapes: registration + examples validation (still
// relevant), prompt-side name+description rendering (updated).
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/drivers/inproc"
)

// promptRecorderLLM is an llm.LLMClient that records every system
// message it is asked to complete, then returns a fixed `_finish`
// envelope. Safe for concurrent use (the slice append is mutex-
// guarded).
type promptRecorderLLM struct {
	mu      sync.Mutex
	systems []string
}

func (c *promptRecorderLLM) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	for _, m := range req.Messages {
		if m.Role == llm.RoleSystem && m.Content.Text != nil {
			c.systems = append(c.systems, *m.Content.Text)
		}
	}
	c.mu.Unlock()
	// Phase 107c (D-167) — native `_finish` ToolCall replaces the
	// prompt-engineered JSON envelope. The projector translates the
	// reserved name to Finish{Goal, Payload: <answer>}.
	return llm.CompleteResponse{
		ToolCalls: []llm.ToolCallStructured{{
			ID:   "call_done",
			Name: "_finish",
			Args: json.RawMessage(`{"answer":"done"}`),
		}},
	}, nil
}

func (c *promptRecorderLLM) Close(_ context.Context) error { return nil }

func (c *promptRecorderLLM) lastSystem() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.systems) == 0 {
		return ""
	}
	return c.systems[len(c.systems)-1]
}

// catalogView83b adapts a real tools.ToolCatalog to the planner's
// ToolCatalogView, applying the run's identity filter — the production
// wiring shape (visibility-filtered view, schemas only).
type catalogView83b struct {
	cat    tools.ToolCatalog
	filter tools.CatalogFilter
}

func (v catalogView83b) Resolve(name string) (tools.Tool, bool) {
	d, ok := v.cat.Resolve(name)
	return d.Tool, ok
}

func (v catalogView83b) List() []tools.Tool { return v.cat.List(v.filter) }

// searchArgs83b is the typed input for the registered `kb_search`
// tool; inproc.RegisterFunc derives the args_schema from it.
type searchArgs83b struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type searchOut83b struct {
	Hits []string `json:"hits"`
}

// registerKBSearch83b registers a real in-process tool with curated
// examples on a real catalog. Returns the catalog.
func registerKBSearch83b(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[searchArgs83b, searchOut83b](cat, "kb_search",
		func(_ context.Context, in searchArgs83b) (searchOut83b, error) {
			return searchOut83b{Hits: []string{in.Query}}, nil
		},
		tools.WithDescription("Search the knowledge base."),
		tools.WithSideEffect(tools.SideEffectRead),
		tools.WithExamples(
			tools.ToolExample{
				Description: "bounded result set",
				Args:        map[string]any{"query": "revenue", "limit": 5},
				Tags:        []string{"common"},
			},
			tools.ToolExample{
				Description: "broadest search",
				Args:        map[string]any{"query": "quarterly revenue"},
				Tags:        []string{"minimal"},
			},
		),
	)
	if err != nil {
		t.Fatalf("RegisterFunc(kb_search): %v", err)
	}
	return cat
}

// TestE2E_Phase83b_EnrichedCatalogReachesPrompt proves the name+
// description <available_tools> block reaches the LLM through a real
// catalog + real planner. Phase 107c (D-167) narrows prompt-side
// rendering; args_schema / side_effects / examples are NOT rendered
// (they live in req.Tools[]).
func TestE2E_Phase83b_EnrichedCatalogReachesPrompt(t *testing.T) {
	t.Parallel()
	cat := registerKBSearch83b(t)
	rec := &promptRecorderLLM{}
	p := react.New(rec)

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		RunID:    "run-1",
	}
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{
		Quadruple: q,
		Goal:      "find revenue docs",
		Catalog: catalogView83b{cat: cat, filter: tools.CatalogFilter{
			TenantID: "t1", UserID: "u1", SessionID: "s1",
		}},
	}
	if _, err := p.Next(ctx, rc); err != nil {
		t.Fatalf("Next: %v", err)
	}

	sys := rec.lastSystem()
	if sys == "" {
		t.Fatal("planner sent no system message")
	}
	for _, want := range []string{
		"<available_tools>",
		"kb_search",
		"Search the knowledge base",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q.\nPrompt:\n%s", want, sys)
		}
	}
	// Phase 107c (D-167): these are NOT rendered in the prompt.
	for _, forbidden := range []string{"args_schema:", "side_effects:", "examples:"} {
		if strings.Contains(sys, forbidden) {
			t.Errorf("Phase 107c: system prompt leaks %q (should be name+desc only).\nPrompt:\n%s", forbidden, sys)
		}
	}
}

// TestE2E_Phase83b_MaxExamplesKnobBounds proves the
// MaxToolExamplesPerTool knob does not break registration or planner
// boot. Phase 107c (D-167) ignores the knob at render time (prompt-side
// rendering is name+description only); the knob is kept for the
// req.Tools[]-side (step 9). This test verifies the planner still boots
// and renders a valid prompt with the knob set.
func TestE2E_Phase83b_MaxExamplesKnobBounds(t *testing.T) {
	t.Parallel()
	cat := registerKBSearch83b(t)
	rec := &promptRecorderLLM{}
	p := react.New(rec, react.WithMaxToolExamplesPerTool(1))

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "t2", UserID: "u2", SessionID: "s2"},
		RunID:    "run-2",
	}
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	rc := planner.RunContext{
		Quadruple: q,
		Goal:      "g",
		Catalog: catalogView83b{cat: cat, filter: tools.CatalogFilter{
			TenantID: "t2", UserID: "u2", SessionID: "s2",
		}},
	}
	if _, err := p.Next(ctx, rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	sys := rec.lastSystem()
	if !strings.Contains(sys, "kb_search") {
		t.Errorf("prompt missing kb_search.\nPrompt:\n%s", sys)
	}
}

// TestE2E_Phase83b_InvalidExampleFailsRegistration is the failure-mode
// assertion (§17.3): an example whose arg key is absent from the
// tool's args_schema fails catalog registration loudly.
func TestE2E_Phase83b_InvalidExampleFailsRegistration(t *testing.T) {
	t.Parallel()
	cat := tools.NewCatalog()
	err := inproc.RegisterFunc[searchArgs83b, searchOut83b](cat, "bad_search",
		func(_ context.Context, in searchArgs83b) (searchOut83b, error) {
			return searchOut83b{}, nil
		},
		tools.WithExamples(tools.ToolExample{
			// `querty` is a typo — not a field of searchArgs83b.
			Args: map[string]any{"querty": "typo"},
			Tags: []string{"minimal"},
		}),
	)
	if err == nil {
		t.Fatal("RegisterFunc accepted an example with an undeclared arg key")
	}
	if !strings.Contains(err.Error(), "querty") {
		t.Errorf("error %q should name the offending arg key", err.Error())
	}
}

// TestE2E_Phase83b_ConcurrentRunsNoPromptBleed runs N≥10 concurrent
// Next calls against ONE shared planner with disjoint identities and
// asserts each run's system prompt carries the tool name — no cross-run
// bleed under -race (cross-package D-025 stress). Phase 107c (D-167)
// narrows to name+description only; args_schema is no longer asserted.
func TestE2E_Phase83b_ConcurrentRunsNoPromptBleed(t *testing.T) {
	t.Parallel()
	const N = 16
	cat := registerKBSearch83b(t)
	p := react.New(&promptRecorderLLM{}) // shared planner

	var (
		wg    sync.WaitGroup
		fails int64
	)
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			rec := &promptRecorderLLM{}
			runID := "run-" + itoa83b(i)
			q := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  "t-" + itoa83b(i),
					UserID:    "u-" + itoa83b(i),
					SessionID: "s-" + itoa83b(i),
				},
				RunID: runID,
			}
			ctx, err := identity.WithRun(context.Background(), q.Identity, runID)
			if err != nil {
				atomic.AddInt64(&fails, 1)
				return
			}
			rp := react.New(rec)
			rc := planner.RunContext{
				Quadruple: q,
				Goal:      "g",
				Catalog: catalogView83b{cat: cat, filter: tools.CatalogFilter{
					TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID,
				}},
			}
			if _, err := rp.Next(ctx, rc); err != nil {
				atomic.AddInt64(&fails, 1)
				return
			}
			if !strings.Contains(rec.lastSystem(), "kb_search") {
				atomic.AddInt64(&fails, 1)
			}
		}()
	}
	wg.Wait()
	_ = p
	if fails != 0 {
		t.Errorf("%d concurrent runs failed the name+description assertion", fails)
	}
}

// itoa83b is a tiny int-to-string for unique per-run identity
// components (avoids importing strconv for one call site).
func itoa83b(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
