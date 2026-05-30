// Package postgres is Harbor's Postgres-backed `memory.MemoryStore`
// driver. It is the third leg of the memory persistence triad
// (in-memory, SQLite, Postgres) defined by RFC §6.6 + §9.
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Parametric queries everywhere; no string
// concatenation into SQL (AGENTS.md §9). Advisory locks serialise
// the migration runner so multi-replica boots are race-free.
//
// # Strategy delegation (Phase 25a, D-174)
//
// All three memory strategies (`none`, `truncation`,
// `rolling_summary`) are implemented by the driver-agnostic
// `internal/memory/strategy` executor package; this driver is a thin
// shell that owns the boundary (identity validation + the
// `memory.identity_rejected` emit + the `closed` flag) and delegates
// every `MemoryStore` method to a `strategy.StrategyExecutor`. The
// executor persists state through the injected `state.StateStore`
// (D-027 typed wrapper, `Kind = "memory.state"`). When that
// StateStore is itself Postgres-backed (the operator's
// `state.driver: postgres`), the memory strategies persist durably —
// which is what makes `truncation` / `rolling_summary` survive a
// restart. No strategy algorithm is reimplemented in SQL here.
//
// The driver still opens its own `*sql.DB` against `cfg.DSN` and runs
// its embedded `memory_state` migration so a misconfigured DSN fails
// loudly at boot, but the live read/write path rides entirely on the
// executor's `state.StateStore` writes. The driver's own
// `memory_state` table is vestigial under delegation; it is kept
// (never edited — migrations are forward-only, AGENTS.md §9 / §13)
// for back-compat with rows written by the pre-25a strategy=none path.
//
// Per AGENTS.md §5 (D-025), the driver is safe for concurrent reuse
// across N goroutines.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/strategy"
)

// driverName is the name under which this driver self-registers with
// `memory.Register`.
const driverName = "postgres"

// pgxDriverName is the database/sql driver name registered by the
// pgx stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Documented in the phase plan; tuning
// lives in a future config knob, not here. Values mirror the Phase
// 16 StateStore + Phase 18 ArtifactStore drivers for consistency.
const (
	defaultMaxOpenConns    = 25
	defaultMaxIdleConns    = 5
	defaultConnMaxLifetime = 5 * time.Minute
)

// New constructs a Postgres-backed `memory.MemoryStore` against
// `cfg.DSN`. Production callers go through `memory.Open`; tests may
// call `New` directly to skip the registry.
//
// The configured strategy is resolved by the shared
// `strategy.StrategyExecutor` (Phase 25a, D-174): `none`,
// `truncation`, and `rolling_summary` all delegate to the executor,
// which persists through `deps.State`. `rolling_summary` requires a
// non-nil `deps.Summarizer`; the executor's `New` rejects a nil
// summariser for that strategy — fail loudly, never a stub fallback
// (AGENTS.md §13).
//
// `deps.Bus` is required. `deps.State` is required — it is the
// persistence floor the strategy executor writes through.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/postgres: deps.Bus is required")
	}
	if deps.State == nil {
		return nil, fmt.Errorf("memory/postgres: deps.State is required (strategy executor persists through it)")
	}
	if cfg.DSN == "" {
		return nil, errors.New("memory/postgres: cfg.DSN is required")
	}

	strategyName := cfg.Strategy
	if strategyName == "" {
		strategyName = memory.StrategyNone
	}

	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("memory/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first AddTurn.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memory/postgres: ping: %w", err)
	}

	if err := applyMigrations(pingCtx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Build the strategy executor. Persistence rides on deps.State
	// (typically the Postgres StateStore), giving truncation +
	// rolling_summary durable, restart-surviving state.
	exec, err := strategy.New(strategyName, strategy.Deps{
		State:              deps.State,
		Bus:                deps.Bus,
		Summarizer:         deps.Summarizer,
		BudgetTokens:       cfg.BudgetTokens,
		RecoveryBacklogMax: cfg.RecoveryBacklogMax,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &driver{
		strategy: strategyName,
		db:       db,
		bus:      deps.Bus,
		exec:     exec,
	}, nil
}

func init() {
	memory.Register(driverName, New)
}

// driver is the Postgres-backed `memory.MemoryStore` implementation.
//
// Fields are immutable after construction except for the atomic
// `closed` flag and the internally-synchronised executor (D-025).
type driver struct {
	strategy memory.Strategy
	db       *sql.DB
	bus      events.EventBus
	exec     strategy.StrategyExecutor

	mu     sync.Mutex
	closed atomic.Bool
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// AddTurn implements memory.MemoryStore.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	return d.exec.AddTurn(ctx, id, turn)
}

// GetLLMContext implements memory.MemoryStore.
func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return d.exec.GetLLMContext(ctx, id)
}

// EstimateTokens implements memory.MemoryStore.
func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return d.exec.EstimateTokens(ctx, id)
}

// Flush implements memory.MemoryStore.
func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}
	return d.exec.Flush(ctx, id)
}

// Health implements memory.MemoryStore.
func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return d.exec.Health(ctx, id)
}

// Snapshot implements memory.MemoryStore.
func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}
	return d.exec.Snapshot(ctx, id)
}

// Restore implements memory.MemoryStore.
func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}
	return d.exec.Restore(ctx, id, snap)
}

// Close implements memory.MemoryStore. Idempotent. Flips the atomic
// flag BEFORE tearing down the executor + closing `db` so subsequent
// calls fast-fail with `ErrStoreClosed`. Joins the strategy
// executor's per-strategy resources (the rolling_summary recovery
// loop goroutine) so the goroutine baseline is restored (AC-9).
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	execErr := d.exec.Close(ctx)
	dbErr := d.db.Close()
	if dbErr != nil {
		dbErr = fmt.Errorf("memory/postgres: db.Close: %w", dbErr)
	}
	if execErr != nil {
		execErr = fmt.Errorf("memory/postgres: executor close: %w", execErr)
	}
	return errors.Join(dbErr, execErr)
}
