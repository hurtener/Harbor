// Cumulative memory, the production compactor and the LLM admission edge,
// exercised through actual embedded runs over a real SQLite StateStore.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

const budgetHeavyThreshold = 32 * 1024
const budgetConstraint = "Keep the emergency exit on the north side."

type budgetDecision struct {
	body   string
	tokens int
}
type budgetDriver struct {
	mu             sync.Mutex
	decisions      map[string]budgetDecision
	summaries      []string
	summaryPadding string
	failSummary    atomic.Bool
}

func (d *budgetDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	var body strings.Builder
	for _, msg := range req.Messages {
		if msg.Content.Text != nil {
			body.WriteString(*msg.Content.Text)
		}
	}
	text := body.String()
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(text, "You summarize historical agent execution") {
		d.summaries = append(d.summaries, text)
		if d.failSummary.Load() {
			return llm.CompleteResponse{}, errors.New("fixture summary failure")
		}
		facts := []string{}
		if strings.Contains(text, budgetConstraint) {
			facts = append(facts, budgetConstraint)
		}
		seen := map[string]bool{}
		for _, marker := range regexp.MustCompile(`MARKER[0-9]{3}`).FindAllString(text, -1) {
			if !seen[marker] {
				facts = append(facts, marker)
				seen[marker] = true
			}
		}
		encoded, err := json.Marshal(map[string]any{
			"goals": []string{"Iterate the layout"}, "facts": facts, "pending": []string{"Next edit"},
			"last_output_digest": "Layout updated", "note": d.summaryPadding,
		})
		return llm.CompleteResponse{Content: string(encoded), FinishReason: "stop"}, err
	}
	d.decisions[q.RunID] = budgetDecision{body: text, tokens: llm.EstimateRequestTokens(req, llm.ModelProfile{TokenEstimator: "chars_div_4"})}
	return llm.CompleteResponse{Content: strings.Repeat("a", 2000), FinishReason: "stop"}, nil
}
func (*budgetDriver) Close(context.Context) error { return nil }
func (d *budgetDriver) decision(run string) budgetDecision {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decisions[run]
}
func (d *budgetDriver) summaryCount() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.summaries) }

type budgetSeam struct {
	mem    memory.MemoryStore
	client llm.LLMClient
	stack  *assemble.Stack
	driver *budgetDriver
}

