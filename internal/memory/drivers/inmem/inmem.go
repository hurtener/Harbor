// Package inmem is Harbor's V1 in-memory MemoryStore driver. It is
// the test reference for the conformance suite — every later
// driver (SQLite + Postgres at Phase 25) inherits the same suite
// verbatim.
//
// At Phase 23 the driver supports `Strategy = StrategyNone` only:
//
//   - AddTurn is a no-op.
//   - GetLLMContext returns an empty `LLMContextPatch`.
//   - EstimateTokens returns 0.
//   - Flush is a no-op.
//   - Health returns `HealthHealthy`.
//   - Snapshot returns an empty snapshot.
//   - Restore accepts only empty snapshots (`ErrInvalidSnapshot`
//     on non-empty bytes).
//
// Phase 24 will extend this driver to implement `truncation` and
// `rolling_summary` over the same surface; Phase 25 will land the
// persistent drivers.
//
// Per D-027, every persistent mutation lands as a `state.StateStore`
// record at `Kind = "memory.state"` so the StateStore conformance
// suite covers the persistence path. Strategy=none has no
// mutations, but the wiring is in place so Phase 24's
// implementations reuse it.
//
// Identity is mandatory at every method: empty tenant / user /
// session returns wrapped `ErrIdentityRequired` AND publishes one
// `memory.identity_rejected` event on the injected EventBus.
package inmem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
)

// kindMemoryState is the StateStore Kind constant for the
// memory-state record. Centralised so the typed wrapper and tests
// reference one symbol.
const kindMemoryState = "memory.state"

// New constructs a `MemoryStore` directly. Exposed for tests that
// want to skip the registry; production callers go through
// `memory.Open`.
//
// Strategy unsupported at Phase 23 (anything other than
// `StrategyNone` or empty-equivalent) returns
// `ErrStrategyNotImplemented` rather than silently coercing. Empty
// Strategy in `cfg` defaults to `StrategyNone`.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("memory/inmem: deps.State is required")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/inmem: deps.Bus is required")
	}
	strategy := cfg.Strategy
	if strategy == "" {
		strategy = memory.StrategyNone
	}
	if strategy != memory.StrategyNone {
		return nil, fmt.Errorf("%w: %q (Phase 23 supports %q only)",
			memory.ErrStrategyNotImplemented, strategy, memory.StrategyNone)
	}
	return &driver{
		strategy: strategy,
		state:    deps.State,
		bus:      deps.Bus,
	}, nil
}

func init() {
	memory.Register("inmem", func(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
		return New(cfg, deps)
	})
}

// driver is the Strategy=none in-memory MemoryStore.
//
// All state behind the driver is held in the injected
// `state.StateStore`; the driver itself carries only configuration
// + the cross-cutting deps. This keeps the driver compatible with
// the D-025 concurrent-reuse contract — there's no mutable field
// on the driver struct other than the close flag.
type driver struct {
	strategy memory.Strategy
	state    state.StateStore
	bus      events.EventBus

	// mu guards the closed flag's read/write pair against double-
	// close interleaving. The atomic is the operational gate; the
	// mutex serialises Close itself so it idempotently observes
	// "already closed" rather than racing on the write.
	mu     sync.Mutex
	closed atomic.Bool
}

// memoryStateRecord is the JSON-serialised internal shape Snapshot
// persists. Phase 23 only writes empty records (Strategy=none);
// Phase 24 will append the recent-turn buffer + the rolling-summary
// fields. The shape is intentionally narrow: drivers re-marshal at
// every Snapshot, and the bytes round-trip through Restore via the
// same JSON shape.
type memoryStateRecord struct {
	Strategy memory.Strategy            `json:"strategy"`
	Turns    []memory.ConversationTurn  `json:"turns,omitempty"`
}

// AddTurn implements memory.MemoryStore.
//
// Strategy=none: no-op. Identity validated at the boundary; missing
// triple → fail-closed with bus emit.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, _ memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	// Strategy=none: nothing persisted. Phase 24 will append the
	// turn to the StateStore-backed buffer here.
	return nil
}

// GetLLMContext implements memory.MemoryStore.
//
// Strategy=none returns an empty patch.
func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return memory.LLMContextPatch{Strategy: d.strategy}, nil
}

