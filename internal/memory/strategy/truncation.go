package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// truncationExec implements `StrategyTruncation`: a synchronous
// recent-window buffer keyed per `identity.Quadruple`, with
// `OverflowDropOldest` enforcement at the configured `BudgetTokens`
// boundary. No background goroutines; no summariser; no health
// FSM (always reports `HealthHealthy`).
//
// Concurrent-reuse contract (D-025): per-key state lives in
// `keys`, a sync.Map of `*keyState` values; each `keyState` has its
// own mutex so concurrent ops on different keys don't contend.
type truncationExec struct {
	state        state.StateStore
	bus          events.EventBus
	budgetTokens int

	// keys is the per-identity buffer store. The map is internally
	// synchronised; per-key mutexes are inside `keyState` so two
	// goroutines on different keys never block each other.
	keys sync.Map // map[quadKey]*truncationKeyState

	// closed gates further operations after `Close` (driver-level
	// idempotency).
	mu     sync.Mutex
	closed bool
}

// truncationKeyState is the per-`(identity.Quadruple)` mutable state
// the truncation strategy maintains.
type truncationKeyState struct {
	mu      sync.Mutex
	turns   []memory.ConversationTurn
	loaded  bool // true once we've attempted to load from StateStore
}

func newTruncationExec(deps Deps) *truncationExec {
	return &truncationExec{
		state:        deps.State,
		bus:          deps.Bus,
		budgetTokens: deps.BudgetTokens,
	}
}

func (e *truncationExec) keyState(id identity.Quadruple) *truncationKeyState {
	k := quadKeyFor(id)
	if v, ok := e.keys.Load(k); ok {
		return v.(*truncationKeyState)
	}
	fresh := &truncationKeyState{}
	actual, _ := e.keys.LoadOrStore(k, fresh)
	return actual.(*truncationKeyState)
}

// loadIfNeeded fills the per-key buffer from StateStore on first
// access. Subsequent calls are no-ops (the in-memory buffer is the
// source of truth once loaded).
func (e *truncationExec) loadIfNeeded(ctx context.Context, ks *truncationKeyState, id identity.Quadruple) error {
	if ks.loaded {
		return nil
	}
	rec, err := loadRecord(ctx, e.state, id)
	if err != nil {
		return err
	}
	// Empty record (or none-strategy record) → start fresh.
	if rec.Strategy == "" || rec.Strategy == memory.StrategyNone {
		ks.turns = nil
	} else if rec.Strategy == memory.StrategyTruncation {
		ks.turns = rec.Turns
	} else {
		// Cross-strategy load: don't clobber the persisted record;
		// start an empty in-memory buffer so the executor's writes
		// land cleanly on the next persist.
		ks.turns = nil
	}
	ks.loaded = true
	return nil
}

func (e *truncationExec) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return err
	}
	ks.turns = append(ks.turns, turn)
	e.enforceBudget(ks)
	return persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyTruncation,
		Turns:    ks.turns,
	})
}

func (e *truncationExec) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if e.isClosed() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return memory.LLMContextPatch{}, err
	}
	// Copy the slice so the caller can't mutate executor state.
	out := make([]memory.ConversationTurn, len(ks.turns))
	copy(out, ks.turns)
	return memory.LLMContextPatch{
		Strategy:    memory.StrategyTruncation,
		RecentTurns: out,
		Tokens:      sumTokens(ks.turns),
	}, nil
}

func (e *truncationExec) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if e.isClosed() {
		return 0, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return 0, err
	}
	return sumTokens(ks.turns), nil
}

func (e *truncationExec) Flush(ctx context.Context, id identity.Quadruple) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.turns = nil
	ks.loaded = true // we know it's empty now
	if err := e.state.Delete(ctx, id, kindMemoryState); err != nil {
		return fmt.Errorf("memory/strategy/truncation: Flush delete: %w", err)
	}
	return nil
}

func (e *truncationExec) Health(_ context.Context, _ identity.Quadruple) (memory.Health, error) {
	if e.isClosed() {
		return "", memory.ErrStoreClosed
	}
	return memory.HealthHealthy, nil
}

func (e *truncationExec) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if e.isClosed() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if err := e.loadIfNeeded(ctx, ks, id); err != nil {
		return memory.Snapshot{}, err
	}
	rec := memoryStateRecord{
		Strategy: memory.StrategyTruncation,
		Turns:    ks.turns,
	}
	bytes, err := json.Marshal(rec)
	if err != nil {
		return memory.Snapshot{}, fmt.Errorf("memory/strategy/truncation: marshal: %w", err)
	}
	return memory.Snapshot{Strategy: memory.StrategyTruncation, Bytes: bytes}, nil
}

func (e *truncationExec) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if e.isClosed() {
		return memory.ErrStoreClosed
	}
	ks := e.keyState(id)
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if snap.IsEmpty() {
		ks.turns = nil
		ks.loaded = true
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyTruncation})
	}
	if snap.Strategy != memory.StrategyTruncation {
		return fmt.Errorf("%w: snapshot strategy=%q, executor strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, memory.StrategyTruncation)
	}
	if len(snap.Bytes) == 0 {
		ks.turns = nil
		ks.loaded = true
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyTruncation})
	}
	var rec memoryStateRecord
	if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrInvalidSnapshot, err)
	}
	if rec.Strategy != memory.StrategyTruncation {
		return fmt.Errorf("%w: record strategy=%q", memory.ErrInvalidSnapshot, rec.Strategy)
	}
	ks.turns = rec.Turns
	ks.loaded = true
	// Re-enforce budget against the restored buffer — defends against
	// a hand-crafted oversized snapshot.
	e.enforceBudget(ks)
	return persistRecord(ctx, e.state, id, memoryStateRecord{
		Strategy: memory.StrategyTruncation,
		Turns:    ks.turns,
	})
}

func (e *truncationExec) Close(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

func (e *truncationExec) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// enforceBudget drops oldest turns until `sumTokens(turns) <=
// budgetTokens`. A zero or negative budget means "no budget" —
// appending is unbounded.
func (e *truncationExec) enforceBudget(ks *truncationKeyState) {
	if e.budgetTokens <= 0 {
		return
	}
	for len(ks.turns) > 0 && sumTokens(ks.turns) > e.budgetTokens {
		ks.turns = ks.turns[1:]
	}
}

// Compile-time assertion that *truncationExec satisfies
// StrategyExecutor.
var _ StrategyExecutor = (*truncationExec)(nil)

// quadKey is the comparable map key shape for an
// `identity.Quadruple`. Hash-friendly + race-free.
type quadKey struct {
	Tenant  string
	User    string
	Session string
	Run     string
}

func quadKeyFor(id identity.Quadruple) quadKey {
	return quadKey{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Run:     id.RunID,
	}
}

// sumTokens returns the token estimate for a buffer of turns. The
// default estimator is "chars/4 + 1" per brief 04 §2 — cheap to
// compute, calibrated for English, and consistent with the
// predecessor's default.
//
// A future operator-injectable token estimator (brief 04 §2's
// `TokenEstimator func(string) int`) is a deliberate non-goal at
// Phase 24; the constant-shape estimator is good enough for the
// strategy executor's budget enforcement.
func sumTokens(turns []memory.ConversationTurn) int {
	total := 0
	for _, t := range turns {
		total += estimateTurnTokens(t)
	}
	return total
}

func estimateTurnTokens(t memory.ConversationTurn) int {
	// chars/4 + 1 per role + 1 per role (the role token itself).
	return len(t.UserMessage)/4 + 1 + len(t.AssistantResponse)/4 + 1
}
