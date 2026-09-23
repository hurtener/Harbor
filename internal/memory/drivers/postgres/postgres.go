// Package postgres provides the postgres driver for cumulative session-memory access.
// The injected StateStore remains the authoritative memory owner.
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
	"github.com/hurtener/Harbor/internal/persistence/postgrespool"
)

// driverName is the name under which this driver self-registers with
// `memory.Register`.
const driverName = "postgres"

// pgxDriverName is the database/sql driver name registered by the
// pgx stdlib adapter.
const pgxDriverName = "pgx"

// Connection-pool defaults. Documented in the phase plan; tuning
// lives in a future config knob, not here. Values mirror the
// StateStore + ArtifactStore drivers for consistency.
const (
	defaultMaxOpenConns    = postgrespool.DefaultMaxOpenConns
	defaultMaxIdleConns    = postgrespool.DefaultMaxIdleConns
	defaultConnMaxLifetime = postgrespool.DefaultConnMaxLifetime
	defaultConnMaxIdleTime = postgrespool.DefaultConnMaxIdleTime
)

// New constructs a Postgres-backed `memory.MemoryStore` against
// `cfg.DSN`. Production callers go through `memory.Open`; tests may
// call `New` directly to skip the registry.
//
// Cumulative memory uses the injected StateStore owner. This adapter has no
// summary engine, private transcript or recovery goroutine. Bus and State are
// mandatory; note writes additionally require the execution redactor.
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
	// Reject removed strategies before sql.Open starts its connection opener.
	if err := memory.ValidateStrategy(cfg.Strategy); err != nil {
		return nil, err
	}

	db, err := sql.Open(pgxDriverName, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("memory/postgres: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(defaultMaxOpenConns)
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)
	db.SetConnMaxIdleTime(defaultConnMaxIdleTime)
	return newWithDB(cfg, deps, db, true)
}

// NewWithDB constructs a memory store over a runtime-owned PostgreSQL pool.
// The returned store borrows db; the runtime pool manager closes it.
func NewWithDB(cfg memory.ConfigSnapshot, deps memory.Deps, db *sql.DB) (memory.MemoryStore, error) {
	if db == nil {
		return nil, errors.New("memory/postgres: injected db is required")
	}
	return newWithDB(cfg, deps, db, false)
}

func newWithDB(cfg memory.ConfigSnapshot, deps memory.Deps, db *sql.DB, ownsDB bool) (memory.MemoryStore, error) {
	if deps.Bus == nil {
		return nil, fmt.Errorf("memory/postgres: deps.Bus is required")
	}
	if deps.State == nil {
		return nil, fmt.Errorf("memory/postgres: deps.State is required (strategy executor persists through it)")
	}
	if cfg.DSN == "" {
		return nil, errors.New("memory/postgres: cfg.DSN is required")
	}
	if err := memory.ValidateStrategy(cfg.Strategy); err != nil {
		return nil, err
	}

	// Probe the connection eagerly. A misconfigured DSN should fail
	// loudly at boot, not on the first memory operation.
	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		if ownsDB {
			_ = db.Close()
		}
		return nil, fmt.Errorf("memory/postgres: ping: %w", err)
	}

	if err := runMigrations(pingCtx, db, cfg.DSN, cfg.MigrationMode); err != nil {
		if ownsDB {
			_ = db.Close()
		}
		return nil, err
	}

	return &driver{
		access: memory.NewAccess(cfg, deps),
		db:     db,
		ownsDB: ownsDB,
		bus:    deps.Bus,
	}, nil
}

func init() {
	memory.Register(driverName, New)
}

// driver is the Postgres-backed `memory.MemoryStore` implementation.
//
// Fields are immutable after construction except for the atomic
// `closed` flag and the internally-synchronised executor.
type driver struct {
	access *memory.Access
	db     *sql.DB
	ownsDB bool
	bus    events.EventBus

	mu     sync.Mutex
	closed atomic.Bool
}

// Inspect implements memory.MemoryStore using the execution-memory owner.
func (d *driver) Inspect(ctx context.Context, id identity.Quadruple) (memory.Inspection, error) {
	if d.closed.Load() {
		return memory.Inspection{}, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return memory.Inspection{}, memory.EmitIdentityRejected(ctx, d.bus, id, "Inspect")
	}
	return d.access.Inspect(ctx, id)
}

// Put implements memory.MemoryStore and returns a committed item identity.
func (d *driver) Put(ctx context.Context, id identity.Quadruple, turn memory.ConversationTurn) (string, error) {
	if d.closed.Load() {
		return "", memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return "", memory.EmitIdentityRejected(ctx, d.bus, id, "Put")
	}
	return d.access.Put(ctx, id, turn)
}

// Delete implements memory.MemoryStore through its conditional owner mutation.
func (d *driver) Delete(ctx context.Context, id identity.Quadruple, key string) (int, error) {
	if d.closed.Load() {
		return 0, memory.ErrStoreClosed
	}
	if memory.ValidateIdentity(id) != nil {
		return 0, memory.EmitIdentityRejected(ctx, d.bus, id, "Delete")
	}
	return d.access.Delete(ctx, id, key)
}

// Compile-time assertion that *driver satisfies memory.MemoryStore.
var _ memory.MemoryStore = (*driver)(nil)

// Close implements memory.MemoryStore. It rejects new operations before
// closing an owned SQL pool. Borrowed pools and the shared StateStore remain
// owned by the runtime. Repeated closes are harmless.
func (d *driver) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed.CompareAndSwap(false, true) {
		return nil
	}
	var dbErr error
	if d.ownsDB {
		dbErr = d.db.Close()
	}
	if dbErr != nil {
		dbErr = fmt.Errorf("memory/postgres: db.Close: %w", dbErr)
	}

	return dbErr
}
