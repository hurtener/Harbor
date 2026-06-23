package strategy_test

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// compressingSummarizer is a deterministic test Summarizer that folds
// turns into a COMPACT marker string while threading the previous
// summary verbatim. Each folded turn adds a fixed-size marker
// (independent of the turn's size), so compaction visibly reduces the
// token estimate AND the prior summary is always a prefix of the
// result (so "never discard prior summary" is observable).
type compressingSummarizer struct{}

func (compressingSummarizer) Summarize(_ context.Context, _ identity.Quadruple, req memory.SummarizeRequest) (memory.SummarizeResponse, error) {
	var b strings.Builder
	b.WriteString(req.PreviousSummary)
	for range req.Turns {
		b.WriteString("|s")
	}
	return memory.SummarizeResponse{Summary: b.String()}, nil
}

func bigTurn(tag string, chars int) memory.ConversationTurn {
	return memory.ConversationTurn{
		UserMessage:       tag + strings.Repeat("u", chars),
		AssistantResponse: strings.Repeat("a", chars),
		Timestamp:         time.Now(),
	}
}

// TestRollingSummary_Budget_PatchNeverExceedsBudget — AC-1: with a
// positive budget the assembled patch (summary + recent turns) is
// always within the budget for any sequence of AddTurn calls, even
// when the summariser does not compress (EchoSummarizer grows the
// summary monotonically — the read-path guarantee must still clamp).
func TestRollingSummary_Budget_PatchNeverExceedsBudget(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	const budget = 200
	deps.BudgetTokens = budget
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()

	ctx := context.Background()
	id := tripleA()
	for i := range 40 {
		if err := exec.AddTurn(ctx, id, bigTurn(fmt.Sprintf("t%d-", i), 120)); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
		patch, err := exec.GetLLMContext(ctx, id)
		if err != nil {
			t.Fatalf("GetLLMContext %d: %v", i, err)
		}
		if patch.Tokens > budget {
			t.Fatalf("turn %d: patch.Tokens=%d exceeds budget=%d", i, patch.Tokens, budget)
		}
	}
}

// TestRollingSummary_Budget_ChunkedRecentCompactionPreservesSummary —
// AC-2: when the recent set alone exceeds the budget the executor folds
// oldest recent turns into the summary in chunks, threading (never
// discarding) the prior summary.
func TestRollingSummary_Budget_ChunkedRecentCompactionPreservesSummary(t *testing.T) {
	deps := buildDeps(t, compressingSummarizer{})
	// recentTurns small so the recent window alone overflows the budget
	// and forces recent-set compaction.
	deps.RecentTurns = 4
	// Each turn ≈ 22 tokens (80 chars/4 + 2 role tokens); 4 recent
	// turns ≈ 88 tokens. A 60-token budget forces folding recent turns
	// into the summary.
	const budget = 60
	deps.BudgetTokens = budget
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()

	r, ok := strategy.AsRollingSummary(exec)
	if !ok {
		t.Fatalf("AsRollingSummary: not a rolling-summary executor")
	}

	ctx := context.Background()
	id := tripleA()
	turn := func(i int) memory.ConversationTurn {
		return memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("user-message-number-%02d-padding", i),
			AssistantResponse: fmt.Sprintf("assistant-reply-number-%02d-pad", i),
		}
	}

	var prevSummary string
	sawGrowth := false
	for i := range 12 {
		if err := exec.AddTurn(ctx, id, turn(i)); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
		_, stored := r.StoredStateForTest(id)
		if prevSummary != "" {
			if !strings.HasPrefix(stored, prevSummary) {
				t.Fatalf("turn %d: stored summary %q does not preserve prior summary %q (prior content discarded)", i, stored, prevSummary)
			}
			if len(stored) > len(prevSummary) {
				sawGrowth = true
			}
		}
		prevSummary = stored

		patch, err := exec.GetLLMContext(ctx, id)
		if err != nil {
			t.Fatalf("GetLLMContext %d: %v", i, err)
		}
		if patch.Tokens > budget {
			t.Fatalf("turn %d: patch.Tokens=%d exceeds budget=%d", i, patch.Tokens, budget)
		}
	}
	if prevSummary == "" {
		t.Fatal("no summary produced — recent-set compaction never ran")
	}
	if !sawGrowth {
		t.Fatal("summary never grew across folds — chunked compaction not observed")
	}
}

