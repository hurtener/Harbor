package strategy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// noneExec implements StrategyExecutor for `StrategyNone`. The
// surface intentionally matches Phase 23's InMem driver verbatim —
// AddTurn is a no-op, GetLLMContext returns empty, EstimateTokens
// returns 0, Flush is a no-op, Health is `HealthHealthy`, and the
// Snapshot/Restore round-trip goes through `state.StateStore`.
//
// Carrying the strategy on the executor lets the rolling-summary
// fallback path produce identical Strategy=none semantics when the
// strategy degrades (it does not, in fact, fall back to noneExec
// today — degraded mode falls back to truncation semantics — but
// the symmetric surface keeps the two strategies pin-compatible).
type noneExec struct {
	state state.StateStore
	bus   events.EventBus
}

func newNoneExec(deps Deps) *noneExec {
	return &noneExec{state: deps.State, bus: deps.Bus}
}

func (e *noneExec) AddTurn(_ context.Context, _ identity.Quadruple, _ memory.ConversationTurn) error {
	// Strategy=none never persists turns. The strategy is
	// operationally "memory disabled".
	return nil
}

func (e *noneExec) GetLLMContext(_ context.Context, _ identity.Quadruple) (memory.LLMContextPatch, error) {
	return memory.LLMContextPatch{Strategy: memory.StrategyNone}, nil
}

func (e *noneExec) EstimateTokens(_ context.Context, _ identity.Quadruple) (int, error) {
	return 0, nil
}

func (e *noneExec) Flush(ctx context.Context, id identity.Quadruple) error {
	if err := e.state.Delete(ctx, id, kindMemoryState); err != nil {
		return fmt.Errorf("memory/strategy/none: Flush delete: %w", err)
	}
	return nil
}

func (e *noneExec) Health(_ context.Context, _ identity.Quadruple) (memory.Health, error) {
	return memory.HealthHealthy, nil
}

func (e *noneExec) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	rec, err := e.state.Load(ctx, id, kindMemoryState)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return memory.Snapshot{Strategy: memory.StrategyNone}, nil
		}
		return memory.Snapshot{}, fmt.Errorf("memory/strategy/none: Snapshot load: %w", err)
	}
	return memory.Snapshot{Strategy: memory.StrategyNone, Bytes: rec.Bytes}, nil
}

func (e *noneExec) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if snap.IsEmpty() {
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyNone})
	}
	if snap.Strategy != memory.StrategyNone {
		return fmt.Errorf("%w: snapshot strategy=%q, executor strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, memory.StrategyNone)
	}
	if len(snap.Bytes) == 0 {
		return persistRecord(ctx, e.state, id, memoryStateRecord{Strategy: memory.StrategyNone})
	}
	var rec memoryStateRecord
	if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
		return fmt.Errorf("%w: %v", memory.ErrInvalidSnapshot, err)
	}
	if rec.Strategy != memory.StrategyNone {
		return fmt.Errorf("%w: record strategy=%q", memory.ErrInvalidSnapshot, rec.Strategy)
	}
	if len(rec.Turns) > 0 {
		return fmt.Errorf("%w: Strategy=none cannot carry %d turn(s)",
			memory.ErrInvalidSnapshot, len(rec.Turns))
	}
	return persistRecord(ctx, e.state, id, rec)
}

func (e *noneExec) Close(_ context.Context) error {
	return nil
}

// memoryStateRecord is the JSON-serialised internal shape every
// strategy persists. Each strategy reads / writes only the fields
// it understands; cross-strategy reads fail loudly at the
// `rec.Strategy` check.
//
// Forward-compatibility: adding a field is safe (the JSON
// unmarshal ignores unknown fields by default); removing a field
// requires care because in-flight snapshots may still reference it.
type memoryStateRecord struct {
	Strategy memory.Strategy           `json:"strategy"`
	Turns    []memory.ConversationTurn `json:"turns,omitempty"`
	Summary  string                    `json:"summary,omitempty"`
}

// persistRecord marshals the typed record and writes through the
// injected StateStore (D-027 typed wrapper).
func persistRecord(ctx context.Context, st state.StateStore, id identity.Quadruple, rec memoryStateRecord) error {
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory/strategy: marshal record: %w", err)
	}
	sr := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: id,
		Kind:     kindMemoryState,
		Bytes:    bytes,
	}
	if err := st.Save(ctx, sr); err != nil {
		return fmt.Errorf("memory/strategy: save record: %w", err)
	}
	return nil
}

// loadRecord reads + unmarshals a memory-state record for `id`.
// Returns the empty record (no error) when the StateStore has no
// entry yet — the executor treats this as "first-touch", not an
// error condition.
func loadRecord(ctx context.Context, st state.StateStore, id identity.Quadruple) (memoryStateRecord, error) {
	rec, err := st.Load(ctx, id, kindMemoryState)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return memoryStateRecord{}, nil
		}
		return memoryStateRecord{}, fmt.Errorf("memory/strategy: load record: %w", err)
	}
	var out memoryStateRecord
	if err := json.Unmarshal(rec.Bytes, &out); err != nil {
		return memoryStateRecord{}, fmt.Errorf("memory/strategy: unmarshal record: %w", err)
	}
	return out, nil
}

// Compile-time assertion that *noneExec satisfies StrategyExecutor.
var _ StrategyExecutor = (*noneExec)(nil)
