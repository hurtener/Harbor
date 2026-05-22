// Package postgres is Harbor's Postgres-backed `memory.MemoryStore`
// driver (Phase 25). It is the third leg of the memory persistence
// triad (in-memory, SQLite, Postgres) defined by RFC §6.6 + §9.
//
// The driver uses `pgx/v5/stdlib` so the rest of Harbor sees a
// `database/sql.DB`. Parametric queries everywhere; no string
// concatenation into SQL (AGENTS.md §9). Advisory locks serialise
// the migration runner so multi-replica boots are race-free.
//
// Memory state lives in its OWN `memory_state` table — the Postgres
// memory driver does NOT piggyback on the Postgres StateStore
// driver's `state_records` table. The injected `state.StateStore`
// dep is accepted to satisfy the shared `memory.Deps` contract but
// is unused; the `events.EventBus` dep IS used (for the fail-closed
// identity-rejection emit path).
//
// Internal model:
//
//   - One row per `(tenant_id, user_id, session_id, run_id, kind)`.
//     `run_id` may be empty (session-scoped); the column is NOT NULL
//     but accepts the empty string.
//   - `bytes` is BYTEA — opaque JSON-serialised `memory.Record`
//     envelope (see `internal/memory/wire.go`).
//   - `strategy` is denormalised onto its own column for grep-ability;
//     the same value lives inside `bytes`.
//   - `Close(ctx)` flips an atomic flag BEFORE `db.Close()` so
//     subsequent calls fast-fail with `ErrStoreClosed` even while
//     in-flight queries are draining.
//
// Per AGENTS.md §5 (D-025), the driver is safe for concurrent reuse
// across N goroutines.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // register the "pgx" database/sql driver

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/state"
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
// Strategy unsupported at Phase 23/25 (anything other than
// `StrategyNone` or empty-equivalent) returns
// `ErrStrategyNotImplemented`. Phase 24 widens the supported set;
// the Postgres driver will inherit that widening automatically
// through the shared conformance suite.
//
// `deps.Bus` is required. `deps.State` is accepted but unused.
func New(cfg memory.ConfigSnapshot, deps memory.Deps) (memory.MemoryStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/postgres: deps.Bus is required")
	}
	if cfg.DSN == "" {
		return nil, errors.New("memory/postgres: cfg.DSN is required")
	}

	strategy := cfg.Strategy
	if strategy == "" {
		strategy = memory.StrategyNone
	}
	if strategy != memory.StrategyNone {
		return nil, fmt.Errorf("%w: %q (Phase 25 supports %q only; Phase 24 will widen)",
			memory.ErrStrategyNotImplemented, strategy, memory.StrategyNone)
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

	return &driver{
		strategy: strategy,
		db:       db,
		bus:      deps.Bus,
		state:    deps.State, // accepted-but-unused; held to keep the dep visible
	}, nil
}

func init() {
	memory.Register(driverName, New)
}

// driver is the Postgres-backed `memory.MemoryStore` implementation.
//
// Fields are immutable after construction except for the atomic
// `closed` flag (D-025).
type driver struct {
	strategy memory.Strategy
	db       *sql.DB
	bus      events.EventBus
	state    state.StateStore // accepted but unused

	mu     sync.Mutex
	closed atomic.Bool
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// AddTurn implements memory.MemoryStore. Strategy=none no-op.
func (d *driver) AddTurn(ctx context.Context, id identity.Quadruple, _ memory.ConversationTurn) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "AddTurn")
	}
	return nil
}

// GetLLMContext implements memory.MemoryStore. Strategy=none returns
// an empty patch.
func (d *driver) GetLLMContext(ctx context.Context, id identity.Quadruple) (memory.LLMContextPatch, error) {
	if d.closed.Load() {
		return memory.LLMContextPatch{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.LLMContextPatch{}, memory.EmitIdentityRejected(ctx, d.bus, id, "GetLLMContext")
	}
	return memory.LLMContextPatch{Strategy: d.strategy}, nil
}

// EstimateTokens implements memory.MemoryStore. Strategy=none
// returns 0.
func (d *driver) EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "EstimateTokens")
	}
	return 0, nil
}