func newBudgetSeam(t *testing.T, budgetTokens, windowTokens int) budgetSeam {
	t.Helper()
	d := &budgetDriver{decisions: map[string]budgetDecision{}}
	name := "budget-integration-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return d, nil })
	cfg := config.Defaults()
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")}
	cfg.Memory.BudgetTokens = budgetTokens
	cfg.LLM.Driver = name
	cfg.LLM.Model = "m"
	snapshot := llm.ConfigSnapshot{
		Driver: name, Model: "m", ContextWindowReserve: .05, HeavyOutputThreshold: budgetHeavyThreshold,
		ModelProfiles:      map[string]llm.ModelProfile{"m": {ContextWindowTokens: windowTokens, TokenEstimator: "chars_div_4"}},
		DisableCorrections: true, DisableRetry: true, DisableDowngrade: true,
	}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	return budgetSeam{mem: stack.Memory, client: stack.LLM, stack: stack, driver: d}
}
func budgetIdentity(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"}, "budget-direct")
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
func budgetTurn(t *testing.T, seam budgetSeam, id identity.Identity, run, query string) budgetDecision {
	t.Helper()
	if _, err := seam.stack.RunOnce(t.Context(), query, id, assemble.WithRunID(run)); err != nil {
		t.Fatalf("run %s: %v", run, err)
	}
	decision := seam.driver.decision(run)
	if decision.body == "" {
		t.Fatalf("no actual decision request for %s", run)
	}
	return decision
}

func TestE2E_Phase123_MemoryLLMBudget_StaysRunnable(t *testing.T) {
	const budget = 32 * 1024
	seam := newBudgetSeam(t, budget, 128*1024)
	id := identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"}
	for i := range 60 {
		query := fmt.Sprintf("turn-%02d-", i) + strings.Repeat("u", 2000)
		if i == 0 {
			query = budgetConstraint + " " + query
		}
		decision := budgetTurn(t, seam, id, fmt.Sprintf("budget-%d", i), query)
		if !strings.Contains(decision.body, budgetConstraint) {
			t.Fatalf("turn %d lost the first constraint", i)
		}
		if decision.tokens > budget {
			t.Fatalf("actual request at turn %d exceeds working budget: %d > %d", i, decision.tokens, budget)
		}
	}
	view, err := seam.mem.Inspect(t.Context(), identity.Quadruple{Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	if seam.driver.summaryCount() == 0 || view.Summary == "" {
		t.Fatalf("fixture did not exercise cumulative compaction: calls=%d bytes=%d", seam.driver.summaryCount(), len(view.Summary))
	}
}

// Summary generation has its own bounded allowance. A provider that exceeds
// that allowance must not evict the unsummarized sources to make room.
func TestE2E_Phase123_MemoryLLMBudget_OversizedSummaryPreservesSources(t *testing.T) {
	seam := newBudgetSeam(t, 100000, 128*1024)
	seam.driver.summaryPadding = strings.Repeat("Synthetic summary detail. ", 1500)
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "summary-failure"}
	for i := range 20 {
		query := fmt.Sprintf("edit %d", i)
		if i == 0 {
			query = budgetConstraint + query
		}
		budgetTurn(t, seam, id, fmt.Sprintf("prefix-%d", i), query)
	}
	q := identity.Quadruple{Identity: id}
	before, err := seam.mem.Inspect(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	_, err = seam.stack.RunOnce(t.Context(), "next edit", id, assemble.WithRunID("summary-failure"))
	if !errors.Is(err, summarizer.ErrTrajectorySummaryCapacity) {
		t.Fatalf("oversized summary: %v", err)
	}
	after, err := seam.mem.Inspect(t.Context(), q)
	if err != nil || len(after.Items) != len(before.Items) || after.Summary != before.Summary {
		t.Fatalf("failed summary changed committed history: %+v, %v", after, err)
	}
	for i, item := range after.Items {
		if item.Key != before.Items[i].Key || string(item.Value) != string(before.Items[i].Value) {
			t.Fatalf("failed summary changed source %d", i)
		}
	}
	if seam.driver.decision("summary-failure").body != "" {
		t.Fatal("decision ran after failed required compaction")
	}
}

// D-241 exempts conversation text from the tool-result byte threshold, but
// never from the model token guard. This is independent of summary limits.
func TestE2E_Phase123_MemoryLLMBudget_ConversationByteExemption(t *testing.T) {
	seam := newBudgetSeam(t, 32*1024, 128*1024)
	text := "Historical conversation: " + strings.Repeat("c", budgetHeavyThreshold+1024)
	_, err := seam.client.Complete(budgetIdentity(t), llm.CompleteRequest{
		Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleSystem, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("legitimate conversation text failed the byte exemption: %v", err)
	}
}

func TestE2E_Phase123_MemoryLLMBudget_ZeroUsesModelCapacity(t *testing.T) {
	seam := newBudgetSeam(t, 0, 10000)
	id := identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "automatic"}
	for i := range 60 {
		query := fmt.Sprintf("turn-%02d-", i) + strings.Repeat("u", 800)
		if i == 0 {
			query = budgetConstraint + " " + query
		}
		decision := budgetTurn(t, seam, id, fmt.Sprintf("auto-%d", i), query)
		if decision.tokens > 9500 || !strings.Contains(decision.body, budgetConstraint) {
			t.Fatalf("turn %d exceeded the model window or lost early context", i)
		}
	}
	if seam.driver.summaryCount() == 0 {
		t.Fatal("zero budget disabled compaction")
	}
}

func TestE2E_Phase123_MemoryLLMBudget_CurrentInputStillGoverned(t *testing.T) {
	seam := newBudgetSeam(t, 0, 512)
	_, err := seam.stack.RunOnce(t.Context(), strings.Repeat("u", 8000), identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "oversized"}, assemble.WithRunID("oversized"))
	if !errors.Is(err, llm.ErrContextWindowExceeded) {
		t.Fatalf("oversized current input: %v", err)
	}
	if seam.driver.decision("oversized").body != "" {
		t.Fatal("provider saw an over-window request")
	}
}

func TestE2E_Phase123_MemoryLLMBudget_IdentityMandatory(t *testing.T) {
	seam := newBudgetSeam(t, 4096, 128*1024)
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1"}}
	if _, err := seam.mem.Put(context.Background(), bad, memory.ConversationTurn{UserMessage: "x", AssistantResponse: "y"}); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("memory identity: %v", err)
	}
	text := "hello"
	_, err := seam.client.Complete(context.Background(), llm.CompleteRequest{Model: "m", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}}})
	if !errors.Is(err, llm.ErrIdentityMissing) {
		t.Fatalf("Complete without identity: %v", err)
	}
}
