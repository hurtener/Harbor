// Package conformancetest exposes the canonical correctness suite
// every `memory.MemoryStore` driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/memory` does not import the standard library `testing`
// package (precedent: `internal/identity/conformancetest`,
// `internal/state/conformancetest`).
//
// Downstream drivers (SQLite + Postgres) consume it via:
//
//	import "github.com/hurtener/Harbor/internal/memory/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() conformancetest.Harness {
//	        // ... build store + bus + cleanup ...
//	    })
//	}
//
// The factory returns a fresh `Harness` per top-level subtest. The
// `Harness.Bus` field is the bus the driver was wired to so the
// test can subscribe and assert audit emits without injecting a
// fake. The cleanup closure releases everything.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
)

// Harness bundles the per-subtest fixture.
//
//   - `Store` is the MemoryStore under test, freshly constructed.
//   - `Bus` is the EventBus the Store was wired to so the test can
//     Subscribe and observe audit emits without injecting a fake.
//   - `Strategy` is the strategy the Store is configured for; the
//     suite forks its strategy-specific subtests on this value.
//     Defaults to `StrategyNone` when zero — preserving the original
//     callers (which had no Strategy field on Harness).
//   - `Cleanup` releases the harness (store.Close + bus.Close +
//     any caller-owned tear-down).
//
// Drivers MUST register their bus subscription identity to a fixed
// triple under their control; the suite uses a known identity for
// happy-path assertions and a partial / empty identity to drive
// rejection paths.
//   - `Retrieval` is the retrieval mode the Store is configured
//     for. Zero (the default mode) makes the suite assert
//     `SearchTurns` fails loudly with `ErrSemanticDisabled`;
//     `RetrievalSemantic` forks the semantic-retrieval subtests
//     (similarity ranking, vector isolation, flush). A semantic
//     harness must be wired with a deterministic embedder (e.g.
//     `embeddingstest.New()`).
type Harness struct {
	Store     memory.MemoryStore
	Bus       events.EventBus
	Strategy  memory.Strategy
	Retrieval memory.RetrievalMode
	// BudgetTokens is the per-key token budget the Store was wired
	// with. When > 0 the strategy-budget subtests assert the assembled
	// GetLLMContext patch never exceeds it. Zero skips those assertions
	// (a harness wired with no budget).
	BudgetTokens int
	Cleanup      func()
}

// strategy returns the configured strategy, defaulting to
// `StrategyNone` when zero (compatibility).
func (h Harness) strategy() memory.Strategy {
	if h.Strategy == "" {
		return memory.StrategyNone
	}
	return h.Strategy
}

// Factory builds a fresh Harness. Called once per top-level
// subtest; the test invokes Cleanup() when it returns.
type Factory func() Harness