// TestRollingSummary_Budget_ZeroIsUnbounded — AC-3: a zero budget
// preserves the prior unbounded behaviour (no read-path clamp, no
// compaction marker).
func TestRollingSummary_Budget_ZeroIsUnbounded(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	deps.BudgetTokens = 0
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()

	ctx := context.Background()
	id := tripleA()
	for i := range 30 {
		if err := exec.AddTurn(ctx, id, bigTurn(fmt.Sprintf("t%d-", i), 200)); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	// Unbounded: the summary accumulated far beyond any small bound and
	// was never truncated with the compaction marker.
	if patch.Tokens < 1000 {
		t.Errorf("budget=0 should accumulate unbounded; Tokens=%d unexpectedly small", patch.Tokens)
	}
	if strings.Contains(patch.Summary, "older context compacted") {
		t.Error("budget=0 must not truncate the summary (unbounded back-compat)")
	}
}

// TestRollingSummary_Budget_ReplayStaysInBudget — AC-7: replay of the
// live-found poisoning sequence (≥50 large turns into one key with a
// 32 KiB token budget) yields a within-budget assembled context, even
// with a non-compressing summariser.
func TestRollingSummary_Budget_ReplayStaysInBudget(t *testing.T) {
	deps := buildDeps(t, strategy.EchoSummarizer{})
	const budget = 32 * 1024 // 32 KiB worth of tokens
	deps.BudgetTokens = budget
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = exec.Close(context.Background()) }()

	ctx := context.Background()
	id := tripleA()
	// Each turn ≈ 1002 tokens; 60 turns ≈ 60K tokens raw, well past the
	// 32K budget, so the assembled context must compact + clamp.
	for i := range 60 {
		if err := exec.AddTurn(ctx, id, bigTurn(fmt.Sprintf("t%d-", i), 2000)); err != nil {
			t.Fatalf("AddTurn %d: %v", i, err)
		}
	}
	patch, err := exec.GetLLMContext(ctx, id)
	if err != nil {
		t.Fatalf("GetLLMContext: %v", err)
	}
	if patch.Tokens > budget {
		t.Fatalf("replay context not within budget: Tokens=%d, budget=%d", patch.Tokens, budget)
	}
	// EstimateTokens (internal, un-clamped) is allowed to exceed; the
	// emitted patch is the guarantee surface.
}

// TestRollingSummary_Budget_ConcurrentReuse — AC-8: N≥100 concurrent
// AddTurn/GetLLMContext across distinct identity keys on one shared
// executor under -race. Asserts per-key budget honoured, no cross-key
// bleed, and goroutine baseline restored after Close.
func TestRollingSummary_Budget_ConcurrentReuse(t *testing.T) {
	deps := buildDeps(t, compressingSummarizer{})
	const budget = 120
	deps.BudgetTokens = budget
	deps.RecentTurns = 3
	// Capture the baseline after the bus/state goroutines from buildDeps
	// are already running; the only goroutine New adds is the executor's
	// recovery loop, which Close must join.
	baseline := runtime.NumGoroutine()
	exec, err := strategy.New(memory.StrategyRollingSummary, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const goroutines = 128
	const turnsPerGo = 8
	var wg sync.WaitGroup
	var failures atomic.Int64
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			id := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("t-%d", g%7),
					UserID:    fmt.Sprintf("u-%d", g%13),
					SessionID: fmt.Sprintf("s-%d", g),
				},
			}
			tag := fmt.Sprintf("g%d", g)
			for i := range turnsPerGo {
				turn := memory.ConversationTurn{
					UserMessage:       fmt.Sprintf("%s-user-%02d-paddingpaddingpadding", tag, i),
					AssistantResponse: fmt.Sprintf("%s-asst-%02d-paddingpaddingpadding", tag, i),
				}
				if err := exec.AddTurn(ctx, id, turn); err != nil {
					failures.Add(1)
					return
				}
				patch, err := exec.GetLLMContext(ctx, id)
				if err != nil {
					failures.Add(1)
					return
				}
				if patch.Tokens > budget {
					failures.Add(1)
					return
				}
				// No cross-key bleed: every recent turn must belong to
				// this goroutine's key (tagged user message).
				for _, rt := range patch.RecentTurns {
					if !strings.HasPrefix(rt.UserMessage, tag+"-user-") {
						failures.Add(1)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d concurrent operations violated the budget / isolation contract", n)
	}

	if err := exec.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
