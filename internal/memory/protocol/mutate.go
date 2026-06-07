// Package protocol — Phase 108n (D-186) additions: the strategy-trace read
// projection + the admin-gated mutation pair (`memory.put` /
// `memory.delete`). Like List / Get / Health these are stateless pure
// functions — every dependency is passed in per call. They compose ONLY
// the shipped `memory.MemoryStore` interface (Snapshot / Restore / AddTurn
// / GetLLMContext / Health) — no new driver-seam method, so all three V1
// drivers (inmem / sqlite / postgres) back them with zero per-driver work.
//
// # strategy_trace is an honest read
//
// `StrategyTrace` projects the strategy's LIVE `GetLLMContext` + `Health`
// output — the rolling-summary text the strategy injects, the verbatim-turn
// count it keeps, the token estimate, and the health state. It is NOT a
// fabricated per-step "selection with rejections" (the rolling_summary
// strategy summarises; it does not select-and-reject candidates). An empty
// session projects an empty trace (CLAUDE.md §13 — no synthesised data).
//
// # The mutations are read-modify-write on the shipped interface
//
// `Put` appends a turn via `AddTurn`. `Delete` removes ONE turn by key via a
// `Snapshot` → drop-the-keyed-turn → `Restore` round-trip — the Record
// envelope (incl. the rolling summary) is preserved losslessly (the
// `memory.Record.Summary` field added in 108n). Both emit an audit event on
// the bus (`memory.item_put` / `memory.item_deleted`, SafePayload — the
// hashed key only, never the turn text). Admin-gating lives at the handler
// edge (D-079); the service trusts a gated, identity-validated call.
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// StrategyTraceDeps carries the StrategyTrace dependencies.
type StrategyTraceDeps struct {
	// Store is the memory store the trace projects from. Mandatory.
	Store memory.MemoryStore
}

// StrategyTrace projects how the configured memory strategy is compacting
// the caller's session memory right now — the strategy's live
// `GetLLMContext` (rolling summary + verbatim turns + token estimate) plus
// `Health`. Identity is validated before the store is touched; a missing
// triple fails loudly with `memory.ErrIdentityRequired` (D-001 / D-033).
func StrategyTrace(ctx context.Context, deps StrategyTraceDeps, id identity.Quadruple) (prototypes.MemoryStrategyTraceResponse, error) {
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryStrategyTraceResponse{}, err
	}
	patch, err := deps.Store.GetLLMContext(ctx, id)
	if err != nil {
		return prototypes.MemoryStrategyTraceResponse{}, fmt.Errorf("memory/protocol: strategy_trace GetLLMContext: %w", err)
	}
	health, err := deps.Store.Health(ctx, id)
	if err != nil {
		return prototypes.MemoryStrategyTraceResponse{}, fmt.Errorf("memory/protocol: strategy_trace Health: %w", err)
	}
	return prototypes.MemoryStrategyTraceResponse{
		Trace: prototypes.MemoryStrategyTrace{
			Strategy:        string(patch.Strategy),
			Summary:         patch.Summary,
			RecentTurnCount: len(patch.RecentTurns),
			EstimatedTokens: patch.Tokens,
			Health:          string(health),
		},
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// PutDeps carries the Put dependencies.
type PutDeps struct {
	// Store is the memory store the turn is appended to. Mandatory.
	Store memory.MemoryStore
	// Bus is the events bus the audit event publishes on. Optional — a
	// nil bus skips the emit (test wiring); production always supplies it.
	Bus events.EventBus
}

// Put appends an operator-supplied conversation turn to the caller's
// session memory via the shipped `AddTurn`, then emits the
// `memory.item_put` audit event. Returns the deterministic key of the
// appended turn (the value a subsequent `memory.get` resolves). Identity is
// validated before the store is touched; admin-gating is the handler's job.
func Put(ctx context.Context, deps PutDeps, req prototypes.MemoryPutRequest, id identity.Quadruple) (prototypes.MemoryPutResponse, error) {
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryPutResponse{}, err
	}
	ts := time.Now()
	turn := memory.ConversationTurn{
		UserMessage:       req.Turn.UserMessage,
		AssistantResponse: req.Turn.AssistantResponse,
		Timestamp:         ts,
	}
	if err := deps.Store.AddTurn(ctx, id, turn); err != nil {
		return prototypes.MemoryPutResponse{}, fmt.Errorf("memory/protocol: put AddTurn: %w", err)
	}
	// Resolve the appended turn's key the same way List / Get do — read
	// the snapshot back and key the LAST turn (the one just appended). A
	// strategy that immediately summarised it away (over-budget) leaves the
	// key empty rather than a fabricated one (CLAUDE.md §13).
	key := ""
	snap, err := deps.Store.Snapshot(ctx, id)
	if err == nil {
		if turns, derr := decodeTurns(snap); derr == nil && len(turns) > 0 {
			last := len(turns) - 1
			key = memTurnKey(id, last, turns[last].Timestamp)
		}
	}
	emitMutation(ctx, deps.Bus, id, memory.EventTypeMemoryItemPut, "put", key)
	return prototypes.MemoryPutResponse{Key: key, ProtocolVersion: prototypes.ProtocolVersion}, nil
}

