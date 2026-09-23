package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
)

// Inspection is the bounded current memory projection used by administrative
// reads. Items come from the same owner as execution, not the event transcript.
type Inspection struct {
	Strategy        Strategy
	Items           []Item
	Summary         string
	RecentTurns     int
	EstimatedTokens int
	Health          Health
}

// Item is an immutable, source-keyed projection of committed session memory.
type Item = sessionmemory.Item

// Access binds the existing driver's administrative operations to execution
// memory. It holds no session state. Non-cumulative strategies retain their
// existing behavior until the remaining legacy-strategy retirement is complete.
type Access struct {
	cfg  ConfigSnapshot
	deps Deps
}

// NewAccess shares runtime dependencies; it creates no store or background work.
func NewAccess(cfg ConfigSnapshot, deps Deps) *Access { return &Access{cfg: cfg, deps: deps} }

// Inspect reads committed cumulative memory for rolling_summary.
func (a *Access) Inspect(ctx context.Context, id identity.Quadruple, store MemoryStore) (Inspection, error) {
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	if a.cfg.Strategy == StrategyRollingSummary {
		view, err := sessionmemory.Inspect(ctx, a.deps.State, id, nil)
		if err != nil {
			return Inspection{}, err
		}
		return Inspection{Strategy: StrategyRollingSummary, Items: view.Items, Summary: view.Summary, RecentTurns: view.RecentTurns, EstimatedTokens: view.EstimatedTokens, Health: HealthHealthy}, nil
	}
	snap, err := store.Snapshot(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	var record Record
	if len(snap.Bytes) > 0 {
		if err := json.Unmarshal(snap.Bytes, &record); err != nil {
			return Inspection{}, err
		}
	}
	patch, err := store.GetLLMContext(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	health, err := store.Health(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	view := Inspection{Strategy: snap.Strategy, Summary: patch.Summary, RecentTurns: len(record.Turns), EstimatedTokens: patch.Tokens, Health: health}
	for i, turn := range record.Turns {
		value, err := turnValue(turn)
		if err != nil {
			return Inspection{}, err
		}
		view.Items = append(view.Items, Item{Key: legacyTurnKey(id, i, turn.Timestamp), CreatedAt: turn.Timestamp, Value: value})
	}
	return view, nil
}

// Put writes through the same cumulative CAS as completed execution history.
func (a *Access) Put(ctx context.Context, id identity.Quadruple, turn ConversationTurn, store MemoryStore) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.cfg.Strategy == StrategyRollingSummary {
		turns, ttl := a.cfg.RecentTurns, a.deps.RetentionTTL
		if turns == 0 {
			turns = 20
		}
		if ttl <= 0 {
			ttl = 24 * time.Hour
		}
		return sessionmemory.Put(ctx, a.deps.State, a.deps.Redactor, id, turn.UserMessage, turn.AssistantResponse, turns, ttl, nil)
	}
	if err := store.AddTurn(ctx, id, turn); err != nil {
		return "", err
	}
	view, err := a.Inspect(ctx, id, store)
	if err != nil {
		return "", err
	}
	if len(view.Items) == 0 {
		return "", nil
	}
	return view.Items[len(view.Items)-1].Key, nil
}

// Delete removes cumulative state through its owner, never Snapshot/Restore.
func (a *Access) Delete(ctx context.Context, id identity.Quadruple, key string, store MemoryStore) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if a.cfg.Strategy == StrategyRollingSummary {
		count, err := sessionmemory.Delete(ctx, a.deps.State, id, key, nil)
		if errors.Is(err, sessionmemory.ErrItemNotFound) {
			return 0, ErrNotFound
		}
		return count, err
	}
	snap, err := store.Snapshot(ctx, id)
	if err != nil {
		return 0, err
	}
	if len(snap.Bytes) == 0 {
		return 0, ErrNotFound
	}
	var record Record
	if err := json.Unmarshal(snap.Bytes, &record); err != nil {
		return 0, err
	}
	for i, turn := range record.Turns {
		if legacyTurnKey(id, i, turn.Timestamp) != key {
			continue
		}
		record.Turns = append(record.Turns[:i:i], record.Turns[i+1:]...)
		encoded, err := json.Marshal(record)
		if err != nil {
			return 0, err
		}
		if err := store.Restore(ctx, id, Snapshot{Strategy: snap.Strategy, Bytes: encoded}); err != nil {
			return 0, err
		}
		return len(record.Turns), nil
	}
	return 0, ErrNotFound
}

// legacyTurnKey is the existing non-cumulative strategy's row identity. Rolling
// memory never uses ordinal keys; its owner keys by immutable source identity.
func legacyTurnKey(id identity.Quadruple, ordinal int, ts time.Time) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%d|%d", id.TenantID, id.UserID, id.SessionID, id.RunID, ordinal, ts.UnixNano()))
	return "mem_" + hex.EncodeToString(digest[:])[:16]
}

// turnValue is the existing note value projection for non-cumulative strategies.
func turnValue(turn ConversationTurn) ([]byte, error) {
	view := map[string]any{"user_message": turn.UserMessage, "assistant_response": turn.AssistantResponse, "timestamp": turn.Timestamp}
	if turn.TrajectoryDigest != nil {
		view["trajectory_digest"] = map[string]any{"tools_invoked": turn.TrajectoryDigest.ToolsInvoked, "observations_summary": turn.TrajectoryDigest.ObservationsSummary, "reasoning_summary": turn.TrajectoryDigest.ReasoningSummary, "artifacts_refs": turn.TrajectoryDigest.ArtifactsRefs}
	}
	if len(turn.ArtifactsHiddenRefs) > 0 {
		view["artifacts_hidden_refs"] = turn.ArtifactsHiddenRefs
	}
	return json.Marshal(view)
}
