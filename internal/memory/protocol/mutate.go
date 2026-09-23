// Administrative memory mutations use the same owner as inspection and
// execution. The transport validates identity and admin authority; the owner
// validates source identity and atomically fences stale context publication.
package protocol

import (
	"context"
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

// StrategyTrace reports the checkpoint and bounded recent tail used by execution.
func StrategyTrace(ctx context.Context, deps StrategyTraceDeps, id identity.Quadruple) (prototypes.MemoryStrategyTraceResponse, error) {
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryStrategyTraceResponse{}, err
	}
	patch, err := deps.Store.Inspect(ctx, id)
	if err != nil {
		return prototypes.MemoryStrategyTraceResponse{}, fmt.Errorf("memory/protocol: strategy_trace Inspect: %w", err)
	}
	return prototypes.MemoryStrategyTraceResponse{
		Trace: prototypes.MemoryStrategyTrace{
			Strategy:        string(patch.Strategy),
			Summary:         patch.Summary,
			RecentTurnCount: patch.RecentTurns,
			EstimatedTokens: patch.EstimatedTokens,
			Health:          string(patch.Health),
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

// Put records an operator note and returns its committed, resolvable key.
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
	key, err := deps.Store.Put(ctx, id, turn)
	if err != nil {
		return prototypes.MemoryPutResponse{}, fmt.Errorf("memory/protocol: put: %w", err)
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

// Delete removes the named source through the owner. Cumulative deletion
// invalidates an affected checkpoint and fences frozen admissions; it never
// blindly restores a stale snapshot or claims selective summary forgetting.
func Delete(ctx context.Context, deps DeleteDeps, req prototypes.MemoryDeleteRequest, id identity.Quadruple) (prototypes.MemoryDeleteResponse, error) {
	if err := memory.ValidateIdentity(id); err != nil {
		return prototypes.MemoryDeleteResponse{}, err
	}
	remaining, err := deps.Store.Delete(ctx, id, req.Key)
	if err != nil {
		return prototypes.MemoryDeleteResponse{}, fmt.Errorf("memory/protocol: delete: %w", err)
	}
	emitMutation(ctx, deps.Bus, id, memory.EventTypeMemoryItemDeleted, "delete", req.Key)
	return prototypes.MemoryDeleteResponse{
		Deleted:         true,
		RemainingTurns:  remaining,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
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