// DeleteDeps carries the Delete dependencies.
type DeleteDeps struct {
	// Store is the memory store the turn is evicted from. Mandatory.
	Store memory.MemoryStore
	// Bus is the events bus the audit event publishes on. Optional.
	Bus events.EventBus
}

// Delete evicts ONE conversation turn (by key) from the caller's session
// memory via a `Snapshot` → drop-the-keyed-turn → `Restore` read-modify-
// write. The Record envelope — including the rolling-summary text — is
// preserved losslessly (`memory.Record.Summary`). A key that matches no
// turn fails loudly with `memory.ErrNotFound` (never a silent no-op).
// Emits the `memory.item_deleted` audit event on success.
func Delete(ctx context.Context, deps DeleteDeps, req prototypes.MemoryDeleteRequest, id identity.Quadruple) (prototypes.MemoryDeleteResponse, error) {
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryDeleteResponse{}, err
	}
	snap, err := deps.Store.Snapshot(ctx, id)
	if err != nil {
		return prototypes.MemoryDeleteResponse{}, fmt.Errorf("memory/protocol: delete Snapshot: %w", err)
	}
	if snap.IsEmpty() || len(snap.Bytes) == 0 {
		return prototypes.MemoryDeleteResponse{}, memory.ErrNotFound
	}
	var rec memory.Record
	if uerr := json.Unmarshal(snap.Bytes, &rec); uerr != nil {
		return prototypes.MemoryDeleteResponse{}, fmt.Errorf("memory/protocol: delete decode snapshot: %w", uerr)
	}
	idx := -1
	for i, turn := range rec.Turns {
		if memTurnKey(id, i, turn.Timestamp) == req.Key {
			idx = i
			break
		}
	}
	if idx < 0 {
		return prototypes.MemoryDeleteResponse{}, memory.ErrNotFound
	}
	rec.Turns = append(rec.Turns[:idx], rec.Turns[idx+1:]...)
	bytes, merr := json.Marshal(rec)
	if merr != nil {
		return prototypes.MemoryDeleteResponse{}, fmt.Errorf("memory/protocol: delete re-marshal record: %w", merr)
	}
	if rerr := deps.Store.Restore(ctx, id, memory.Snapshot{Strategy: snap.Strategy, Bytes: bytes}); rerr != nil {
		return prototypes.MemoryDeleteResponse{}, fmt.Errorf("memory/protocol: delete Restore: %w", rerr)
	}
	emitMutation(ctx, deps.Bus, id, memory.EventTypeMemoryItemDeleted, "delete", req.Key)
	return prototypes.MemoryDeleteResponse{
		Deleted:         true,
		RemainingTurns:  len(rec.Turns),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// decodeTurns decodes a snapshot's Record envelope into its turn slice.
func decodeTurns(snap memory.Snapshot) ([]memory.ConversationTurn, error) {
	if snap.IsEmpty() || len(snap.Bytes) == 0 {
		return nil, nil
	}
	var rec memory.Record
	if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
		return nil, err
	}
	return rec.Turns, nil
}

// emitMutation publishes the `memory.item_put` / `memory.item_deleted`
// audit event on the bus. Best-effort (the mutation already committed; a
// bus failure must not undo it — the same posture EmitHealthChanged takes).
// A nil bus (test wiring) is a no-op.
func emitMutation(ctx context.Context, bus events.EventBus, id identity.Quadruple, typ events.EventType, op, key string) {
	if bus == nil {
		return
	}
	_ = bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort audit emit — a bus failure must not undo the committed mutation (see func doc).
		Type:       typ,
		Identity:   id,
		OccurredAt: time.Now(),
		Payload:    memory.MemoryMutationPayload{Operation: op, Key: key},
	})
}
