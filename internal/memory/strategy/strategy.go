// Package strategy holds the memory-strategy executors: the
// algorithmic core that any `memory.MemoryStore` driver delegates
// to. The strategy is a behaviour mode of the driver, not a
// separate driver per AGENTS.md §4.4 — every memory driver hosts
// the same executor surface.
//
// Phase 24 ships three executors:
//
//   - `none` (`noneExec`) — Strategy=none preserves the Phase 23
//     semantics: AddTurn is a no-op, GetLLMContext returns empty,
//     all snapshot bytes round-trip through `state.StateStore`.
//   - `truncation` (`truncationExec`) — synchronous recent-window
//     buffer with `OverflowDropOldest` enforcement at the configured
//     `BudgetTokens` boundary.
//   - `rolling_summary` (`rollingSummaryExec`) — recent-window
//     buffer + background-summarised long-term context. Health FSM
//     `healthy → retry → degraded → recovering → healthy` with a
//     bounded recovery backlog (`RecoveryBacklogMax`); failure paths
//     emit `memory.health_changed` and `memory.recovery_dropped`
//     events so degradation is observable, not silent (D-034).
//
// Persistence: every successful mutation lands as a
// `state.StateStore` record at `Kind = "memory.state"` (D-027 typed
// wrapper). Phase 25's SQLite + Postgres drivers will inherit the
// shape unchanged.
//
// Identity-mandatory contract (D-001): every method validates the
// identity quadruple at the boundary. Drivers OWN the boundary
// validation + the `memory.identity_rejected` emit; executors trust
// the driver's pre-validation. The driver passes through validated
// identities and the executor focuses on the algorithmic core.
//
// Concurrent-reuse contract (D-025): one executor is safe to share
// across N concurrent goroutines. Per-key state lives in mutex-
// guarded maps inside the executor; no per-call mutable state on
// the executor struct itself.
package strategy

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// StrategyExecutor is the strategy-side surface a `MemoryStore`
// driver delegates to. The shape mirrors `memory.MemoryStore` minus
// `Close` — the driver owns lifecycle. `Close` exists on the
// executor too so per-strategy resources (recovery loops, in-flight
// summariser cancellations) tear down idempotently when the driver
// is closed.
//
// The interface is unexported-by-naming (`StrategyExecutor` is
// exported because Phase 25's persistent drivers will type-assert
// it); concrete implementations are unexported types behind the
// package's `New` factory.
//
//nolint:revive // strategy.StrategyExecutor stutters but reads cleaner than strategy.Executor at call sites that already import memory.
type StrategyExecutor interface {
	AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error
	GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error)
	EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error)
	Flush(ctx context.Context, id identity.Quadruple) error
	Health(ctx context.Context, id identity.Quadruple) (memory.Health, error)
	Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error)
	Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error
	Close(ctx context.Context) error
}

// Deps carries the runtime dependencies an executor consumes.
//
// `State` is the persistence floor — every executor writes typed
// records through it (D-027). Mandatory for every strategy
// including `none` (the wiring is exercised by Phase 23's
// conformance suite).
//
// `Bus` is the event bus the executor publishes lifecycle events
// on — `memory.identity_rejected` (driver-side; passed through
// here so executors can re-emit if needed), `memory.health_changed`
// (rolling-summary FSM transitions), `memory.recovery_dropped`
// (bounded-backlog overflow). Mandatory.
//
// `Summarizer` is the injectable LLM-edge callable the
// rolling-summary strategy consumes. Mandatory for
// `StrategyRollingSummary`; ignored by other strategies. The
// LLM-backed implementation lands at Phase 32+; Phase 24 ships a
// test-grade stub (`EchoSummarizer`).
//
// `BudgetTokens` is the truncation / rolling-summary budget cap
// (per-key, applied as token estimates). Zero is honoured as "no
// budget" — appending is unbounded. Positive values enforce the
// `OverflowDropOldest` policy.
//
// `RecoveryBacklogMax` is the bounded queue size for the
// rolling-summary recovery loop. Zero defaults to
// `DefaultRecoveryBacklogMax` (16).
type Deps struct {
	State              state.StateStore
	Bus                events.EventBus
	Summarizer         memory.Summarizer
	BudgetTokens       int
	RecoveryBacklogMax int
}

// DefaultRecoveryBacklogMax is the default recovery-backlog cap for
// the rolling-summary strategy when `Deps.RecoveryBacklogMax` is
// zero. Sized to absorb a short summariser outage (≈4 minutes at
// `defaultDegradedRetryEvery = 10s` × 16 retries) without
// unbounded memory growth.
const DefaultRecoveryBacklogMax = 16

// FullZoneTurns is the recent-window size before turns spill into
// the rolling-summary `pending` queue. Constant per D-034 (the
// brief 04 §2 knob is encoded as a constant; an operator who needs
// to tune it files an RFC PR rather than fighting yaml).
const FullZoneTurns = 4

// New constructs the strategy executor for the given strategy.
// Unknown strategies return wrapped
// `memory.ErrStrategyNotImplemented`.
//
// Phase 24 routes:
//
//	StrategyNone           → noneExec (no-op surface; preserves Phase 23 semantics)
//	StrategyTruncation     → truncationExec
//	StrategyRollingSummary → rollingSummaryExec
//
// A nil `Deps.State` or `Deps.Bus` returns a wrapped construction
// error. A nil `Deps.Summarizer` is rejected for
// `StrategyRollingSummary` and ignored for the others.
func New(s memory.Strategy, deps Deps) (StrategyExecutor, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("memory/strategy: Deps.State is required")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/strategy: Deps.Bus is required")
	}
	if deps.RecoveryBacklogMax < 0 {
		return nil, fmt.Errorf("memory/strategy: Deps.RecoveryBacklogMax must be >= 0, got %d", deps.RecoveryBacklogMax)
	}
	switch s {
	case memory.StrategyNone, "":
		return newNoneExec(deps), nil
	case memory.StrategyTruncation:
		return newTruncationExec(deps), nil
	case memory.StrategyRollingSummary:
		if deps.Summarizer == nil {
			return nil, fmt.Errorf("memory/strategy: Deps.Summarizer is required for %q", s)
		}
		return newRollingSummaryExec(deps), nil
	default:
		return nil, fmt.Errorf("%w: %q", memory.ErrStrategyNotImplemented, s)
	}
}

// kindMemoryState is the StateStore Kind constant for memory-state
// records. Centralised so every executor references one symbol;
// matches the Phase 23 inmem driver's constant verbatim.
const kindMemoryState = "memory.state"
