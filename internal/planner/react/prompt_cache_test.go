package react

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tools"
)

// promptCacheFixtureTools is a catalog spanning every stability tier. Names
// are chosen so a plain name sort would interleave the tiers: the
// user-owned tool sorts FIRST alphabetically.
func promptCacheFixtureTools(withUserTool bool) []tools.Tool {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)
	out := []tools.Tool{
		{Name: "artifact_fetch", Description: "Fetch a stored result window.", ArgsSchema: schema, Transport: tools.TransportInProcess},
		{Name: "tool_search", Description: "Search the tool catalog.", ArgsSchema: schema, Transport: tools.TransportInProcess},
		{Name: "crm~a-0011223344556677_lookup", Description: "Agent CRM lookup.", ArgsSchema: schema, Transport: tools.TransportMCP, Source: "crm~a-0011223344556677"},
		{Name: "docs_search", Description: "Operator docs search.", ArgsSchema: schema, Transport: tools.TransportHTTP, Source: "docs"},
	}
	if withUserTool {
		// A user's connection attaching between turns.
		out = append(out, tools.Tool{Name: "aaa~u-8899aabbccddeeff_mail", Description: "User mailbox.", ArgsSchema: schema, Transport: tools.TransportMCP, Source: "aaa~u-8899aabbccddeeff"})
	}
	return out
}

// nameSortedCatalog mimics the production catalog's List: sorted by name.
type nameSortedCatalog struct{ stubCatalog }

