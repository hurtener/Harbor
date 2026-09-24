package memory

import (
	"context"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
)

// Inspection is a bounded read of committed execution memory, never another transcript.
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

// Access binds driver operations to the cumulative owner and holds no session state.
type Access struct {
	cfg  ConfigSnapshot
	deps Deps
}

// NewAccess shares runtime dependencies without opening storage or background work.
func NewAccess(cfg ConfigSnapshot, deps Deps) *Access {
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyRollingSummary
	}
	return &Access{cfg: cfg, deps: deps}
}

// Inspect reads committed, unexpired memory. Disabled memory never reads history.
func (a *Access) Inspect(ctx context.Context, id identity.Quadruple) (Inspection, error) {
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	if a.cfg.Strategy == StrategyNone {
		return Inspection{Strategy: StrategyNone, Health: HealthHealthy}, nil
	}
	view, err := sessionmemory.Inspect(ctx, a.deps.State, id, nil)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Strategy: StrategyRollingSummary, Items: view.Items, Summary: view.Summary, RecentTurns: view.RecentTurns, EstimatedTokens: view.EstimatedTokens, Health: HealthHealthy}, nil
}

// Put commits an operator note through the execution owner's conditional write.
func (a *Access) Put(ctx context.Context, id identity.Quadruple, turn ConversationTurn) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if a.cfg.Strategy == StrategyNone {
		return "", nil
	}
	turns, ttl := a.cfg.RecentTurns, a.deps.RetentionTTL
	if turns == 0 {
		turns = 20
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return sessionmemory.Put(ctx, a.deps.State, a.deps.Redactor, id, turn.UserMessage, turn.AssistantResponse, turns, ttl, nil)
}

// Delete invalidates affected cumulative context through the same owner as execution.
func (a *Access) Delete(ctx context.Context, id identity.Quadruple, key string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if a.cfg.Strategy == StrategyNone {
		return 0, ErrNotFound
	}
	count, err := sessionmemory.Delete(ctx, a.deps.State, id, key, nil)
	if errors.Is(err, sessionmemory.ErrItemNotFound) {
		return 0, ErrNotFound
	}
	return count, err
}