// Run executes the canonical correctness suite. Subtests are
// strategy-aware via `Harness.Strategy`: identity / cross-isolation
// / Close / lifecycle subtests run unconditionally, while
// strategy-specific subtests fork on the configured strategy.
//
// Strategy-agnostic subtests:
//
//   - GetLLMContext_EmptyByDefault          (Strategy on the patch matches the configured one)
//   - Flush_NoOp                            (Flush succeeds on a fresh store)
//   - Health_ReturnsHealthy                 (initial state is healthy for every strategy)
//   - Snapshot_Empty                        (initial snapshot is consistent with the strategy)
//   - Restore_RoundTripsEmptySnapshot
//   - Identity_Mandatory_AddTurn
//   - Identity_Mandatory_GetLLMContext
//   - Identity_Mandatory_AllMethods
//   - CrossTenant_Isolation
//   - CrossSession_Isolation
//   - Concurrent_AllMethods_NoRace
//   - Close_Idempotent
//   - AfterClose_OperationsError
//
// Strategy=none-specific subtests:
//
//   - None_AddTurn_IsNoOp
//   - None_EstimateTokens_IsZero
//   - None_Restore_RejectsNonEmpty
//
// Strategy=truncation-specific subtests:
//
//   - Truncation_AddTurn_PersistsAndAccumulates
//   - Truncation_EnforcesBudget
//   - Truncation_SnapshotRestore_RoundTripsBuffer
//
// Strategy=rolling_summary-specific subtests:
//
//   - RollingSummary_AddTurn_TriggersSummarisation
//   - RollingSummary_SnapshotRestore_RoundTripsSummary
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("GetLLMContext_EmptyByDefault", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		patch, err := h.Store.GetLLMContext(context.Background(), tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Strategy != h.strategy() {
			t.Errorf("patch.Strategy=%q, want %q", patch.Strategy, h.strategy())
		}
		// On a fresh store, no turns have been added → the patch
		// has no recent turns / summary / tokens regardless of
		// strategy.
		if patch.Tokens != 0 || patch.Summary != "" || len(patch.RecentTurns) != 0 {
			t.Errorf("non-empty patch on fresh store under %q: %+v", h.strategy(), patch)
		}
	})

	// Strategy=none-specific subtests.
	t.Run("None_AddTurn_IsNoOp", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyNone {
			t.Skipf("subtest only runs under StrategyNone; got %q", h.strategy())
		}
		ctx := context.Background()
		if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
			t.Fatalf("AddTurn: %v", err)
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Tokens != 0 || patch.Summary != "" || len(patch.RecentTurns) != 0 {
			t.Errorf("Strategy=none returned non-empty patch: %+v", patch)
		}
	})

	t.Run("None_EstimateTokens_IsZero", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyNone {
			t.Skipf("subtest only runs under StrategyNone; got %q", h.strategy())
		}
		got, err := h.Store.EstimateTokens(context.Background(), tripleA())
		if err != nil {
			t.Fatalf("EstimateTokens: %v", err)
		}
		if got != 0 {
			t.Errorf("EstimateTokens=%d, want 0", got)
		}
	})

	// Strategy=truncation-specific subtests.
	t.Run("Truncation_AddTurn_PersistsAndAccumulates", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyTruncation {
			t.Skipf("subtest only runs under StrategyTruncation; got %q", h.strategy())
		}
		ctx := context.Background()
		for i := range 3 {
			if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if len(patch.RecentTurns) == 0 {
			t.Error("truncation returned no recent turns after 3 AddTurns")
		}
		if patch.Tokens == 0 {
			t.Error("truncation returned zero tokens after 3 AddTurns")
		}
	})

	t.Run("Truncation_EnforcesBudget", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyTruncation {
			t.Skipf("subtest only runs under StrategyTruncation; got %q", h.strategy())
		}
		ctx := context.Background()
		bigTurn := memory.ConversationTurn{
			UserMessage:       strings.Repeat("u", 200),
			AssistantResponse: strings.Repeat("a", 200),
		}
		// Push enough turns to overflow the test budget (64 tokens
		// in newHarness); buffer must drop oldest to fit.
		for i := range 5 {
			if err := h.Store.AddTurn(ctx, tripleA(), bigTurn); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Tokens > 100 {
			t.Errorf("truncation did not enforce budget: Tokens=%d, want <= 100", patch.Tokens)
		}
	})

	t.Run("Truncation_SnapshotRestore_RoundTripsBuffer", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyTruncation {
			t.Skipf("subtest only runs under StrategyTruncation; got %q", h.strategy())
		}
		ctx := context.Background()
		if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
			t.Fatalf("AddTurn: %v", err)
		}
		snap, err := h.Store.Snapshot(ctx, tripleA())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := h.Store.Restore(ctx, tripleA(), snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext after restore: %v", err)
		}
		if len(patch.RecentTurns) == 0 {
			t.Error("restored truncation snapshot lost recent turns")
		}
	})

	// Strategy=rolling_summary-specific subtests.
	t.Run("RollingSummary_AddTurn_TriggersSummarisation", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyRollingSummary {
			t.Skipf("subtest only runs under StrategyRollingSummary; got %q", h.strategy())
		}
		ctx := context.Background()
		// Add enough turns to spill into pending and trigger
		// summarisation (FullZoneTurns is 4 inside the strategy
		// package; 6 AddTurns guarantees spillage).
		for i := range 6 {
			if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
		}
		// The stub Summarizer (EchoSummarizer) runs inline, so the
		// summary is observable immediately on the next GetLLMContext.
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Summary == "" {
			t.Error("rolling_summary failed to produce a summary after 6 turns")
		}
	})

	t.Run("RollingSummary_EnforcesBudget", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyRollingSummary {
			t.Skipf("subtest only runs under StrategyRollingSummary; got %q", h.strategy())
		}
		if h.BudgetTokens <= 0 {
			t.Skip("harness wired with no budget; skipping budget assertion")
		}
		ctx := context.Background()
		// Each turn fits the budget individually (~32 tokens) but the
		// accumulation (recent window + rolling summary) blows past it;
		// the assembled context must stay within budget via compaction +
		// the read-path guarantee — every driver honours this with parity.
		big := memory.ConversationTurn{
			UserMessage:       strings.Repeat("u", 60),
			AssistantResponse: strings.Repeat("a", 60),
		}
		for i := range 12 {
			if err := h.Store.AddTurn(ctx, tripleA(), big); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
			patch, err := h.Store.GetLLMContext(ctx, tripleA())
			if err != nil {
				t.Fatalf("GetLLMContext %d: %v", i, err)
			}
			if patch.Tokens > h.BudgetTokens {
				t.Fatalf("turn %d: rolling_summary did not enforce budget: Tokens=%d, want <= %d", i, patch.Tokens, h.BudgetTokens)
			}
		}
	})

	t.Run("RollingSummary_SnapshotRestore_RoundTripsSummary", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyRollingSummary {
			t.Skipf("subtest only runs under StrategyRollingSummary; got %q", h.strategy())
		}
		ctx := context.Background()
		for i := range 6 {
			if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
		}
		snap, err := h.Store.Snapshot(ctx, tripleA())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := h.Store.Restore(ctx, tripleA(), snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext after restore: %v", err)
		}
		if patch.Summary == "" {
			t.Error("rolling_summary restore lost the summary")
		}
	})

	t.Run("Flush_NoOp", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		if err := h.Store.Flush(ctx, tripleA()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		// And idempotent: a second Flush on a never-touched memory is also fine.
		if err := h.Store.Flush(ctx, tripleA()); err != nil {
			t.Fatalf("Flush 2: %v", err)
		}
	})

	t.Run("Flush_LeavesNoResidualContext", func(t *testing.T) {
		// The session-erasure gate (§17.6): after a Flush the
		// memory MUST be reset to a clean state — GetLLMContext returns no
		// recent turns / empty summary / zero tokens, and (for a semantic
		// store) SearchTurns finds no residual embedded turns. Runs for
		// EVERY strategy / retrieval mode: StrategyNone is a trivially-clean
		// no-op, and the persisting strategies must prove Flush genuinely
		// purges. A driver whose Flush leaves retrievable residue fails
		// HERE — and is fixed in the same phase that found it.
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		// Seed a few turns (a no-op under StrategyNone; persisted under the
		// truncation / rolling-summary strategies).
		for range 3 {
			if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
				t.Fatalf("AddTurn: %v", err)
			}
		}
		if err := h.Store.Flush(ctx, tripleA()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext after Flush: %v", err)
		}
		if len(patch.RecentTurns) != 0 {
			t.Errorf("Flush left %d recent turn(s) in the LLM context", len(patch.RecentTurns))
		}
		if patch.Summary != "" {
			t.Errorf("Flush left a residual summary: %q", patch.Summary)
		}
		if patch.Tokens != 0 {
			t.Errorf("Flush left %d residual tokens in the LLM context", patch.Tokens)
		}
		if h.Retrieval == memory.RetrievalSemantic {
			got, serr := h.Store.SearchTurns(ctx, tripleA(), sampleTurn().UserMessage, 5)
			if serr != nil {
				t.Fatalf("SearchTurns after Flush: %v", serr)
			}
			if len(got) != 0 {
				t.Errorf("Flush left %d embedded turn(s) retrievable by SearchTurns", len(got))
			}
		}
	})

	t.Run("Health_ReturnsHealthy", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		got, err := h.Store.Health(context.Background(), tripleA())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if got != memory.HealthHealthy {
			t.Errorf("Health=%q, want %q", got, memory.HealthHealthy)
		}
	})

	t.Run("Snapshot_Empty", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		got, err := h.Store.Snapshot(context.Background(), tripleA())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if got.Strategy != h.strategy() {
			t.Errorf("snapshot strategy=%q, want %q", got.Strategy, h.strategy())
		}
		// Bytes may be empty (never-written) or carry an empty JSON
		// record; either is acceptable on a fresh store — but the
		// snapshot MUST round-trip through Restore.
	})

	t.Run("Restore_RoundTripsEmptySnapshot", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		// Empty snapshot — should always be accepted.
		if err := h.Store.Restore(ctx, tripleA(), memory.Snapshot{}); err != nil {
			t.Fatalf("Restore empty: %v", err)
		}
		// Snapshot then Restore the same bytes round-trips.
		snap, err := h.Store.Snapshot(ctx, tripleA())
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if err := h.Store.Restore(ctx, tripleA(), snap); err != nil {
			t.Fatalf("Restore round-trip: %v", err)
		}
	})

	t.Run("None_Restore_RejectsNonEmpty", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.strategy() != memory.StrategyNone {
			t.Skipf("subtest only runs under StrategyNone; got %q", h.strategy())
		}
		ctx := context.Background()
		bogus := memory.Snapshot{
			Strategy: memory.StrategyNone,
			Bytes:    []byte(`{"strategy":"none","turns":[{"user_message":"x"}]}`),
		}
		err := h.Store.Restore(ctx, tripleA(), bogus)
		if !errors.Is(err, memory.ErrInvalidSnapshot) {
			t.Fatalf("Restore non-empty: err=%v, want ErrInvalidSnapshot", err)
		}
		// And a mismatched-strategy snapshot is also rejected.
		mismatched := memory.Snapshot{
			Strategy: memory.StrategyTruncation,
			Bytes:    []byte(`{"strategy":"truncation"}`),
		}
		err = h.Store.Restore(ctx, tripleA(), mismatched)
		if !errors.Is(err, memory.ErrInvalidSnapshot) {
			t.Fatalf("Restore mismatched strategy: err=%v, want ErrInvalidSnapshot", err)
		}
	})

	t.Run("Identity_Mandatory_AddTurn", func(t *testing.T) {
		assertIdentityMandatory(t, factory, "AddTurn", func(s memory.MemoryStore, q identity.Quadruple) error {
			return s.AddTurn(context.Background(), q, sampleTurn())
		})
	})

	t.Run("Identity_Mandatory_GetLLMContext", func(t *testing.T) {
		assertIdentityMandatory(t, factory, "GetLLMContext", func(s memory.MemoryStore, q identity.Quadruple) error {
			_, err := s.GetLLMContext(context.Background(), q)
			return err
		})
	})

	t.Run("Identity_Mandatory_AllMethods", func(t *testing.T) {
		methods := map[string]func(memory.MemoryStore, identity.Quadruple) error{
			"AddTurn": func(s memory.MemoryStore, q identity.Quadruple) error {
				return s.AddTurn(context.Background(), q, sampleTurn())
			},
			"GetLLMContext": func(s memory.MemoryStore, q identity.Quadruple) error {
				_, err := s.GetLLMContext(context.Background(), q)
				return err
			},
			"EstimateTokens": func(s memory.MemoryStore, q identity.Quadruple) error {
				_, err := s.EstimateTokens(context.Background(), q)
				return err
			},
			"Flush": func(s memory.MemoryStore, q identity.Quadruple) error {
				return s.Flush(context.Background(), q)
			},
			"Health": func(s memory.MemoryStore, q identity.Quadruple) error {
				_, err := s.Health(context.Background(), q)
				return err
			},
			"Snapshot": func(s memory.MemoryStore, q identity.Quadruple) error {
				_, err := s.Snapshot(context.Background(), q)
				return err
			},
			"Restore": func(s memory.MemoryStore, q identity.Quadruple) error {
				return s.Restore(context.Background(), q, memory.Snapshot{})
			},
			"SearchTurns": func(s memory.MemoryStore, q identity.Quadruple) error {
				_, err := s.SearchTurns(context.Background(), q, "probe", 3)
				return err
			},
		}
		for name, op := range methods {
			t.Run(name, func(t *testing.T) {
				assertIdentityMandatory(t, factory, name, op)
			})
		}
	})

	t.Run("CrossTenant_Isolation", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		// Snapshot under tenant A; Restore under tenant B reads
		// tenant B's empty slot (the StateStore key is the full
		// triple, so the two never see each other).
		snapA, err := h.Store.Snapshot(ctx, tripleA())
		if err != nil {
			t.Fatalf("Snapshot A: %v", err)
		}
		snapB, err := h.Store.Snapshot(ctx, tripleB())
		if err != nil {
			t.Fatalf("Snapshot B: %v", err)
		}
		// At Strategy=none, both snapshots are "empty" semantically
		// but the StateStore layer separates them. Restore tenant A's
		// snapshot under tenant A; reading B's slot must NOT find A's
		// record.
		if err := h.Store.Restore(ctx, tripleA(), snapA); err != nil {
			t.Fatalf("Restore A: %v", err)
		}
		// Snapshot B again — must still be empty / consistent with the
		// initial snapshot taken before A wrote.
		snapBAgain, err := h.Store.Snapshot(ctx, tripleB())
		if err != nil {
			t.Fatalf("Snapshot B again: %v", err)
		}
		// Strategy=none: both snapshots have Strategy=none. Bytes
		// equivalence (both "empty-after-write" OR both untouched) is
		// the cross-tenant isolation assertion.
		if snapBAgain.Strategy != snapB.Strategy {
			t.Errorf("Tenant B strategy bleed: before=%q after-A-write=%q", snapB.Strategy, snapBAgain.Strategy)
		}
	})

	t.Run("CrossSession_Isolation", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		q1 := identity.Quadruple{
			Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S1"},
		}
		q2 := identity.Quadruple{
			Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S2"},
		}
		// Restore against S1 must not appear in S2's Snapshot.
		if err := h.Store.Restore(ctx, q1, memory.Snapshot{}); err != nil {
			t.Fatalf("Restore S1: %v", err)
		}
		snap2, err := h.Store.Snapshot(ctx, q2)
		if err != nil {
			t.Fatalf("Snapshot S2: %v", err)
		}
		// S2 hasn't been written; its snapshot is the empty /
		// initial-state default for the configured strategy,
		// regardless of S1's restore.
		if snap2.Strategy != h.strategy() {
			t.Errorf("session 2 leaked strategy: %q (want %q)", snap2.Strategy, h.strategy())
		}
	})

	t.Run("Concurrent_AllMethods_NoRace", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 128
		const opsPerGo = 8

		var wg sync.WaitGroup
		var errCount atomic.Int64
		wg.Add(goroutines)
		for i := range goroutines {

			go func() {
				defer wg.Done()
				ctx := context.Background()
				ident := identity.Quadruple{
					Identity: identity.Identity{
						TenantID:  fmt.Sprintf("t-%d", i%17),
						UserID:    fmt.Sprintf("u-%d", i%41),
						SessionID: fmt.Sprintf("s-%d", i),
					},
				}
				for j := range opsPerGo {
					switch j % 8 {
					case 0:
						if err := h.Store.AddTurn(ctx, ident, sampleTurn()); err != nil {
							errCount.Add(1)
						}
					case 1:
						if _, err := h.Store.GetLLMContext(ctx, ident); err != nil {
							errCount.Add(1)
						}
					case 2:
						if _, err := h.Store.EstimateTokens(ctx, ident); err != nil {
							errCount.Add(1)
						}
					case 3:
						if err := h.Store.Flush(ctx, ident); err != nil {
							errCount.Add(1)
						}
					case 4:
						if _, err := h.Store.Health(ctx, ident); err != nil {
							errCount.Add(1)
						}
					case 5:
						if _, err := h.Store.Snapshot(ctx, ident); err != nil {
							errCount.Add(1)
						}
					case 6:
						if err := h.Store.Restore(ctx, ident, memory.Snapshot{}); err != nil {
							errCount.Add(1)
						}
					case 7:
						// `ErrSemanticDisabled` is the documented
						// answer under the default retrieval mode —
						// only unexpected failures count.
						if _, err := h.Store.SearchTurns(ctx, ident, "probe", 3); err != nil &&
							!errors.Is(err, memory.ErrSemanticDisabled) {
							errCount.Add(1)
						}
					}
				}
			}()
		}
		wg.Wait()
		if n := errCount.Load(); n != 0 {
			t.Fatalf("%d concurrent operations errored", n)
		}

		// Wait briefly for any internal goroutines to wind down before
		// asserting baseline restoration (no time.Sleep for sync — bounded
		// real-time deadline + Gosched yields, same pattern as the state suite).
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	t.Run("Close_Idempotent", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		if err := h.Store.Close(ctx); err != nil {
			t.Fatalf("Close 1: %v", err)
		}
		if err := h.Store.Close(ctx); err != nil {
			t.Fatalf("Close 2 (idempotent): %v", err)
		}
	})

	t.Run("AfterClose_OperationsError", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		ctx := context.Background()
		if err := h.Store.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		err := h.Store.AddTurn(ctx, tripleA(), sampleTurn())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("AddTurn after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = h.Store.GetLLMContext(ctx, tripleA())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("GetLLMContext after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = h.Store.EstimateTokens(ctx, tripleA())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("EstimateTokens after Close: err=%v, want ErrStoreClosed", err)
		}
		err = h.Store.Flush(ctx, tripleA())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("Flush after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = h.Store.Health(ctx, tripleA())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("Health after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = h.Store.Snapshot(ctx, tripleA())
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("Snapshot after Close: err=%v, want ErrStoreClosed", err)
		}
		err = h.Store.Restore(ctx, tripleA(), memory.Snapshot{})
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("Restore after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = h.Store.SearchTurns(ctx, tripleA(), "probe", 3)
		if !errors.Is(err, memory.ErrStoreClosed) {
			t.Errorf("SearchTurns after Close: err=%v, want ErrStoreClosed", err)
		}
	})

	// Retrieval-mode subtests. The default mode must FAIL LOUDLY on
	// SearchTurns; the semantic mode must rank, isolate, and flush.
	t.Run("SearchTurns_DisabledFailsLoudly", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.Retrieval == memory.RetrievalSemantic {
			t.Skipf("subtest only runs under the default retrieval mode; got %q", h.Retrieval)
		}
		_, err := h.Store.SearchTurns(context.Background(), tripleA(), "anything", 3)
		if !errors.Is(err, memory.ErrSemanticDisabled) {
			t.Fatalf("SearchTurns under default mode: err=%v, want ErrSemanticDisabled", err)
		}
	})

	t.Run("Semantic_SearchTurns_RanksBySimilarity", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.Retrieval != memory.RetrievalSemantic {
			t.Skipf("subtest only runs under RetrievalSemantic; got %q", h.Retrieval)
		}
		ctx := context.Background()
		turns := []memory.ConversationTurn{
			{UserMessage: "how do I dock the boat at the harbor pier", AssistantResponse: "tie the boat to the pier cleats"},
			{UserMessage: "what is the capital of France", AssistantResponse: "Paris is the capital of France"},
			{UserMessage: "bake a chocolate cake recipe", AssistantResponse: "mix flour sugar cocoa and bake"},
		}
		for i, turn := range turns {
			if err := h.Store.AddTurn(ctx, tripleA(), turn); err != nil {
				t.Fatalf("AddTurn %d: %v", i, err)
			}
		}
		got, err := h.Store.SearchTurns(ctx, tripleA(), "dock a boat at the pier", 2)
		if err != nil {
			t.Fatalf("SearchTurns: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("SearchTurns returned %d results, want 2", len(got))
		}
		if got[0].Turn.UserMessage != turns[0].UserMessage {
			t.Errorf("top result = %q, want the boat/pier turn", got[0].Turn.UserMessage)
		}
		if got[0].Score < got[1].Score {
			t.Errorf("results not ranked: scores %v then %v", got[0].Score, got[1].Score)
		}
		// The strategy patch is UNCHANGED by the semantic mode —
		// composition, not replacement.
		patch, err := h.Store.GetLLMContext(ctx, tripleA())
		if err != nil {
			t.Fatalf("GetLLMContext: %v", err)
		}
		if patch.Strategy != h.strategy() {
			t.Errorf("semantic mode changed the patch strategy: %q, want %q", patch.Strategy, h.strategy())
		}
	})

	t.Run("Semantic_CrossSession_Isolation", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.Retrieval != memory.RetrievalSemantic {
			t.Skipf("subtest only runs under RetrievalSemantic; got %q", h.Retrieval)
		}
		ctx := context.Background()
		q1 := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S1"}}
		q2 := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S2"}}
		if err := h.Store.AddTurn(ctx, q1, memory.ConversationTurn{
			UserMessage:       "secret topic only session one knows",
			AssistantResponse: "indeed, session one's secret",
		}); err != nil {
			t.Fatalf("AddTurn S1: %v", err)
		}
		got, err := h.Store.SearchTurns(ctx, q2, "secret topic only session one knows", 5)
		if err != nil {
			t.Fatalf("SearchTurns S2: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("session 2 retrieved %d turn(s) embedded under session 1 — vector isolation breach", len(got))
		}
		// And a cross-TENANT probe for the same vectors.
		qT := identity.Quadruple{Identity: identity.Identity{TenantID: "T2", UserID: "U", SessionID: "S1"}}
		got, err = h.Store.SearchTurns(ctx, qT, "secret topic only session one knows", 5)
		if err != nil {
			t.Fatalf("SearchTurns tenant B: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("tenant B retrieved %d turn(s) embedded under tenant A — vector isolation breach", len(got))
		}
	})

	t.Run("Semantic_Flush_DropsVectors", func(t *testing.T) {
		h := factory()
		defer h.Cleanup()
		if h.Retrieval != memory.RetrievalSemantic {
			t.Skipf("subtest only runs under RetrievalSemantic; got %q", h.Retrieval)
		}
		ctx := context.Background()
		if err := h.Store.AddTurn(ctx, tripleA(), sampleTurn()); err != nil {
			t.Fatalf("AddTurn: %v", err)
		}
		if err := h.Store.Flush(ctx, tripleA()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		got, err := h.Store.SearchTurns(ctx, tripleA(), sampleTurn().UserMessage, 5)
		if err != nil {
			t.Fatalf("SearchTurns after Flush: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("Flush left %d vector entr(ies) behind", len(got))
		}
	})
}

// assertIdentityMandatory exercises the fail-closed contract on a
// single store method: empty tenant / user / session each must
// return wrapped `ErrIdentityRequired` AND emit one
// `memory.identity_rejected` event on the bus.
//
// Per-case isolation: each iteration builds a fresh harness so the
// bus subscription only sees events for the current case.
func assertIdentityMandatory(
	t *testing.T,
	factory Factory,
	operation string,
	op func(memory.MemoryStore, identity.Quadruple) error,
) {
	t.Helper()
	cases := map[string]identity.Quadruple{
		"empty_all": {},
		"empty_tenant": {
			Identity: identity.Identity{UserID: "U", SessionID: "S"},
		},
		"empty_user": {
			Identity: identity.Identity{TenantID: "T", SessionID: "S"},
		},
		"empty_session": {
			Identity: identity.Identity{TenantID: "T", UserID: "U"},
		},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			h := factory()
			defer h.Cleanup()

			// Subscribe with Admin: true so the cross-tenant /
			// missing-triple emit reaches the subscription. The
			// emit-site sentinel "<missing>" substitutes any empty
			// component on the published event so ValidateEvent's
			// own triple check passes.
			sub, err := h.Bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{memory.EventTypeMemoryIdentityRejected},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			err = op(h.Store, q)
			if !errors.Is(err, memory.ErrIdentityRequired) {
				t.Fatalf("operation=%s case=%s: err=%v, want ErrIdentityRequired", operation, name, err)
			}

			// One event observable on the bus, within a short deadline.
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					t.Fatalf("operation=%s case=%s: subscription channel closed before rejection event", operation, name)
				}
				if ev.Type != memory.EventTypeMemoryIdentityRejected {
					t.Errorf("operation=%s case=%s: event type=%q, want %q",
						operation, name, ev.Type, memory.EventTypeMemoryIdentityRejected)
				}
				payload, ok := ev.Payload.(memory.MemoryIdentityRejectedPayload)
				if !ok {
					t.Errorf("operation=%s case=%s: payload type=%T, want MemoryIdentityRejectedPayload", operation, name, ev.Payload)
					return
				}
				if payload.Operation != operation {
					t.Errorf("operation=%s case=%s: payload.Operation=%q, want %q",
						operation, name, payload.Operation, operation)
				}
				if payload.Reason == "" {
					t.Errorf("operation=%s case=%s: payload.Reason empty", operation, name)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("operation=%s case=%s: timed out waiting for rejection event", operation, name)
			}
		})
	}
}

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
		RunID:    "run-1",
	}
}

func tripleB() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-B", UserID: "user-9", SessionID: "sess-9"},
		RunID:    "run-9",
	}
}

func sampleTurn() memory.ConversationTurn {
	return memory.ConversationTurn{
		UserMessage:       "hello",
		AssistantResponse: "world",
		Timestamp:         time.Now(),
	}
}