// EstimateTokens implements memory.MemoryStore.
//
// Strategy=none returns 0.
func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return 0, nil
}

// Flush implements memory.MemoryStore.
//
// Strategy=none: no-op. Phase 24 will delete the StateStore
// record at (id, kindMemoryState).
func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}
	// Strategy=none: idempotent no-op. Even though there's nothing
	// to delete, we walk the StateStore-delete path so the InMem
	// driver's behaviour matches the persistent drivers' (Phase 25):
	// Flush always returns nil on a valid identity.
	if err := d.state.Delete(ctx, id, kindMemoryState); err != nil {
		return fmt.Errorf("memory/inmem: Flush delete: %w", err)
	}
	return nil
}

// Health implements memory.MemoryStore.
//
// Strategy=none always reports `HealthHealthy`.
func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return memory.HealthHealthy, nil
}

// Snapshot implements memory.MemoryStore.
//
// Strategy=none returns an empty snapshot. Reads through the
// StateStore so the conformance suite exercises the
// `state.StateStore.Load` path (D-027 typed wrapper). Missing
// records translate to an empty snapshot (memory has not been
// touched yet for this identity); other errors propagate.
func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}

	rec, err := d.state.Load(ctx, id, kindMemoryState)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			// No prior memory state for this identity — return an
			// empty Strategy=none snapshot. Strategy=none is the
			// default initial state per RFC §6.6.
			return memory.Snapshot{Strategy: d.strategy}, nil
		}
		return memory.Snapshot{}, fmt.Errorf("memory/inmem: Snapshot load: %w", err)
	}
	return memory.Snapshot{Strategy: d.strategy, Bytes: rec.Bytes}, nil
}

// Restore implements memory.MemoryStore.
//
// Strategy=none accepts only empty snapshots. The snapshot's
// Strategy MUST match the driver's; mismatched strategies return
// `ErrInvalidSnapshot` — fail loudly, never silently coerce.
//
// Restore persists the (empty) snapshot through the StateStore so
// later Snapshot calls observe the slot. This exercises the D-027
// typed-wrapper write path even at Strategy=none, so Phase 24's
// `truncation` / `rolling_summary` inherit the wiring.
func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}

	// Empty snapshot (zero value, no strategy / bytes) is always
	// acceptable and round-trips the initial state.
	if snap.IsEmpty() {
		return d.persistRecord(ctx, id, memoryStateRecord{Strategy: d.strategy})
	}
	// Otherwise: the snapshot's Strategy must match the driver's.
	if snap.Strategy != d.strategy {
		return fmt.Errorf("%w: snapshot strategy=%q driver strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, d.strategy)
	}
	// Same-strategy empty-bytes is the "default initial state" snapshot
	// `Snapshot` returns when no record exists yet — round-trip it.
	if len(snap.Bytes) == 0 {
		return d.persistRecord(ctx, id, memoryStateRecord{Strategy: d.strategy})
	}
	if d.strategy == memory.StrategyNone {
		// At Strategy=none the only acceptable bytes are the
		// canonical empty record `{"strategy":"none"}`. Decode + reject
		// anything that carries actual turns. Decoding here also
		// catches malformed bytes loudly rather than silently storing.
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
		return d.persistRecord(ctx, id, rec)
	}
	// Phase 24 will branch on d.strategy here for truncation +
	// rolling_summary. Today, only Strategy=none with empty bytes is
	// reachable.
	return fmt.Errorf("%w: unsupported snapshot for strategy %q",
		memory.ErrInvalidSnapshot, d.strategy)
}

// persistRecord marshals the typed record and writes through the
// injected StateStore (D-027). EventID is fresh per write — Restore
// is not idempotency-keyed at the caller level.
func (d *driver) persistRecord(ctx context.Context, id identity.Quadruple, rec memoryStateRecord) error {
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory/inmem: marshal record: %w", err)
	}
	sr := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: id,
		Kind:     kindMemoryState,
		Bytes:    bytes,
	}
	if err := d.state.Save(ctx, sr); err != nil {
		return fmt.Errorf("memory/inmem: save record: %w", err)
	}
	return nil
}

// Close implements memory.MemoryStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed.Store(true)
	return nil
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)