func newNameSortedCatalog(in []tools.Tool) *nameSortedCatalog {
	sorted := append([]tools.Tool(nil), in...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Name < sorted[i].Name {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return &nameSortedCatalog{stubCatalog{tools: sorted}}
}

func promptCacheCtx(t *testing.T, q identity.Quadruple, agent string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return tools.WithEffectiveAgentConfig(ctx, agent)
}

func quad(tenant, session, run string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-1", SessionID: session}, RunID: run}
}

// nextRequest runs one planner step and returns the request it sent.
func nextRequest(t *testing.T, ctx context.Context, rc planner.RunContext) llm.CompleteRequest {
	t.Helper()
	client := &lastRequestClient{}
	p := New(client)
	if _, err := p.Next(ctx, rc); err != nil {
		t.Fatalf("Next: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.req == nil {
		t.Fatal("planner made no LLM call")
	}
	return *client.req
}

func systemText(t *testing.T, req llm.CompleteRequest) string {
	t.Helper()
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem || req.Messages[0].Content.Text == nil {
		t.Fatal("request has no leading system message")
	}
	return *req.Messages[0].Content.Text
}

// A user-owned connection attaching between two turns (different sessions)
// must leave the provider-visible prefix untouched up to the user-owned
// block: every earlier declaration keeps its exact position and bytes, the
// system spine is unchanged, and the prompt cache scope is identical.
func TestNext_PromptPrefixStableWhenUserScopedToolAttaches(t *testing.T) {
	t.Parallel()
	turn1 := nextRequest(t, promptCacheCtx(t, quad("tenant-a", "sess-1", "run-1"), "agent-1"), planner.RunContext{
		Quadruple: quad("tenant-a", "sess-1", "run-1"), Goal: "hola",
		Catalog: newNameSortedCatalog(promptCacheFixtureTools(false)),
	})
	turn2 := nextRequest(t, promptCacheCtx(t, quad("tenant-a", "sess-2", "run-2"), "agent-1"), planner.RunContext{
		Quadruple: quad("tenant-a", "sess-2", "run-2"), Goal: "hola de nuevo",
		Catalog: newNameSortedCatalog(promptCacheFixtureTools(true)),
	})

	if len(turn2.Tools) != len(turn1.Tools)+1 {
		t.Fatalf("turn 2 declares %d tools, want %d", len(turn2.Tools), len(turn1.Tools)+1)
	}
	for i := range turn1.Tools {
		a, b := turn1.Tools[i], turn2.Tools[i]
		if a.Name != b.Name || a.Description != b.Description || string(a.Schema) != string(b.Schema) {
			t.Fatalf("declaration %d changed when a user tool attached: %q -> %q", i, a.Name, b.Name)
		}
	}
	if last := turn2.Tools[len(turn2.Tools)-1].Name; last != sanitizeToolName("aaa~u-8899aabbccddeeff_mail") {
		t.Errorf("user-owned tool declared at %q, want it last", last)
	}

	// Tier order: builtins, then shared, then agent-owned, then user-owned.
	var order []tools.PromptStabilityTier
	byName := map[string]tools.Tool{}
	for _, tool := range promptCacheFixtureTools(true) {
		byName[sanitizeToolName(tool.Name)] = tool
	}
	for _, d := range turn2.Tools {
		if tool, ok := byName[d.Name]; ok {
			order = append(order, tools.PromptStabilityTierOf(tool))
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] < order[i-1] {
			t.Errorf("declaration tiers out of order: %v", order)
			break
		}
	}

	spine1, spine2 := systemSpine(turn1.Messages), systemSpine(turn2.Messages)
	if spine1 == "" || spine1 != spine2 {
		t.Errorf("system spine changed across turns (or is empty)")
	}
	if turn1.PromptCacheScope == "" || turn1.PromptCacheScope != turn2.PromptCacheScope {
		t.Errorf("prompt cache scope changed across turns: %q vs %q", turn1.PromptCacheScope, turn2.PromptCacheScope)
	}
}

// The scope is stable for the same inputs and separates tenants and agents.
func TestNext_PromptCacheScopeSeparatesTenantsAndAgents(t *testing.T) {
	t.Parallel()
	scope := func(tenant, agent, session string) string {
		q := quad(tenant, session, "run-"+session)
		return nextRequest(t, promptCacheCtx(t, q, agent), planner.RunContext{
			Quadruple: q, Goal: "g-" + session,
			Catalog: newNameSortedCatalog(promptCacheFixtureTools(false)),
		}).PromptCacheScope
	}
	base := scope("tenant-a", "agent-1", "s1")
	if again := scope("tenant-a", "agent-1", "s2"); again != base {
		t.Errorf("same tenant/agent gave a different scope: %q vs %q", again, base)
	}
	if other := scope("tenant-b", "agent-1", "s3"); other == base {
		t.Error("different tenants share a prompt cache scope")
	}
	if other := scope("tenant-a", "agent-2", "s4"); other == base {
		t.Error("different agents share a prompt cache scope")
	}
	for _, raw := range []string{"tenant-a", "agent-1"} {
		if strings.Contains(base, raw) {
			t.Errorf("scope %q carries the raw identifier %q", base, raw)
		}
	}
}

// Per-turn content (goal, trajectory, steering, discovered tools) never
// reaches the base system message; only the trailing <current_date> varies,
// and only by day.
func TestNext_SystemPromptCarriesNoPerTurnContent(t *testing.T) {
	t.Parallel()
	q1, q2 := quad("tenant-a", "sess-1", "run-1"), quad("tenant-a", "sess-2", "run-2")
	cat := newNameSortedCatalog(promptCacheFixtureTools(false))
	a := nextRequest(t, promptCacheCtx(t, q1, "agent-1"), planner.RunContext{Quadruple: q1, Goal: "first question", Catalog: cat})
	b := nextRequest(t, promptCacheCtx(t, q2, "agent-1"), planner.RunContext{
		Quadruple: q2, Goal: "a different question", Catalog: cat,
		Control: planner.ControlSignals{UserMessages: []string{"steer this way"}},
	})
	sa, sb := systemText(t, a), systemText(t, b)
	if sa != sb {
		t.Errorf("base system message differs between turns:\n--- a ---\n%s\n--- b ---\n%s", sa, sb)
	}
	for _, leak := range []string{"first question", "a different question", "steer this way", "sess-", "run-"} {
		if strings.Contains(sa, leak) || strings.Contains(sb, leak) {
			t.Errorf("system message carries per-turn content %q", leak)
		}
	}
	if !strings.HasSuffix(sa, "</current_date>") {
		t.Errorf("the volatile date is not the final system section")
	}
}

// Each tool description reaches the model exactly once: in its native
// declaration, never again in the system prompt.
func TestNext_ToolDescriptionsAreNotDuplicated(t *testing.T) {
	t.Parallel()
	q := quad("tenant-a", "sess-1", "run-1")
	req := nextRequest(t, promptCacheCtx(t, q, "agent-1"), planner.RunContext{
		Quadruple: q, Goal: "g", Catalog: newNameSortedCatalog(promptCacheFixtureTools(true)),
	})
	var prompt strings.Builder
	for _, m := range req.Messages {
		if m.Content.Text != nil {
			prompt.WriteString(*m.Content.Text)
		}
	}
	for _, d := range req.Tools {
		if d.Description == "" {
			continue
		}
		if strings.Contains(prompt.String(), d.Description) {
			t.Errorf("description of %q appears in the prompt as well as its declaration", d.Name)
		}
		if !strings.Contains(prompt.String(), "- "+d.Name+"\n") && !strings.HasPrefix(d.Name, "_") {
			t.Errorf("declared tool %q missing from <available_tools>", d.Name)
		}
	}
}

// TestDefaultBuilder_GoldenToolsPrompt pins the rendered <available_tools>
// block for the canonical two-tool fixture: names only.
func TestDefaultBuilder_GoldenToolsPrompt(t *testing.T) {
	t.Parallel()
	const goldenPath = "testdata/golden_tools_prompt.txt"
	got := renderAvailableToolsSection(planner.RunContext{Catalog: fixtureCatalog()}, 0)
	if os.Getenv("HARBOR_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("update golden fixture: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	if got != string(want) {
		t.Errorf("rendered <available_tools> diverged from %s (regenerate with HARBOR_UPDATE_GOLDEN=1).\n--- got ---\n%s\n--- want ---\n%s", goldenPath, got, want)
	}
}

// TestPromptCacheScope_ConcurrentReuse runs one shared planner across 128
// concurrent runs of four tenants and asserts each run's scope equals its
// tenant's sequential scope (no cross-run bleed).
func TestPromptCacheScope_ConcurrentReuse(t *testing.T) {
	t.Parallel()
	client := &recordingScopeClient{}
	p := New(client)
	cat := newNameSortedCatalog(promptCacheFixtureTools(true))
	want := map[string]string{}
	for _, tenant := range []string{"t0", "t1", "t2", "t3"} {
		q := quad(tenant, "seed", "seed")
		want[tenant] = nextRequest(t, promptCacheCtx(t, q, "agent-1"), planner.RunContext{Quadruple: q, Goal: "g", Catalog: cat}).PromptCacheScope
	}
	const n = 128
	errs := make(chan error, n)
	for i := range n {
		go func() {
			tenant := []string{"t0", "t1", "t2", "t3"}[i%4]
			q := quad(tenant, "s", "r")
			q.SessionID += string(rune('a' + i%26))
			ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
			if err != nil {
				errs <- err
				return
			}
			ctx = tools.WithEffectiveAgentConfig(ctx, "agent-1")
			_, err = p.Next(ctx, planner.RunContext{Quadruple: q, Goal: "g", Catalog: cat})
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.seen) != n {
		t.Fatalf("recorded %d calls, want %d", len(client.seen), n)
	}
	for _, s := range client.seen {
		if s.scope != want[s.tenant] {
			t.Errorf("tenant %s got scope %q, want %q", s.tenant, s.scope, want[s.tenant])
		}
	}
}

type scopeRecord struct{ tenant, scope string }

// recordingScopeClient records the calling tenant and the scope of every
// request; safe for concurrent use.
type recordingScopeClient struct {
	mu   sync.Mutex
	seen []scopeRecord
}

func (c *recordingScopeClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, _ := identity.QuadrupleFrom(ctx)
	c.mu.Lock()
	c.seen = append(c.seen, scopeRecord{tenant: q.TenantID, scope: req.PromptCacheScope})
	c.mu.Unlock()
	return llm.CompleteResponse{Content: "done"}, nil
}

func (c *recordingScopeClient) Close(context.Context) error { return nil }

// lastRequestClient records the last request and answers with prose.
type lastRequestClient struct {
	mu  sync.Mutex
	req *llm.CompleteRequest
}

func (c *lastRequestClient) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	c.req = &req
	c.mu.Unlock()
	return llm.CompleteResponse{Content: "done"}, nil
}

func (c *lastRequestClient) Close(context.Context) error { return nil }
