// Package inmem is Harbor's V1 in-memory MemoryStore driver. It is
// the test reference for the conformance suite — every later
// driver (SQLite + Postgres at Phase 25) inherits the same suite
// verbatim.
//
// At Phase 24 the driver supports all three strategies:
//
//   - `none` — AddTurn is a no-op; GetLLMContext returns empty.
//   - `truncation` — recent-window buffer with `OverflowDropOldest`
//     enforcement at the configured `BudgetTokens` boundary.
//   - `rolling_summary` — recent-window + background-summarised
//     long-term context with the `healthy → retry → degraded →
//     recovering → healthy` FSM. The injectable
//     `memory.Summarizer` (LLM-backed at Phase 32+; stubbed via
//     `strategy.EchoSummarizer` for tests) is consumed via
//     `inmem.Options.Summarizer`.
//
// Per D-027 (typed wrapper over StateStore), every successful
// mutation lands as a `state.StateStore` record at `Kind =
// "memory.state"` so the StateStore conformance suite covers the
// persistence path. The driver itself holds no per-key buffer
// state; everything lives behind the strategy executor.
//
// Identity is mandatory at every method: empty tenant / user /
// session returns wrapped `memory.ErrIdentityRequired` AND
// publishes one `memory.identity_rejected` event on the injected
// EventBus.
package inmem

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// kindMemoryState aliases the canonical `memory.KindMemoryState`
// constant for ergonomic in-package use. The exported symbol lives
// in `internal/memory/wire.go` so SQLite + Postgres drivers (Phase
// 25) share the same routing key.
const kindMemoryState = memory.KindMemoryState

// Options carries InMem-driver-specific knobs that don't live on
// the generic `memory.ConfigSnapshot`. The summariser is the only
// Phase-24-relevant option; future drivers may add more.
//
// `Summarizer` is REQUIRED when the configured strategy is
// `rolling_summary`. The `New` constructor rejects a nil
// summariser for that strategy with a wrapped error; the registry
// path (`memory.Open`) currently has no way to inject a
// non-default summariser, so callers wanting `rolling_summary` MUST
// call `New` directly (Phase 32+ will land an LLM-backed
// summariser the registry can resolve by default).
//
// `RecoveryBacklogMax` overrides the strategy default (16); zero
// uses the default. Operators set this through
// `config.MemoryConfig.RecoveryBacklogMax`; the registry-level
// `memory.Open` propagates the value via `ConfigSnapshot` (see
// Phase 24's `memory.ConfigSnapshot` extension).
type Options struct {
	Summarizer         memory.Summarizer
	RecoveryBacklogMax int
}

// New constructs a `MemoryStore` directly. Exposed for tests +
// production callers that want full control over the strategy
// `Options`; production callers using `memory.Open` go through the
// registry and the default summariser (which is nil at Phase 24 —
// only `none` + `truncation` are registry-reachable).
//
// Strategy unsupported at this phase returns
// `memory.ErrStrategyNotImplemented`.
func New(cfg memory.ConfigSnapshot, deps memory.Deps, opts Options) (memory.MemoryStore, error) {
	if deps.State == nil {
		return nil, fmt.Errorf("memory/inmem: deps.State is required")
	}
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/inmem: deps.Bus is required")
	}
	s := cfg.Strategy
	if s == "" {
		s = memory.StrategyNone
	}
	// Options-level overrides take precedence; otherwise the
	// ConfigSnapshot value flows through.
	backlog := opts.RecoveryBacklogMax
	if backlog == 0 {
		backlog = cfg.RecoveryBacklogMax
	}
	execDeps := strategy.Deps{
		State:              deps.State,
		Bus:                deps.Bus,
		Summarizer:         opts.Summarizer,
		BudgetTokens:       cfg.BudgetTokens,
		RecoveryBacklogMax: backlog,
	}
	exec, err := strategy.New(s, execDeps)
	if err != nil {
		return nil, err
	}
	return &driver{
		strategy: s,
		bus:      deps.Bus,
		exec:     exec,
	}, nil
}

func init() {
	memory.Register("inmem", func(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
		// Registry path: no summariser is injectable. Only
		// strategies that don't require a summariser are
		// constructable through this path. The rolling-summary
		// strategy needs `Summarizer`; operators staging it must
		// call `inmem.New(...)` directly until Phase 32+ lands the
		// LLM-backed summariser the registry resolves by default.
		return New(cfg, deps, Options{})
	})
}

// driver is the Phase-24 in-memory MemoryStore. The driver itself
// owns identity-rejection emit + the closed flag; per-key state +
// strategy logic live behind the strategy executor.
//
// Concurrent-reuse contract (D-025): one instance is safe to share
// across N concurrent goroutines. The closed flag is `atomic.Bool`
// + a sync.Mutex serialises Close to guarantee idempotency.
type driver struct {
	strategy memory.Strategy
	bus      events.EventBus
	exec     strategy.StrategyExecutor

	mu     sync.Mutex
	closed atomic.Bool
}

// memoryStateRecord aliases `memory.Record` — the canonical wire
// envelope shared by every driver (Phase 25 SQLite + Postgres
// inherit the same shape). Centralised in `internal/memory/wire.go`
// so cross-driver `Snapshot/Restore` is byte-stable.
type memoryStateRecord = memory.Record

// AddTurn implements memory.MemoryStore. Identity validated at the
// boundary; missing triple → fail-closed with bus emit. The
// strategy executor owns turn-handling — `Strategy=none` is a
// no-op, `truncation` / `rolling_summary` consume `turn`.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	return d.exec.AddTurn(ctx, id, turn)
}

func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return d.exec.GetLLMContext(ctx, id)
}

func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return d.exec.EstimateTokens(ctx, id)
}

func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}
	return d.exec.Flush(ctx, id)
}

func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return d.exec.Health(ctx, id)
}

func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}
	return d.exec.Snapshot(ctx, id)
}

func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}
	return d.exec.Restore(ctx, id, snap)
}

// Close implements memory.MemoryStore. Idempotent. Tears down the
// strategy executor's per-strategy resources (recovery loop
// goroutine for rolling_summary; nothing for none/truncation).
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed.Load() {
		return nil
	}
	d.closed.Store(true)
	return d.exec.Close(ctx)
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)