// Flush implements memory.MemoryStore. Idempotent delete-by-slot.
func (d *driver) Flush(ctx context.Context, id identity.Quadruple) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Flush")
	}
	const del = `
		DELETE FROM memory_state
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4 AND kind = $5
	`
	if _, err := d.db.ExecContext(ctx, del,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState); err != nil {
		return d.translateErr(err, "memory/postgres: Flush delete")
	}
	return nil
}

// Health implements memory.MemoryStore. Strategy=none always
// reports `HealthHealthy`.
func (d *driver) Health(ctx context.Context, id identity.Quadruple) (memory.Health, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Health")
	}
	return memory.HealthHealthy, nil
}

// Snapshot implements memory.MemoryStore. Missing row returns an
// empty Strategy=none snapshot; present row returns its stored bytes
// verbatim (cross-driver byte-stability).
func (d *driver) Snapshot(ctx context.Context, id identity.Quadruple) (memory.Snapshot, error) {
	if d.closed.Load() {
		return memory.Snapshot{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Snapshot{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Snapshot")
	}

	const sel = `
		SELECT strategy, bytes
		FROM memory_state
		WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3 AND run_id = $4 AND kind = $5
	`
	row := d.db.QueryRowContext(ctx, sel,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState)

	var strategy string
	var data []byte
	if err := row.Scan(&strategy, &data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return memory.Snapshot{Strategy: d.strategy}, nil
		}
		return memory.Snapshot{}, d.translateErr(err, "memory/postgres: Snapshot load")
	}
	return memory.Snapshot{Strategy: memory.Strategy(strategy), Bytes: data}, nil
}

// Restore implements memory.MemoryStore.
func (d *driver) Restore(ctx context.Context, id identity.Quadruple, snap memory.Snapshot) error {
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.EmitIdentityRejected(ctx, d.bus, id, "Restore")
	}

	if snap.IsEmpty() {
		return d.persistRecord(ctx, id, memory.Record{Strategy: d.strategy})
	}
	if snap.Strategy != d.strategy {
		return fmt.Errorf("%w: snapshot strategy=%q driver strategy=%q",
			memory.ErrInvalidSnapshot, snap.Strategy, d.strategy)
	}
	if len(snap.Bytes) == 0 {
		return d.persistRecord(ctx, id, memory.Record{Strategy: d.strategy})
	}
	if d.strategy == memory.StrategyNone {
		var rec memory.Record
		if err := json.Unmarshal(snap.Bytes, &rec); err != nil {
			return fmt.Errorf("%w: %w", memory.ErrInvalidSnapshot, err)
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
	return fmt.Errorf("%w: unsupported snapshot for strategy %q",
		memory.ErrInvalidSnapshot, d.strategy)
}

// persistRecord marshals the typed record and UPSERTs it.
func (d *driver) persistRecord(ctx context.Context, id identity.Quadruple, rec memory.Record) error {
	bytes, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("memory/postgres: marshal record: %w", err)
	}
	const upsert = `
		INSERT INTO memory_state
			(tenant_id, user_id, session_id, run_id, kind, strategy, bytes, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, user_id, session_id, run_id, kind) DO UPDATE
			SET strategy   = EXCLUDED.strategy,
			    bytes      = EXCLUDED.bytes,
			    updated_at = EXCLUDED.updated_at
	`
	if _, err := d.db.ExecContext(ctx, upsert,
		id.TenantID, id.UserID, id.SessionID, id.RunID, memory.KindMemoryState,
		string(rec.Strategy), bytes, time.Now().UTC(),
	); err != nil {
		return d.translateErr(err, "memory/postgres: persist record")
	}
	return nil
}

// Close implements memory.MemoryStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("memory/postgres: db.Close: %w", err)
	}
	return nil
}

// translateErr maps low-level driver errors to Harbor sentinels at the
// boundary. Callers compare via errors.Is against memory.ErrXxx; raw
// pgx errors must never leak.
func (d *driver) translateErr(err error, ctxMsg string) error {
	if err == nil {
		return nil
	}
	if d.closed.Load() {
		return memory.ErrStoreClosed
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("%s: %w", ctxMsg, memory.ErrStoreClosed)
	}
	return fmt.Errorf("%s: %w", ctxMsg, err)
}
