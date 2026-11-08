package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// TurnStage identifies a fixed, content-free pre-model preparation stage.
type TurnStage uint8

const (
	// TurnStageMCPReconcile measures the full run-start MCP reconciliation.
	TurnStageMCPReconcile TurnStage = iota
	// TurnStageMCPDiscoveryReconcile measures discovery-origin reconciliation.
	TurnStageMCPDiscoveryReconcile
	// TurnStageToolAttach measures connection detach/attach reconciliation.
	TurnStageToolAttach
	// TurnStageProviderRoute accumulates provider-route selection and credential
	// resolution before dispatch.
	TurnStageProviderRoute
	turnStageCount
)

// TurnPrepared records how run start used ahead-of-time session preparation.
// The values are a fixed closed set so the log attribute stays bounded.
type TurnPrepared uint8

const (
	// TurnPreparedNone means no preparation outcome was recorded.
	TurnPreparedNone TurnPrepared = iota
	// TurnPreparedHit means a completed, still-fresh preparation with an
	// unchanged input stamp replaced the run-start reconcile.
	TurnPreparedHit
	// TurnPreparedJoined means run start waited for an in-flight preparation
	// and then reused it.
	TurnPreparedJoined
	// TurnPreparedStale means a preparation existed but was expired, failed,
	// or its inputs changed, so the full reconcile ran.
	TurnPreparedStale
	// TurnPreparedMiss means no preparation existed for the run's principal.
	TurnPreparedMiss
	turnPreparedCount
)

var turnPreparedNames = [...]string{"", "hit", "joined", "stale", "miss"}

type turnTimingKey struct{}

// turnTiming is per-turn state, internally synchronized because parallel
// model calls may race to observe the first dispatch. It never stores payloads.
type turnTiming struct {
	mu      sync.Mutex
	logger  *slog.Logger
	started time.Time
	now     func() time.Time
	emitted bool
	stages  [turnStageCount]time.Duration
	// prepared is the run-start preparation outcome; routeCached reports
	// that at least one provider-route selection was served from cache.
	prepared    TurnPrepared
	routeCached bool
}

// BeginTurnTiming starts a fresh turn observation. Callers use a server-owned
// turn/control timestamp; a zero timestamp means the current instant.
// Nothing is logged until an actual model request is dispatched.
func BeginTurnTiming(ctx context.Context, logger *slog.Logger, startedAt time.Time) context.Context {
	if logger == nil {
		logger = slog.Default()
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	return context.WithValue(ctx, turnTimingKey{}, &turnTiming{
		logger: logger, started: startedAt, now: time.Now,
	})
}

// EnsureTurnTiming starts an observation only when the caller has not already
// started one, preserving preparation time measured by an outer runtime layer.
func EnsureTurnTiming(ctx context.Context, logger *slog.Logger, startedAt time.Time) context.Context {
	if _, ok := ctx.Value(turnTimingKey{}).(*turnTiming); ok {
		return ctx
	}
	return BeginTurnTiming(ctx, logger, startedAt)
}

// RecordTurnStage adds a completed preparation stage to the current turn.
// Only fixed stage identifiers are accepted; calls after dispatch are ignored.
func RecordTurnStage(ctx context.Context, stage TurnStage, duration time.Duration) {
	timing, ok := ctx.Value(turnTimingKey{}).(*turnTiming)
	if !ok || stage >= turnStageCount || duration < 0 {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if !timing.emitted {
		timing.stages[stage] += duration
	}
}

// RecordTurnPrepared records the run-start preparation outcome for this
// turn. Unknown values and calls after dispatch are ignored.
func RecordTurnPrepared(ctx context.Context, prepared TurnPrepared) {
	timing, ok := ctx.Value(turnTimingKey{}).(*turnTiming)
	if !ok || prepared >= turnPreparedCount {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if !timing.emitted {
		timing.prepared = prepared
	}
}

// RecordTurnProviderRouteCached marks that a provider-route selection for
// this turn was served from the selection cache.
func RecordTurnProviderRouteCached(ctx context.Context) {
	timing, ok := ctx.Value(turnTimingKey{}).(*turnTiming)
	if !ok {
		return
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	if !timing.emitted {
		timing.routeCached = true
	}
}

// ObserveFirstModelRequest emits turn.pre_model exactly once for this turn.
// It must be called only for a main planner completion at the actual driver
// dispatch boundary, after local gates. Auxiliary calls do not consume it.
// No model means no record. Identity comes from the dispatch context.
func ObserveFirstModelRequest(ctx context.Context) {
	timing, ok := ctx.Value(turnTimingKey{}).(*turnTiming)
	if !ok {
		return
	}
	timing.mu.Lock()
	if timing.emitted {
		timing.mu.Unlock()
		return
	}
	timing.emitted = true
	elapsed := max(timing.now().Sub(timing.started), 0)
	stages := timing.stages
	prepared, routeCached := timing.prepared, timing.routeCached
	timing.mu.Unlock()
	attrs := []slog.Attr{slog.Float64("duration_ms", float64(elapsed)/float64(time.Millisecond))}
	keys := [...]string{"mcp_reconcile_ms", "mcp_discovery_reconcile_ms", "tool_attach_ms", "provider_route_ms"}
	for stage, duration := range stages {
		if duration > 0 {
			attrs = append(attrs, slog.Float64(keys[stage], float64(duration)/float64(time.Millisecond)))
		}
	}
	if prepared != TurnPreparedNone {
		attrs = append(attrs, slog.String("prepared", turnPreparedNames[prepared]))
	}
	if routeCached {
		attrs = append(attrs, slog.Bool("provider_route_cached", true))
	}
	timing.logger.LogAttrs(ctx, slog.LevelInfo, "turn.pre_model", appendCtxIdentity(ctx, attrs)...)
}
